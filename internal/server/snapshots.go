package server

// Snapshots: status, listing, rollback, deletion and cleanup.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/sdrahn/mcp-tukit/internal/tukit"
)

type snapshot struct {
	Number      string `json:"number"`
	Date        string `json:"date,omitempty"`
	Description string `json:"description,omitempty"`
	Active      bool   `json:"active"`
	Default     bool   `json:"default"`
}

func yes(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "true", "1", "*":
		return true
	}
	return false
}

func (s *Server) snapshots(ctx context.Context) ([]snapshot, error) {
	list, err := s.tk.List(ctx, tukit.Columns)
	if err != nil {
		return nil, err
	}
	var out []snapshot
	for _, m := range list {
		// Snapshot 0 is snapper's "current", the live file system.
		if m["number"] == "" || m["number"] == "0" {
			continue
		}
		out = append(out, snapshot{Number: m["number"], Date: m["date"], Description: m["description"],
			Active: yes(m["active"]), Default: yes(m["default"])})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, errA := strconv.Atoi(out[i].Number)
		b, errB := strconv.Atoi(out[j].Number)
		if errA != nil || errB != nil {
			return out[i].Number < out[j].Number
		}
		return a < b
	})
	return out, nil
}

func (s *Server) getStatus(ctx context.Context, args json.RawMessage) map[string]any {
	if err := decode(args, &struct{}{}); err != nil {
		return toolError("%v", err)
	}
	ctx, cancel := opContext(ctx)
	defer cancel()
	snaps, err := s.snapshots(ctx)
	if err != nil {
		return toolError("listing snapshots: %v", err)
	}
	st := map[string]any{"snapshots": len(snaps)}
	var active, def string
	for _, sn := range snaps {
		if sn.Active {
			active = sn.Number
		}
		if sn.Default {
			def = sn.Number
			st["next"] = sn
		}
	}
	st["active"], st["default"] = active, def
	pending := def != "" && active != "" && def != active
	st["reboot_pending"] = pending
	var text string
	switch {
	case active == "" && def == "":
		text = fmt.Sprintf("%d snapshots; tukit names neither an active nor a default snapshot", len(snaps))
	case pending:
		text = fmt.Sprintf("Running snapshot %s; snapshot %s is the default: a reboot activates it.", active, def)
	default:
		text = fmt.Sprintf("Running snapshot %s, which is also the default: no reboot pending.", active)
	}
	return result(text, st, false)
}

func (s *Server) listSnapshots(ctx context.Context, args json.RawMessage) map[string]any {
	var a struct {
		Limit int `json:"limit"`
	}
	if err := decode(args, &a); err != nil {
		return toolError("%v", err)
	}
	ctx, cancel := opContext(ctx)
	defer cancel()
	snaps, err := s.snapshots(ctx)
	if err != nil {
		return toolError("listing snapshots: %v", err)
	}
	total := len(snaps)
	if a.Limit > 0 && a.Limit < len(snaps) {
		snaps = snaps[len(snaps)-a.Limit:]
	}
	var b strings.Builder
	for _, sn := range snaps {
		mark := ""
		switch {
		case sn.Active && sn.Default:
			mark = " [active, default]"
		case sn.Active:
			mark = " [active]"
		case sn.Default:
			mark = " [default: boots next]"
		}
		fmt.Fprintf(&b, "%s\t%s\t%s%s\n", sn.Number, sn.Date, sn.Description, mark)
	}
	if b.Len() == 0 {
		b.WriteString("no snapshots\n")
	}
	if snaps == nil {
		snaps = []snapshot{}
	}
	return result(b.String(), map[string]any{"snapshots": snaps, "total": total}, false)
}

func (s *Server) rollback(ctx context.Context, args json.RawMessage) map[string]any {
	var a struct {
		Snapshot string `json:"snapshot"`
	}
	if err := decode(args, &a); err != nil {
		return toolError("%v", err)
	}
	if !tukit.ValidSnapshot(a.Snapshot, false) {
		return toolError("snapshot %q: must be a snapshot number", a.Snapshot)
	}
	s.change.Lock()
	defer s.change.Unlock()
	ctx, cancel := opContext(context.WithoutCancel(ctx))
	defer cancel()
	def, err := s.tk.RollbackTo(ctx, a.Snapshot)
	if err != nil {
		s.logf("rollback to %s: %v", a.Snapshot, err)
		return toolError("rollback to %s: %v", a.Snapshot, err)
	}
	s.logf("rollback to %s: default is %s", a.Snapshot, def)
	text := fmt.Sprintf("Snapshot %s is the default: the system boots it at the next reboot.", def)
	if def != a.Snapshot {
		text = fmt.Sprintf("Rolled back to snapshot %s: its copy, snapshot %s, is the default and boots at the next reboot.", a.Snapshot, def)
	}
	return result(text, map[string]any{"snapshot": a.Snapshot, "default": def, "reboot_required": true}, false)
}

func (s *Server) deleteSnapshot(ctx context.Context, args json.RawMessage) map[string]any {
	var a struct {
		Snapshot string `json:"snapshot"`
	}
	if err := decode(args, &a); err != nil {
		return toolError("%v", err)
	}
	if !tukit.ValidSnapshot(a.Snapshot, false) || a.Snapshot == "0" {
		return toolError("snapshot %q: must be a snapshot number", a.Snapshot)
	}
	s.change.Lock()
	defer s.change.Unlock()
	ctx, cancel := opContext(context.WithoutCancel(ctx))
	defer cancel()
	snaps, err := s.snapshots(ctx)
	if err != nil {
		return toolError("listing snapshots: %v", err)
	}
	found := false
	for _, sn := range snaps {
		if sn.Number != a.Snapshot {
			continue
		}
		found = true
		if sn.Active {
			return toolError("snapshot %s is the running system and cannot be deleted", a.Snapshot)
		}
		if sn.Default {
			return toolError("snapshot %s is the default (booted next) and cannot be deleted; roll back to another snapshot first", a.Snapshot)
		}
	}
	if !found {
		return result(fmt.Sprintf("There is no snapshot %s.", a.Snapshot), map[string]any{"done": true}, false)
	}
	if err := s.tk.Delete(ctx, a.Snapshot); err != nil {
		s.logf("delete snapshot %s: %v", a.Snapshot, err)
		return toolError("deleting snapshot %s: %v", a.Snapshot, err)
	}
	s.logf("snapshot %s deleted", a.Snapshot)
	return result(fmt.Sprintf("Snapshot %s deleted.", a.Snapshot), map[string]any{"done": true}, false)
}

func (s *Server) cleanup(ctx context.Context, args json.RawMessage) map[string]any {
	if err := decode(args, &struct{}{}); err != nil {
		return toolError("%v", err)
	}
	s.change.Lock()
	defer s.change.Unlock()
	ctx, cancel := opContext(context.WithoutCancel(ctx))
	defer cancel()
	if err := s.tk.Cleanup(ctx); err != nil {
		s.logf("cleanup: %v", err)
		var te *tukit.Error
		if errors.As(err, &te) && te.Name == "org.freedesktop.DBus.Error.UnknownMethod" {
			return toolError("cleanup: this tukitd has no Cleanup (added in transactional-update 6.2); snapper's own cleanup timer still removes snapshots by its rules")
		}
		return toolError("cleanup: %v", err)
	}
	s.logf("cleanup done")
	return result("Snapshots cleaned up.", map[string]any{"done": true}, false)
}

// activeDefault returns the running and the default snapshot.
func (s *Server) activeDefault(ctx context.Context) (active, def string, err error) {
	snaps, err := s.snapshots(ctx)
	if err != nil {
		return "", "", err
	}
	for _, sn := range snaps {
		if sn.Active {
			active = sn.Number
		}
		if sn.Default {
			def = sn.Number
		}
	}
	return active, def, nil
}
