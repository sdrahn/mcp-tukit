package server

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"

	"github.com/sdrahn/mcp-tukit/internal/tukit"
)

// Looking without changing: zypper refresh and list-updates run in a new
// snapshot that is discarded afterwards, as transactional-update runs
// zypper refresh. The running system is read-only, and the server runs
// no program itself; /var/cache, with zypper's repository metadata, is
// the running system's in the snapshot too, so a refresh is kept.

// look is how a command in a snapshot that is discarded afterwards
// ended.
type look struct {
	Snapshot string
	ExitCode int
	Output   string
	// Error tells why the command did not run, or not to its end.
	Error string
	// Running is set when the command did not end within the timeout:
	// the snapshot stays open.
	Running bool
}

// peek runs argv in a new snapshot from base and discards the snapshot.
func (s *Server) peek(ctx context.Context, base, what string, argv []string, opened chan<- string) look {
	snap, err := s.tk.Open(ctx, base, "MCP: "+what)
	if err != nil {
		l := look{Error: fmt.Sprintf("opening a snapshot from %s: %v", base, err)}
		s.logf("%s: %s", what, l.Error)
		return l
	}
	opened <- snap
	l := look{Snapshot: snap}
	s.logf("%s: snapshot %s opened from %s: %s", what, snap, base, tukit.Quote(argv))
	r, err := s.tk.Call(ctx, snap, argv)
	switch {
	case err != nil && ctx.Err() != nil:
		// The command still runs in tukitd, which holds the snapshot.
		l.Running = true
		l.Error = fmt.Sprintf("the command did not end within %s; snapshot %s stays open (not the default) and can be deleted once it ends", s.cfg.Timeout, snap)
		s.logf("%s: snapshot %s: %s", what, snap, l.Error)
		return l
	case err != nil:
		l.Error = fmt.Sprintf("running the command: %v", err)
	case r.Failed:
		l.Error = "tukit: " + firstLine(r.Output)
	default:
		l.ExitCode, l.Output = r.ExitCode, r.Output
	}
	s.discard(snap, what)
	if l.Error != "" {
		s.logf("%s: snapshot %s discarded: %s", what, snap, l.Error)
	} else {
		s.logf("%s: snapshot %s discarded (exit %d)", what, snap, l.ExitCode)
	}
	return l
}

// look runs argv as peek does, under the change lock.
func (s *Server) look(ctx context.Context, base, what string, argv []string) look {
	return exclusive(s, ctx, base, what, func(work context.Context, opened chan<- string) look {
		return s.peek(work, base, what, argv, opened)
	}, func(snap string) look {
		return look{Snapshot: snap, Running: true, Error: "the call ended first; the command goes on in snapshot " + snap + ", which is discarded when it ends"}
	})
}

// refresh is the result of refresh_repositories.
type refresh struct {
	Status          string   `json:"status"`
	Base            string   `json:"base"`
	Command         []string `json:"command"`
	ExitCode        *int     `json:"exit_code,omitempty"`
	Output          string   `json:"output,omitempty"`
	OutputTruncated bool     `json:"output_truncated,omitempty"`
	Error           string   `json:"error,omitempty"`
}

var refreshSchema = map[string]any{"type": "object", "properties": map[string]any{
	"status":           map[string]any{"type": "string", "enum": []string{"refreshed", "partial", "failed", "running"}, "description": "refreshed: all repositories; partial: some could not be refreshed (zypper exit 106); failed; running: the refresh goes on (the call ended first)"},
	"base":             map[string]any{"type": "string"},
	"command":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	"exit_code":        map[string]any{"type": "integer"},
	"output":           map[string]any{"type": "string"},
	"output_truncated": map[string]any{"type": "boolean"},
	"error":            map[string]any{"type": "string"},
}, "required": []string{"status"}}

func (s *Server) refreshRepositories(ctx context.Context, args json.RawMessage) map[string]any {
	var a struct {
		Force bool   `json:"force"`
		Base  string `json:"base"`
	}
	if err := decode(args, &a); err != nil {
		return toolError("%v", err)
	}
	base, err := s.base(a.Base)
	if err != nil {
		return toolError("%v", err)
	}
	argv := s.zypper("refresh")
	if a.Force {
		argv = append(argv, "--force")
	}
	l := s.look(ctx, base, "refresh", argv)
	r := refresh{Base: base, Command: argv, Error: l.Error}
	var b strings.Builder
	switch {
	case l.Running:
		r.Status = "running"
		b.WriteString(l.Error + "\n")
	case l.Error != "":
		r.Status = "failed"
		b.WriteString("Failed: " + l.Error + "\n")
	default:
		code := l.ExitCode
		r.ExitCode = &code
		r.Output, r.OutputTruncated = clip(l.Output, s.cfg.MaxOutput)
		switch code {
		case 0:
			r.Status = "refreshed"
			b.WriteString("The repositories are refreshed.\n")
		case 106:
			r.Status = "partial"
			b.WriteString("Some repositories could not be refreshed (zypper exit 106); the others are.\n")
		default:
			r.Status = "failed"
			r.Error = fmt.Sprintf("zypper refresh exited with %d", code)
			b.WriteString("Failed: " + r.Error + "\n")
		}
		if r.Output != "" {
			b.WriteString("\n" + r.Output)
		}
	}
	return result(b.String(), r, r.Status == "failed" || r.Status == "running")
}

// update is one available update, from zypper's XML.
type update struct {
	Kind             string `json:"kind"`
	Name             string `json:"name"`
	Version          string `json:"version"`
	InstalledVersion string `json:"installed_version,omitempty"`
	Arch             string `json:"arch,omitempty"`
	Repository       string `json:"repository,omitempty"`
	Summary          string `json:"summary,omitempty"`
	// Patches only.
	Category        string `json:"category,omitempty"`
	Severity        string `json:"severity,omitempty"`
	Status          string `json:"status,omitempty"`
	RebootSuggested bool   `json:"reboot_suggested,omitempty"`
	Interactive     bool   `json:"interactive,omitempty"`
}

// updateList is the result of list_updates.
type updateList struct {
	Kind    string   `json:"kind"`
	Base    string   `json:"base"`
	Count   int      `json:"count"`
	Updates []update `json:"updates"`
	// Blocked are patches a package lock keeps back.
	Blocked []update `json:"blocked,omitempty"`
	// Messages are zypper's warnings and errors, such as repositories
	// it could not refresh.
	Messages []string `json:"messages,omitempty"`
}

var updateSchema = map[string]any{"type": "object", "properties": map[string]any{
	"kind":              map[string]any{"type": "string"},
	"name":              map[string]any{"type": "string"},
	"version":           map[string]any{"type": "string", "description": "the version available"},
	"installed_version": map[string]any{"type": "string", "description": "the version installed (packages)"},
	"arch":              map[string]any{"type": "string"},
	"repository":        map[string]any{"type": "string", "description": "the repository's alias"},
	"summary":           map[string]any{"type": "string"},
	"category":          map[string]any{"type": "string", "description": "patches: security, recommended, optional, feature, ..."},
	"severity":          map[string]any{"type": "string", "description": "patches: critical, important, moderate, low, unspecified"},
	"status":            map[string]any{"type": "string", "description": "patches: needed, applied, ..."},
	"reboot_suggested":  map[string]any{"type": "boolean"},
	"interactive":       map[string]any{"type": "boolean", "description": "patches: needs confirmation (a license or a message)"},
}, "required": []string{"name", "version"}}

var updateListSchema = map[string]any{"type": "object", "properties": map[string]any{
	"kind":     map[string]any{"type": "string", "enum": []string{"package", "patch"}},
	"base":     map[string]any{"type": "string"},
	"count":    map[string]any{"type": "integer"},
	"updates":  map[string]any{"type": "array", "items": updateSchema},
	"blocked":  map[string]any{"type": "array", "items": updateSchema, "description": "patches kept back by a package lock"},
	"messages": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "zypper's warnings and errors"},
}, "required": []string{"kind", "count", "updates"}}

// maxListed bounds the updates named in the text answer; all are in the
// structured content.
const maxListed = 200

func (s *Server) listUpdates(ctx context.Context, args json.RawMessage) map[string]any {
	var a struct {
		Kind string `json:"kind"`
		Base string `json:"base"`
	}
	if err := decode(args, &a); err != nil {
		return toolError("%v", err)
	}
	base, err := s.base(a.Base)
	if err != nil {
		return toolError("%v", err)
	}
	var argv []string
	switch a.Kind {
	case "", "package":
		a.Kind, argv = "package", s.zypper("--xmlout", "list-updates")
	case "patch":
		argv = s.zypper("--xmlout", "list-patches")
	default:
		return toolError("kind %q: must be package or patch", a.Kind)
	}
	l := s.look(ctx, base, "list "+a.Kind+" updates", argv)
	if l.Error != "" {
		return toolError("%s", l.Error)
	}
	list := updateList{Kind: a.Kind, Base: base}
	list.Updates, list.Blocked, list.Messages, err = parseUpdates(l.Output)
	// zypper exits 100 or 101 from list-patches when patches (security
	// patches) are needed, 106 when it skipped repositories it could not
	// refresh.
	switch {
	case err != nil:
		out, _ := clip(l.Output, s.cfg.MaxOutput)
		return toolError("listing the updates (zypper exit %d): %v\n\n%s", l.ExitCode, err, out)
	case l.ExitCode != 0 && l.ExitCode != 100 && l.ExitCode != 101 && l.ExitCode != 106:
		out, _ := clip(l.Output, s.cfg.MaxOutput)
		return toolError("listing the updates: zypper exited with %d\n\n%s", l.ExitCode, out)
	}
	if list.Updates == nil {
		list.Updates = []update{}
	}
	list.Count = len(list.Updates)
	return result(updatesText(list), list, false)
}

func updatesText(list updateList) string {
	var b strings.Builder
	what := map[string]string{"package": "package updates", "patch": "needed patches"}[list.Kind]
	switch list.Count {
	case 0:
		fmt.Fprintf(&b, "No %s (as the system would be from snapshot %s).\n", what, list.Base)
	default:
		fmt.Fprintf(&b, "%d %s (as the system would be from snapshot %s):\n", list.Count, what, list.Base)
	}
	for i, u := range list.Updates {
		if i == maxListed {
			fmt.Fprintf(&b, "  … and %d more (all are in the structured content)\n", list.Count-maxListed)
			break
		}
		b.WriteString("  " + updateLine(u) + "\n")
	}
	if len(list.Blocked) > 0 {
		fmt.Fprintf(&b, "Kept back by a package lock: %d\n", len(list.Blocked))
		for _, u := range list.Blocked {
			b.WriteString("  " + updateLine(u) + "\n")
		}
	}
	for _, m := range list.Messages {
		b.WriteString("zypper: " + m + "\n")
	}
	return b.String()
}

func updateLine(u update) string {
	var b strings.Builder
	b.WriteString(u.Name)
	switch {
	case u.Kind == "patch":
		fmt.Fprintf(&b, " (%s, %s)", u.Category, u.Severity)
		if u.Summary != "" {
			b.WriteString(": " + u.Summary)
		}
	case u.InstalledVersion != "":
		fmt.Fprintf(&b, " %s → %s", u.InstalledVersion, u.Version)
	default:
		b.WriteString(" " + u.Version)
	}
	if u.Repository != "" {
		b.WriteString(" [" + u.Repository + "]")
	}
	return b.String()
}

// zypper's XML (zypper --xmlout list-updates, list-patches): an
// <update-status> in a <stream> of messages and progress.
type xmlUpdate struct {
	Kind        string `xml:"kind,attr"`
	Name        string `xml:"name,attr"`
	Edition     string `xml:"edition,attr"`
	EditionOld  string `xml:"edition-old,attr"`
	Arch        string `xml:"arch,attr"`
	Status      string `xml:"status,attr"`
	Category    string `xml:"category,attr"`
	Severity    string `xml:"severity,attr"`
	Restart     string `xml:"restart,attr"`
	Interactive string `xml:"interactive,attr"`
	Summary     string `xml:"summary"`
	Source      struct {
		Alias string `xml:"alias,attr"`
	} `xml:"source"`
}

type xmlStream struct {
	Status *struct {
		Updates []xmlUpdate `xml:"update-list>update"`
		Blocked []xmlUpdate `xml:"blocked-update-list>update"`
	} `xml:"update-status"`
	Messages []struct {
		Type string `xml:"type,attr"`
		Text string `xml:",chardata"`
	} `xml:"message"`
}

// parseUpdates reads zypper's XML from out, which may have other output
// (stderr) before it. Messages are zypper's warnings and errors.
func parseUpdates(out string) (updates, blocked []update, messages []string, err error) {
	i := strings.Index(out, "<stream")
	if i < 0 {
		return nil, nil, nil, errors.New("zypper gave no XML")
	}
	var st xmlStream
	if err := xml.NewDecoder(strings.NewReader(out[i:])).Decode(&st); err != nil {
		return nil, nil, nil, fmt.Errorf("zypper's XML: %v", err)
	}
	for _, m := range st.Messages {
		if m.Type == "warning" || m.Type == "error" {
			messages = append(messages, strings.TrimSpace(m.Text))
		}
	}
	if st.Status == nil {
		if len(messages) > 0 {
			return nil, nil, messages, fmt.Errorf("no list from zypper: %s", strings.Join(messages, "; "))
		}
		return nil, nil, messages, errors.New("no list from zypper")
	}
	conv := func(xs []xmlUpdate) []update {
		var us []update
		for _, x := range xs {
			us = append(us, update{Kind: x.Kind, Name: x.Name, Version: x.Edition, InstalledVersion: x.EditionOld,
				Arch: x.Arch, Repository: x.Source.Alias, Summary: strings.TrimSpace(x.Summary),
				Category: x.Category, Severity: x.Severity, Status: x.Status,
				RebootSuggested: x.Restart == "true", Interactive: x.Interactive == "true"})
		}
		return us
	}
	return conv(st.Status.Updates), conv(st.Status.Blocked), messages, nil
}
