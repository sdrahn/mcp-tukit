package server

// Activating the default snapshot: reboot and apply.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sdrahn/mcp-tukit/internal/tukit"
)

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

func (s *Server) apply(ctx context.Context, args json.RawMessage) map[string]any {
	var a struct {
		Snapshot string `json:"snapshot"`
	}
	if err := decode(args, &a); err != nil {
		return toolError("%v", err)
	}
	if a.Snapshot != "" && !tukit.ValidSnapshot(a.Snapshot, false) {
		return toolError("snapshot %q: must be a snapshot number", a.Snapshot)
	}
	// Not in the middle of a change: the snapshot would not be complete.
	if !s.change.TryLock() {
		return toolError("a change of this server is running; apply once it has ended")
	}
	defer s.change.Unlock()
	ctx, cancel := opContext(context.WithoutCancel(ctx))
	defer cancel()
	active, def, err := s.activeDefault(ctx)
	if err != nil {
		return toolError("listing snapshots: %v", err)
	}
	if def == "" {
		return toolError("tukit names no default snapshot; nothing applied")
	}
	if a.Snapshot != "" && a.Snapshot != def {
		return toolError("snapshot %s is not the default: apply would make snapshot %s the running system's; nothing applied", a.Snapshot, def)
	}
	if def == active {
		return toolError("nothing to apply: the default snapshot %s is the running one", def)
	}
	run, err := s.tk.RunUnit(ctx, s.cfg.ApplyUnit)
	if err != nil {
		s.logf("apply %s: %v", def, err)
		return toolError("apply: %v", err)
	}
	if !run.OK() {
		s.logf("apply %s: %s ended %s (%s)", def, s.cfg.ApplyUnit, run.Result, run.ActiveState)
		return result(fmt.Sprintf("transactional-update apply failed (%s, %s): snapshot %s is not applied; it still boots next. "+
			"The administrator finds why in journalctl -u %s.", run.Result, run.ActiveState, def, s.cfg.ApplyUnit),
			map[string]any{"applied": false, "snapshot": def, "result": run.Result}, true)
	}
	now, _, err := s.activeDefault(ctx)
	if err != nil {
		now = ""
	}
	s.logf("apply %s: %s done (running %s before, %s now)", def, s.cfg.ApplyUnit, active, now)
	text := fmt.Sprintf("Snapshot %s applied: its /usr, /etc and /boot are the running system's now. "+
		"Running programs and services keep their old files until they are restarted; a new kernel or initrd takes effect at the next reboot.", def)
	return result(text, map[string]any{"applied": true, "snapshot": def, "active": now, "result": run.Result}, false)
}
