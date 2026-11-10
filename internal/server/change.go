package server

// The machinery of a change: a command in a new snapshot that is kept or
// discarded, as transactional-update does it.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sdrahn/mcp-tukit/internal/mcp"
	"github.com/sdrahn/mcp-tukit/internal/tukit"
)

// outcome decides from a command's end whether its snapshot is kept.
type outcome int

const (
	keep outcome = iota
	discardNoChanges
	discardFailed
)

// zypperOutcome: the codes transactional-update accepts (0, 102 reboot
// needed, 103 zypper itself was updated, 106 some repositories were
// skipped), and "Nothing to do." as no change.
func zypperOutcome(r tukit.CallResult) outcome {
	switch r.ExitCode {
	case 0, 102, 103, 106:
	default:
		return discardFailed
	}
	if strings.Contains(r.Output, "Nothing to do.") {
		return discardNoChanges
	}
	return keep
}

func exitOutcome(r tukit.CallResult) outcome {
	if r.ExitCode != 0 {
		return discardFailed
	}
	return keep
}

// change is the result of a change.
type change struct {
	Status          string   `json:"status"`
	Snapshot        string   `json:"snapshot,omitempty"`
	Base            string   `json:"base"`
	Command         []string `json:"command"`
	ExitCode        *int     `json:"exit_code,omitempty"`
	Output          string   `json:"output,omitempty"`
	OutputTruncated bool     `json:"output_truncated,omitempty"`
	RebootRequired  bool     `json:"reboot_required"`
	Error           string   `json:"error,omitempty"`
}

// transact runs argv in a new snapshot from base and keeps or discards
// the snapshot as decide says: what transactional-update does around a
// command. Once begun it goes on when the call is cancelled, or ends
// first: a change is finished (the snapshot closed or discarded) rather
// than left half-done, within the configured timeout.
func (s *Server) transact(ctx context.Context, base, what string, argv []string, decide func(tukit.CallResult) outcome) map[string]any {
	c, err := exclusive(s, ctx, base, what, func(work context.Context, opened chan<- string) change {
		return s.run(work, base, what, argv, decide, opened)
	}, func(snap string) change {
		// Not answered (cancelled), but the change goes on to its end.
		return change{Status: "running", Snapshot: snap, Base: base, Command: argv}
	})
	if err != nil {
		return toolError("%v", err)
	}
	return changeResult(c)
}

// exclusive runs work, which opens a snapshot from base, under the change
// lock, in the background, within the configured timeout, with progress
// on ctx while it runs. A call cancelled while it waits for the lock does
// nothing (errNotStarted). Once work has begun it goes on to its end: if
// ctx ends first, exclusive returns cancelled(snapshot) at once.
func exclusive[T any](s *Server, ctx context.Context, base, what string, work func(ctx context.Context, opened chan<- string) T, cancelled func(snap string) T) (T, error) {
	if err := s.lockChange(ctx); err != nil {
		var zero T
		return zero, err
	}
	done := make(chan T, 1)
	opened := make(chan string, 1)
	go func() {
		defer s.change.Unlock()
		w, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.Timeout)
		defer cancel()
		done <- work(w, opened)
	}()
	tick := time.NewTicker(s.interval)
	defer tick.Stop()
	snap := ""
	start := time.Now()
	for {
		select {
		case r := <-done:
			return r, nil
		case snap = <-opened:
			mcp.Progress(ctx, fmt.Sprintf("snapshot %s opened from %s; running %s", snap, base, shorten(printable(what), maxDescription)))
		case <-tick.C:
			mcp.Progress(ctx, fmt.Sprintf("running %s in snapshot %s (%s)", shorten(printable(what), maxDescription), snap, time.Since(start).Round(time.Second)))
		case <-ctx.Done():
			return cancelled(snap), nil
		}
	}
}

func (s *Server) run(ctx context.Context, base, what string, argv []string, decide func(tukit.CallResult) outcome, opened chan<- string) change {
	c := change{Base: base, Command: argv}
	snap, err := s.tk.Open(ctx, base, description(what))
	if err != nil {
		c.Status, c.Error = "failed", fmt.Sprintf("opening a snapshot from %s: %v", base, err)
		s.logf("%s: %s", what, c.Error)
		return c
	}
	c.Snapshot = snap
	opened <- snap
	s.logf("%s: snapshot %s opened from %s: %s", what, snap, base, tukit.Quote(argv))
	r, err := s.tk.Call(ctx, snap, argv)
	if err != nil {
		if ctx.Err() != nil {
			// The command still runs in tukitd, which holds the snapshot;
			// it can be neither closed nor discarded now.
			c.Status = "running"
			c.Error = fmt.Sprintf("the command did not end within %s; snapshot %s stays open (not the default) and can be deleted once it ends", s.cfg.Timeout, snap)
		} else {
			c.Status, c.Error = "failed", fmt.Sprintf("running the command: %v", err)
			s.discard(snap, what)
		}
		s.logf("%s: snapshot %s: %s", what, snap, c.Error)
		return c
	}
	code := r.ExitCode
	c.ExitCode = &code
	c.Output, c.OutputTruncated = clip(r.Output, s.cfg.MaxOutput)
	if r.Failed {
		c.Status, c.Error = "failed", "tukit: "+firstLine(r.Output)
		s.discard(snap, what)
		s.logf("%s: snapshot %s: %s", what, snap, c.Error)
		return c
	}
	// Close and Abort are not cut short: their context is only the
	// fallback against a hanging tukitd.
	end, cancel := opContext(context.WithoutCancel(ctx))
	defer cancel()
	switch decide(r) {
	case discardNoChanges:
		c.Status = "no_changes"
		s.discard(snap, what)
	case discardFailed:
		c.Status, c.Error = "failed", fmt.Sprintf("the command exited with %d; snapshot %s was discarded", code, snap)
		s.discard(snap, what)
	default:
		if err := s.tk.Close(end, snap); err != nil {
			c.Status, c.Error = "failed", fmt.Sprintf("closing snapshot %s: %v", snap, err)
			s.discard(snap, what)
		} else {
			c.Status, c.RebootRequired = "applied", true
		}
	}
	s.logf("%s: snapshot %s: %s (exit %d)", what, snap, c.Status, code)
	return c
}

func (s *Server) discard(snap, what string) {
	ctx, cancel := opContext(context.Background())
	defer cancel()
	if err := s.tk.Abort(ctx, snap); err != nil {
		s.logf("%s: discarding snapshot %s: %v", what, snap, err)
	}
}

func changeResult(c change) map[string]any {
	var b strings.Builder
	switch c.Status {
	case "applied":
		fmt.Fprintf(&b, "Applied in snapshot %s (from %s), now the default: it takes effect at the next reboot.\n", c.Snapshot, c.Base)
	case "no_changes":
		fmt.Fprintf(&b, "Nothing to do; snapshot %s was discarded.\n", c.Snapshot)
	case "running":
		if c.Error != "" {
			b.WriteString(c.Error + "\n")
		} else {
			fmt.Fprintf(&b, "Still running in snapshot %s.\n", c.Snapshot)
		}
	default:
		b.WriteString("Failed: " + c.Error + "\n")
	}
	if c.Output != "" {
		b.WriteString("\n")
		if c.OutputTruncated {
			b.WriteString("[output shortened]\n")
		}
		b.WriteString(c.Output)
		if !strings.HasSuffix(c.Output, "\n") {
			b.WriteString("\n")
		}
	}
	return result(b.String(), c, c.Status == "failed" || c.Status == "running")
}

// clip keeps the start and the end of out within max bytes: the end has
// zypper's summary and errors.
func clip(out string, max int) (string, bool) {
	if len(out) <= max {
		return out, false
	}
	head := max / 8
	tail := max - head
	h, t := out[:head], out[len(out)-tail:]
	return strings.ToValidUTF8(h, "") + "\n[…]\n" + strings.ToValidUTF8(t, ""), true
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
