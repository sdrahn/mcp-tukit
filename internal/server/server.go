// Package server is the MCP side of mcp-server-tukit: the tools, their
// schemas and what each does with tukit.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sdrahn/mcp-tukit/internal/mcp"
	"github.com/sdrahn/mcp-tukit/internal/tukit"
)

// Name is the server's name in serverInfo.
const Name = "mcp-server-tukit"

// Config is what the command line sets.
type Config struct {
	// Version is reported in serverInfo.
	Version string
	// Zypper is the package manager's path inside the snapshot.
	Zypper string
	// DefaultBase is the base of new snapshots when a call names none.
	DefaultBase string
	// Timeout bounds a change: opening the snapshot, the command, closing.
	Timeout time.Duration
	// AllowCommands offers run_in_snapshot, which runs any command as
	// root in a new snapshot.
	AllowCommands bool
	// MaxOutput bounds the command output in a result.
	MaxOutput int
	// RebootUnit is the systemd unit the reboot tool starts (it runs
	// transactional-update reboot); empty: no reboot tool.
	RebootUnit string
	// RebootConfig are transactional-update's configuration files, in the
	// order it reads them, for the reboot method the result names.
	RebootConfig []string
	// About, if set, opens the instructions.
	About string
	// Log receives a line for every change (the journal, on stderr).
	Log io.Writer
}

// Server answers MCP requests with tukit.
type Server struct {
	cfg Config
	tk  tukit.Backend

	// change serializes the changes of this instance: each one opens a
	// snapshot from the default, and two at once would race for it.
	change sync.Mutex
	// interval between progress notifications while a command runs.
	interval time.Duration
}

// New returns a server using tk.
func New(cfg Config, tk tukit.Backend) *Server {
	if cfg.Zypper == "" {
		cfg.Zypper = "/usr/bin/zypper"
	}
	if cfg.DefaultBase == "" {
		cfg.DefaultBase = "default"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Hour
	}
	if cfg.MaxOutput <= 0 {
		cfg.MaxOutput = 32 << 10
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	return &Server{cfg: cfg, tk: tk, interval: 15 * time.Second}
}

// Handle answers one MCP request.
func (s *Server) Handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(params, &p)
		return map[string]any{
			"protocolVersion": mcp.Negotiate(p.ProtocolVersion),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": Name, "title": "Transactional updates (tukit)", "version": s.cfg.Version},
			"instructions":    s.instructions(),
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.toolList()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &mcp.Error{Code: mcp.CodeInvalidParams, Message: "invalid params"}
		}
		t := s.tool(p.Name)
		if t == nil {
			return nil, &mcp.Error{Code: mcp.CodeInvalidParams, Message: "unknown tool " + strconv.Quote(p.Name)}
		}
		args := p.Arguments
		if len(args) == 0 || string(args) == "null" {
			args = json.RawMessage("{}")
		}
		return t.call(ctx, args), nil
	}
	return nil, &mcp.Error{Code: mcp.CodeMethodNotFound, Message: "method not found"}
}

func (s *Server) instructions() string {
	out := "Changes this transactional system (openSUSE MicroOS, Leap Micro, SL Micro, SLES 16 in immutable mode) " +
		"the way transactional-update does, through tukit: each change runs in a new snapshot of the root file system, " +
		"and the running system stays as it is. The new snapshot becomes the default and takes effect at the next reboot; " +
		"until then the change is not visible in the running system. A change that fails is discarded and leaves no snapshot. " +
		"New snapshots build on the default snapshot (\"" + s.cfg.DefaultBase + "\" unless a call names a base), " +
		"so changes made one after the other before a reboot add up. " +
		"Use get_status to see whether a reboot is pending, list_snapshots for the snapshots, " +
		"and rollback to boot an earlier snapshot next time. " +
		"/etc, /var and /home are not part of the read-only root and are changed directly, not with this server."
	if s.cfg.RebootUnit != "" {
		out += " reboot activates the default snapshot, the way transactional-update reboot does (the configured reboot method: " +
			"at once, or in rebootmgr's maintenance window); it takes the system and this connection down: " +
			"call it only when asked to reboot, or when a change should take effect now and that was agreed, and name the snapshot you expect to boot."
	} else {
		out += " This server does not reboot the system; say that a reboot is needed when a change was applied."
	}
	if s.cfg.AllowCommands {
		out += " run_in_snapshot runs any command as root in a new snapshot; prefer the package tools where they do the job."
	}
	if s.cfg.About != "" {
		out = s.cfg.About + "\n\n" + out
	}
	return out
}

// tool is one MCP tool.
type tool struct {
	name        string
	title       string
	description string
	input       map[string]any
	output      map[string]any
	readOnly    bool
	idempotent  bool
	openWorld   bool
	call        func(ctx context.Context, args json.RawMessage) map[string]any
}

func (s *Server) tools() []*tool {
	ts := []*tool{
		{name: "get_status", title: "Status", readOnly: true, idempotent: true,
			description: "The snapshot the system runs (active) and the one it boots next (default), and whether a reboot is pending to activate changes.",
			input:       object(nil, nil), output: statusSchema, call: s.getStatus},
		{name: "list_snapshots", title: "List snapshots", readOnly: true, idempotent: true,
			description: "The snapshots of the root file system: number, date, description, and which is active (running) and default (booted next).",
			input: object(map[string]any{
				"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "description": "only the newest this many (default: all)"},
			}, nil),
			output: listSchema, call: s.listSnapshots},
		{name: "pkg_install", title: "Install packages", idempotent: true, openWorld: true,
			description: "Installs packages in a new snapshot (transactional-update pkg install): zypper install, chrooted in the snapshot. " +
				"Takes effect at the next reboot.",
			input: object(map[string]any{
				"packages":        packagesSchema,
				"base":            baseSchema,
				"accept_licenses": map[string]any{"type": "boolean", "description": "agree to the packages' licenses (zypper --auto-agree-with-licenses); without it, a package with a license to confirm fails"},
				"no_recommends":   map[string]any{"type": "boolean", "description": "do not install recommended packages"},
			}, []string{"packages"}),
			output: changeSchema, call: s.pkgInstall},
		{name: "pkg_remove", title: "Remove packages", idempotent: true,
			description: "Removes packages in a new snapshot (transactional-update pkg remove): zypper remove, chrooted in the snapshot. " +
				"Takes effect at the next reboot.",
			input: object(map[string]any{
				"packages":   packagesSchema,
				"base":       baseSchema,
				"clean_deps": map[string]any{"type": "boolean", "description": "also remove dependencies no longer needed (zypper --clean-deps)"},
			}, []string{"packages"}),
			output: changeSchema, call: s.pkgRemove},
		{name: "update_system", title: "Update the system", openWorld: true,
			description: "Updates the system in a new snapshot, as transactional-update up, dup or patch: " +
				"mode update (zypper update, the default on SL Micro and Leap Micro), dist-upgrade (zypper dup, the default on MicroOS and Tumbleweed-based systems) " +
				"or patch (zypper patch: the released patches only). Takes effect at the next reboot; with nothing to update, no snapshot is kept.",
			input: object(map[string]any{
				"mode":            map[string]any{"type": "string", "enum": []string{"update", "dist-upgrade", "patch"}, "default": "update"},
				"base":            baseSchema,
				"accept_licenses": map[string]any{"type": "boolean", "description": "agree to the packages' licenses (zypper --auto-agree-with-licenses)"},
				"no_recommends":   map[string]any{"type": "boolean", "description": "do not install newly recommended packages"},
			}, nil),
			output: changeSchema, call: s.updateSystem},
		{name: "rollback", title: "Roll back",
			description: "Makes a snapshot the default, so that the system boots it at the next reboot (transactional-update rollback). " +
				"Use list_snapshots for the numbers; to undo the last change before a reboot, roll back to the active snapshot.",
			input: object(map[string]any{
				"snapshot": map[string]any{"type": "string", "pattern": "^[0-9]{1,10}$", "description": "the snapshot's number"},
			}, []string{"snapshot"}),
			output: rollbackSchema, call: s.rollback},
		{name: "delete_snapshot", title: "Delete a snapshot", idempotent: true,
			description: "Deletes a snapshot. The active and the default snapshot cannot be deleted.",
			input: object(map[string]any{
				"snapshot": map[string]any{"type": "string", "pattern": "^[0-9]{1,10}$", "description": "the snapshot's number"},
			}, []string{"snapshot"}),
			output: simpleSchema, call: s.deleteSnapshot},
		{name: "cleanup_snapshots", title: "Clean up snapshots", idempotent: true,
			description: "Removes snapshots that are no longer needed, by the configured cleanup rules (tukit cleanup, transactional-update cleanup).",
			input:       object(nil, nil), output: simpleSchema, call: s.cleanup},
	}
	if s.cfg.RebootUnit != "" {
		ts = append(ts, &tool{name: "reboot", title: "Reboot into the default snapshot",
			description: "Reboots the system so that the default snapshot becomes the running one, as transactional-update reboot does, " +
				"with the reboot method the administrator configured (REBOOT_METHOD: systemd, soft-reboot or kexec where allowed, rebootmgr's maintenance window, kured). " +
				"Refused when no reboot is pending (the default snapshot is the running one) unless force is set, when snapshot is given and is not the default, " +
				"and while a change of this server runs. The reboot starts about 10 s after the answer; the connection ends with it.",
			input: object(map[string]any{
				"snapshot": map[string]any{"type": "string", "pattern": "^[0-9]{1,10}$", "description": "the snapshot you expect to boot: refused if it is not the default (recommended)"},
				"force":    map[string]any{"type": "boolean", "description": "reboot even though no snapshot is pending (the default is the running one)"},
			}, nil),
			output: rebootSchema, call: s.reboot})
	}
	if s.cfg.AllowCommands {
		ts = append(ts, &tool{name: "run_in_snapshot", title: "Run a command in a new snapshot",
			description: "Runs a command as root, chrooted in a new snapshot (transactional-update run). " +
				"The snapshot is kept and becomes the default if the command exits 0, and is discarded otherwise. " +
				"There is no shell: the command is a program and its arguments; for a pipeline, run [\"/bin/sh\", \"-c\", \"…\"].",
			input: object(map[string]any{
				"command":     map[string]any{"type": "array", "minItems": 1, "maxItems": 256, "items": map[string]any{"type": "string", "maxLength": 4096}, "description": "the program (absolute path, or a name found in the snapshot's PATH) and its arguments"},
				"base":        baseSchema,
				"description": map[string]any{"type": "string", "maxLength": 200, "description": "the snapshot's description"},
			}, []string{"command"}),
			output: changeSchema, call: s.runInSnapshot})
	}
	return ts
}

func (s *Server) tool(name string) *tool {
	for _, t := range s.tools() {
		if t.name == name {
			return t
		}
	}
	return nil
}

func (s *Server) toolList() []map[string]any {
	var out []map[string]any
	for _, t := range s.tools() {
		annotations := map[string]any{"title": t.title, "readOnlyHint": t.readOnly, "openWorldHint": t.openWorld}
		if !t.readOnly {
			annotations["destructiveHint"] = true
		}
		if t.idempotent {
			annotations["idempotentHint"] = true
		}
		out = append(out, map[string]any{"name": t.name, "title": t.title, "description": t.description,
			"inputSchema": t.input, "outputSchema": t.output, "annotations": annotations})
	}
	return out
}

func object(props map[string]any, required []string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	o := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

// packageRE is what a package argument may be: a name, optionally with
// an architecture (.x86_64), a kind (pattern:…) or a version relation
// (foo>=1.2). Leading characters that zypper reads as an option, paths,
// URLs and anything a shell or wordexp would expand are refused.
var packageRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+:~-]*(?:(?:<=|>=|<|>|=)[A-Za-z0-9_.+:~-]+)?$`)

var packagesSchema = map[string]any{"type": "array", "minItems": 1, "maxItems": 100, "uniqueItems": true,
	"items":       map[string]any{"type": "string", "maxLength": 200, "pattern": packageRE.String()},
	"description": "package names, optionally with a version relation (\"foo>=1.2\"), an architecture (\"foo.x86_64\") or a kind (\"pattern:foo\")"}

var baseSchema = map[string]any{"type": "string", "pattern": "^(active|default|[0-9]{1,10})$",
	"description": "the snapshot to build on: default (the one booted next: changes add up), active (the running one: discards changes not yet booted), or a number"}

var changeSchema = map[string]any{"type": "object", "properties": map[string]any{
	"status":           map[string]any{"type": "string", "enum": []string{"applied", "no_changes", "failed", "running"}, "description": "applied: the snapshot is the new default; no_changes: there was nothing to do, the snapshot was discarded; failed: the snapshot was discarded; running: the command still runs (the call ended first)"},
	"snapshot":         map[string]any{"type": "string", "description": "the new snapshot"},
	"base":             map[string]any{"type": "string"},
	"command":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	"exit_code":        map[string]any{"type": "integer"},
	"output":           map[string]any{"type": "string"},
	"output_truncated": map[string]any{"type": "boolean"},
	"reboot_required":  map[string]any{"type": "boolean"},
	"error":            map[string]any{"type": "string"},
}, "required": []string{"status"}}

var snapshotSchema = map[string]any{"type": "object", "properties": map[string]any{
	"number": map[string]any{"type": "string"}, "date": map[string]any{"type": "string"}, "description": map[string]any{"type": "string"},
	"active": map[string]any{"type": "boolean"}, "default": map[string]any{"type": "boolean"},
}}

var listSchema = map[string]any{"type": "object", "properties": map[string]any{
	"snapshots": map[string]any{"type": "array", "items": snapshotSchema},
	"total":     map[string]any{"type": "integer"},
}, "required": []string{"snapshots"}}

var statusSchema = map[string]any{"type": "object", "properties": map[string]any{
	"active":         map[string]any{"type": "string", "description": "the snapshot running now"},
	"default":        map[string]any{"type": "string", "description": "the snapshot booted next"},
	"reboot_pending": map[string]any{"type": "boolean", "description": "the default differs from the active snapshot: a reboot activates it"},
	"snapshots":      map[string]any{"type": "integer"},
	"next":           snapshotSchema,
}, "required": []string{"reboot_pending"}}

var rollbackSchema = map[string]any{"type": "object", "properties": map[string]any{
	"snapshot":        map[string]any{"type": "string", "description": "the snapshot rolled back to"},
	"default":         map[string]any{"type": "string", "description": "the snapshot booted next (a copy of snapshot on some systems)"},
	"reboot_required": map[string]any{"type": "boolean"},
}, "required": []string{"default"}}

var rebootSchema = map[string]any{"type": "object", "properties": map[string]any{
	"started": map[string]any{"type": "boolean"},
	"active":  map[string]any{"type": "string", "description": "the snapshot running now"},
	"default": map[string]any{"type": "string", "description": "the snapshot the system boots"},
	"method":  map[string]any{"type": "string", "description": "transactional-update's REBOOT_METHOD"},
	"unit":    map[string]any{"type": "string"},
}, "required": []string{"started"}}

var simpleSchema = map[string]any{"type": "object", "properties": map[string]any{
	"done": map[string]any{"type": "boolean"},
}}

// decode reads a tool's arguments into v, refusing unknown ones.
func decode(args json.RawMessage, v any) error {
	d := json.NewDecoder(strings.NewReader(string(args)))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("arguments: %v", err)
	}
	return nil
}

func result(text string, structured any, isError bool) map[string]any {
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": text}},
		"structuredContent": structured,
		"isError":           isError,
	}
}

func toolError(format string, a ...any) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": fmt.Sprintf(format, a...)}}, "isError": true}
}

// opContext bounds a tukit call that is not a command run.
func opContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 10*time.Minute)
}

type snapshot struct {
	Number      string `json:"number"`
	Date        string `json:"date,omitempty"`
	Description string `json:"description,omitempty"`
	Active      bool   `json:"active"`
	Default     bool   `json:"default"`
}

func yes(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "true", "1":
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

func (s *Server) checkPackages(pkgs []string) error {
	if len(pkgs) == 0 {
		return errors.New("packages: none given")
	}
	if len(pkgs) > 100 {
		return errors.New("packages: at most 100")
	}
	for _, p := range pkgs {
		if len(p) > 200 || !packageRE.MatchString(p) {
			return fmt.Errorf("package %q: not a package name (letters, digits and _.+:~-, optionally a version relation such as foo>=1.2)", p)
		}
	}
	return nil
}

func (s *Server) base(b string) (string, error) {
	if b == "" {
		return s.cfg.DefaultBase, nil
	}
	if !tukit.ValidSnapshot(b, true) {
		return "", fmt.Errorf("base %q: must be active, default or a snapshot number", b)
	}
	return b, nil
}

func (s *Server) zypper(args ...string) []string {
	return append([]string{s.cfg.Zypper, "--non-interactive"}, args...)
}

func (s *Server) pkgInstall(ctx context.Context, args json.RawMessage) map[string]any {
	var a struct {
		Packages       []string `json:"packages"`
		Base           string   `json:"base"`
		AcceptLicenses bool     `json:"accept_licenses"`
		NoRecommends   bool     `json:"no_recommends"`
	}
	if err := decode(args, &a); err != nil {
		return toolError("%v", err)
	}
	if err := s.checkPackages(a.Packages); err != nil {
		return toolError("%v", err)
	}
	base, err := s.base(a.Base)
	if err != nil {
		return toolError("%v", err)
	}
	argv := s.zypper("install")
	if a.AcceptLicenses {
		argv = append(argv, "--auto-agree-with-licenses")
	}
	if a.NoRecommends {
		argv = append(argv, "--no-recommends")
	}
	argv = append(argv, a.Packages...)
	return s.transact(ctx, base, "pkg install "+strings.Join(a.Packages, " "), argv, zypperOutcome)
}

func (s *Server) pkgRemove(ctx context.Context, args json.RawMessage) map[string]any {
	var a struct {
		Packages  []string `json:"packages"`
		Base      string   `json:"base"`
		CleanDeps bool     `json:"clean_deps"`
	}
	if err := decode(args, &a); err != nil {
		return toolError("%v", err)
	}
	if err := s.checkPackages(a.Packages); err != nil {
		return toolError("%v", err)
	}
	base, err := s.base(a.Base)
	if err != nil {
		return toolError("%v", err)
	}
	argv := s.zypper("remove")
	if a.CleanDeps {
		argv = append(argv, "--clean-deps")
	}
	argv = append(argv, a.Packages...)
	return s.transact(ctx, base, "pkg remove "+strings.Join(a.Packages, " "), argv, zypperOutcome)
}

func (s *Server) updateSystem(ctx context.Context, args json.RawMessage) map[string]any {
	var a struct {
		Mode           string `json:"mode"`
		Base           string `json:"base"`
		AcceptLicenses bool   `json:"accept_licenses"`
		NoRecommends   bool   `json:"no_recommends"`
	}
	if err := decode(args, &a); err != nil {
		return toolError("%v", err)
	}
	base, err := s.base(a.Base)
	if err != nil {
		return toolError("%v", err)
	}
	var argv []string
	var what string
	switch a.Mode {
	case "", "update":
		argv, what = s.zypper("update"), "up"
	case "dist-upgrade":
		argv, what = s.zypper("dist-upgrade"), "dup"
	case "patch":
		argv, what = s.zypper("patch"), "patch"
	default:
		return toolError("mode %q: must be update, dist-upgrade or patch", a.Mode)
	}
	if a.AcceptLicenses {
		argv = append(argv, "--auto-agree-with-licenses")
	}
	if a.NoRecommends {
		argv = append(argv, "--no-recommends")
	}
	return s.transact(ctx, base, what, argv, zypperOutcome)
}

func (s *Server) runInSnapshot(ctx context.Context, args json.RawMessage) map[string]any {
	var a struct {
		Command     []string `json:"command"`
		Base        string   `json:"base"`
		Description string   `json:"description"`
	}
	if err := decode(args, &a); err != nil {
		return toolError("%v", err)
	}
	if len(a.Command) == 0 || a.Command[0] == "" {
		return toolError("command: none given")
	}
	if len(a.Command) > 256 {
		return toolError("command: at most 256 words")
	}
	for _, w := range a.Command {
		if len(w) > 4096 || strings.ContainsRune(w, 0) {
			return toolError("command: a word is longer than 4096 bytes or contains a NUL byte")
		}
	}
	base, err := s.base(a.Base)
	if err != nil {
		return toolError("%v", err)
	}
	desc := a.Description
	if desc == "" {
		desc = "run " + strings.Join(a.Command, " ")
	}
	return s.transact(ctx, base, desc, a.Command, exitOutcome)
}

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
// command. It goes on when the call is cancelled, or ends first: a change
// is finished (the snapshot closed or discarded) rather than left
// half-done, within the configured timeout.
func (s *Server) transact(ctx context.Context, base, what string, argv []string, decide func(tukit.CallResult) outcome) map[string]any {
	done := make(chan change, 1)
	opened := make(chan string, 1)
	go func() {
		s.change.Lock()
		defer s.change.Unlock()
		work, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.Timeout)
		defer cancel()
		done <- s.run(work, base, what, argv, decide, opened)
	}()
	tick := time.NewTicker(s.interval)
	defer tick.Stop()
	snap := ""
	start := time.Now()
	for {
		select {
		case c := <-done:
			return changeResult(c)
		case snap = <-opened:
			mcp.Progress(ctx, fmt.Sprintf("snapshot %s opened from %s; running %s", snap, base, what))
		case <-tick.C:
			mcp.Progress(ctx, fmt.Sprintf("running %s in snapshot %s (%s)", what, snap, time.Since(start).Round(time.Second)))
		case <-ctx.Done():
			// Not answered (cancelled), but the change goes on to its end.
			return changeResult(change{Status: "running", Snapshot: snap, Base: base, Command: argv})
		}
	}
}

func (s *Server) run(ctx context.Context, base, what string, argv []string, decide func(tukit.CallResult) outcome, opened chan<- string) change {
	c := change{Base: base, Command: argv}
	snap, err := s.tk.Open(ctx, base, "MCP: "+what)
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

func (s *Server) logf(format string, a ...any) {
	_, _ = fmt.Fprintf(s.cfg.Log, Name+": "+format+"\n", a...)
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
		return toolError("cleanup: %v", err)
	}
	s.logf("cleanup done")
	return result("Snapshots cleaned up.", map[string]any{"done": true}, false)
}

func (s *Server) reboot(ctx context.Context, args json.RawMessage) map[string]any {
	var a struct {
		Snapshot string `json:"snapshot"`
		Force    bool   `json:"force"`
	}
	if err := decode(args, &a); err != nil {
		return toolError("%v", err)
	}
	if a.Snapshot != "" && !tukit.ValidSnapshot(a.Snapshot, false) {
		return toolError("snapshot %q: must be a snapshot number", a.Snapshot)
	}
	// Not in the middle of a change: its snapshot would be cut off.
	if !s.change.TryLock() {
		return toolError("a change of this server is running; reboot once it has ended")
	}
	defer s.change.Unlock()
	ctx, cancel := opContext(context.WithoutCancel(ctx))
	defer cancel()
	snaps, err := s.snapshots(ctx)
	if err != nil {
		return toolError("listing snapshots: %v", err)
	}
	var active, def string
	for _, sn := range snaps {
		if sn.Active {
			active = sn.Number
		}
		if sn.Default {
			def = sn.Number
		}
	}
	if def == "" {
		return toolError("tukit names no default snapshot; not rebooting")
	}
	if a.Snapshot != "" && a.Snapshot != def {
		return toolError("snapshot %s is not the default: the system would boot snapshot %s; not rebooting", a.Snapshot, def)
	}
	if def == active && !a.Force {
		return toolError("no reboot pending: the default snapshot %s is the running one (force reboots anyway)", def)
	}
	method := rebootMethod(s.cfg.RebootConfig)
	job, err := s.tk.StartUnit(ctx, s.cfg.RebootUnit)
	if err != nil {
		s.logf("reboot into %s: %v", def, err)
		return toolError("reboot: %v", err)
	}
	s.logf("reboot into %s (running %s, REBOOT_METHOD=%s): %s started, job %s", def, active, method, s.cfg.RebootUnit, job)
	var how string
	switch method {
	case "rebootmgr":
		how = "rebootmgr reboots in its maintenance window"
	case "kured":
		how = "kured is told to reboot (it drains the node first)"
	case "notify":
		how = "the logged-in users are notified to reboot"
	case "none":
		how = "reboots are turned off (REBOOT_METHOD=none): nothing happens"
	default:
		how = "the system reboots in about 10 s (rebootmgr's maintenance window instead, if rebootmgr runs)"
	}
	text := fmt.Sprintf("Reboot into snapshot %s started with transactional-update reboot (REBOOT_METHOD=%s): %s. This connection ends with it.", def, method, how)
	return result(text, map[string]any{"started": true, "active": active, "default": def, "method": method, "unit": s.cfg.RebootUnit}, false)
}

// rebootMethod is transactional-update's REBOOT_METHOD from its
// configuration files (shell assignments; the last one wins), "auto" if
// none sets it.
func rebootMethod(files []string) string {
	method := "auto"
	for _, f := range files {
		paths := []string{f}
		if strings.HasSuffix(f, ".d") {
			paths, _ = filepath.Glob(filepath.Join(f, "*"))
		}
		for _, p := range paths {
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(data), "\n") {
				v, ok := strings.CutPrefix(strings.TrimSpace(line), "REBOOT_METHOD=")
				if !ok {
					continue
				}
				v, _, _ = strings.Cut(v, "#")
				v = strings.Trim(strings.TrimSpace(v), `"'`)
				if v != "" {
					method = v
				}
			}
		}
	}
	return method
}
