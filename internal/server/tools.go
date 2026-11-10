package server

// The tools: their registry, schemas and the helpers every tool uses.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

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
		{name: "list_updates", title: "List available updates", readOnly: true, idempotent: true, openWorld: true,
			description: "Lists the updates the repositories offer, as the system would be from base: kind package (zypper list-updates: newer versions of installed packages) " +
				"or patch (zypper list-patches: the needed patches, with category and severity). Changes nothing: it runs zypper in a new snapshot, which is discarded. " +
				"zypper refreshes repositories whose metadata are older than its refresh delay; refresh_repositories refreshes them now. " +
				"On MicroOS and Tumbleweed-based systems update_system with dist-upgrade may bring more than list_updates shows.",
			input: object(map[string]any{
				"kind": map[string]any{"type": "string", "enum": []string{"package", "patch"}, "default": "package"},
				"base": baseSchema,
			}, nil),
			output: updateListSchema, call: s.listUpdates},
		{name: "refresh_repositories", title: "Refresh the repositories", idempotent: true, openWorld: true,
			description: "Refreshes the metadata of the enabled repositories (zypper refresh), so that list_updates and later changes see what the repositories offer now. " +
				"It runs zypper in a new snapshot, which is discarded; the metadata are kept (/var/cache/zypp is the running system's). It installs nothing.",
			input: object(map[string]any{
				"force": map[string]any{"type": "boolean", "description": "download the metadata even if they are up to date (zypper refresh --force)"},
				"base":  baseSchema,
			}, nil),
			output: refreshSchema, call: s.refreshRepositories},
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
	if s.cfg.ApplyUnit != "" {
		ts = append(ts, &tool{name: "apply", title: "Apply the default snapshot to the running system",
			description: "Mounts the default snapshot's /usr, /etc and /boot into the running system, without a reboot, as transactional-update apply does: " +
				"newly installed programs can be used at once. Like an update on a system without snapshots, running programs keep the files they have open, " +
				"services are not restarted, and a new kernel or initrd takes effect only at the next reboot. " +
				"Refused when there is nothing to apply (the default snapshot is the running one), when snapshot is given and is not the default, " +
				"and while a change of this server runs.",
			input: object(map[string]any{
				"snapshot": map[string]any{"type": "string", "pattern": "^[0-9]{1,10}$", "description": "the snapshot you expect to apply: refused if it is not the default (recommended)"},
			}, nil),
			output: applySchema, call: s.apply})
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

var applySchema = map[string]any{"type": "object", "properties": map[string]any{
	"applied":  map[string]any{"type": "boolean"},
	"snapshot": map[string]any{"type": "string", "description": "the snapshot applied (the default)"},
	"active":   map[string]any{"type": "string", "description": "the snapshot tukit reports as running afterwards"},
	"result":   map[string]any{"type": "string", "description": "the result of the apply unit (systemd): success, exit-code, ..."},
}, "required": []string{"applied"}}

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
