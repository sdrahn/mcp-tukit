package tukit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	systemdName  = "org.freedesktop.systemd1"
	systemdPath  = "/org/freedesktop/systemd1"
	systemdIface = "org.freedesktop.systemd1.Manager"
)

// SystemdError is an error systemd reported.
type SystemdError struct {
	Unit    string
	Name    string
	Message string
}

func (e *SystemdError) Error() string {
	switch e.Name {
	case "org.freedesktop.systemd1.NoSuchUnit":
		return "systemd: there is no unit " + e.Unit + " (package mcp-server-tukit-gateway installs it)"
	case "org.freedesktop.DBus.Error.AccessDenied", "org.freedesktop.DBus.Error.InteractiveAuthorizationRequired":
		return "systemd refused to start " + e.Unit + " (the server needs CAP_SYS_ADMIN, and SELinux allows mcpsrv_tukit_t this unit only): " + e.Message
	}
	return "systemd: " + e.Name + ": " + e.Message
}

// StartUnit starts unit through systemd's manager (mode "replace") and
// returns the job's path. systemd lets a caller with CAP_SYS_ADMIN start
// units (others it asks polkit about) and checks the SELinux service
// class on the unit file's type.
func (c *Client) StartUnit(ctx context.Context, unit string) (string, error) {
	var job dbus.ObjectPath
	err := c.conn.Object(systemdName, systemdPath).CallWithContext(ctx, systemdIface+".StartUnit", 0, unit, "replace").Store(&job)
	if err != nil {
		return "", systemdErr(unit, err)
	}
	return string(job), nil
}

func systemdErr(unit string, err error) error {
	var de dbus.Error
	if errors.As(err, &de) {
		msg := ""
		if len(de.Body) > 0 {
			msg, _ = de.Body[0].(string)
		}
		return &SystemdError{Unit: unit, Name: de.Name, Message: msg}
	}
	return err
}

// UnitRun is how a run of a oneshot unit ended.
type UnitRun struct {
	// ActiveState is the unit's state afterwards: "inactive" (or
	// "active" with RemainAfterExit) when it succeeded, "failed" when not.
	ActiveState string
	// Result is the service's result: "success", "exit-code", "timeout", ...
	Result string
}

// OK tells whether the run succeeded.
func (r UnitRun) OK() bool { return r.Result == "success" && r.ActiveState != "failed" }

// RunUnit starts a oneshot unit and waits until its job is done, then
// returns how it ended. It polls the unit's Job property: that needs only
// the SELinux "status" permission on the unit's own type, where
// subscribing to systemd's job signals would need it on systemd itself.
// The unit may re-execute systemd (transactional-update apply does), which
// leaves the bus for a moment and drops the calls it has not answered:
// those are asked again until ctx ends.
func (c *Client) RunUnit(ctx context.Context, unit string) (UnitRun, error) {
	if _, err := c.StartUnit(ctx, unit); err != nil {
		return UnitRun{}, err
	}
	// call retries what fails while systemd is away.
	call := func(obj dbus.BusObject, method string, out any, args ...any) error {
		for {
			err := obj.CallWithContext(ctx, method, dbus.FlagNoAutoStart, args...).Store(out)
			if err == nil {
				return nil
			}
			if !systemdAway(err) {
				return systemdErr(unit, err)
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("%s: systemd did not come back: %w (%v)", unit, ctx.Err(), systemdErr(unit, err))
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	var path dbus.ObjectPath
	if err := call(c.conn.Object(systemdName, systemdPath), systemdIface+".GetUnit", &path, unit); err != nil {
		return UnitRun{}, err
	}
	obj := c.conn.Object(systemdName, path)
	get := func(prop string) (dbus.Variant, error) {
		var v dbus.Variant
		i := strings.LastIndex(prop, ".")
		err := call(obj, "org.freedesktop.DBus.Properties.Get", &v, prop[:i], prop[i+1:])
		return v, err
	}
	for {
		v, err := get("org.freedesktop.systemd1.Unit.Job")
		if err != nil {
			return UnitRun{}, err
		}
		// Job is (uo): the id, 0 when no job is queued or running.
		if job, ok := v.Value().([]interface{}); ok && len(job) == 2 {
			if id, ok := job[0].(uint32); ok && id == 0 {
				break
			}
		} else {
			return UnitRun{}, fmt.Errorf("systemd: unexpected Job property of %s: %v", unit, v)
		}
		select {
		case <-ctx.Done():
			return UnitRun{}, fmt.Errorf("%s still runs: %w", unit, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	state, err := get("org.freedesktop.systemd1.Unit.ActiveState")
	if err != nil {
		return UnitRun{}, err
	}
	result, err := get("org.freedesktop.systemd1.Service.Result")
	if err != nil {
		return UnitRun{}, err
	}
	var r UnitRun
	r.ActiveState, _ = state.Value().(string)
	r.Result, _ = result.Value().(string)
	return r, nil
}

// systemdAway tells whether err comes from systemd being off the bus for
// a moment, as while it re-executes, rather than from systemd itself.
func systemdAway(err error) bool {
	var de dbus.Error
	if !errors.As(err, &de) {
		return false
	}
	switch de.Name {
	case "org.freedesktop.DBus.Error.NoReply",
		"org.freedesktop.DBus.Error.ServiceUnknown",
		"org.freedesktop.DBus.Error.NameHasNoOwner",
		"org.freedesktop.DBus.Error.Disconnected",
		"org.freedesktop.DBus.Error.Timeout":
		return true
	}
	return false
}

func (l *Lazy) StartUnit(ctx context.Context, unit string) (string, error) {
	c, err := l.client()
	if err != nil {
		return "", err
	}
	return c.StartUnit(ctx, unit)
}

func (l *Lazy) RunUnit(ctx context.Context, unit string) (UnitRun, error) {
	c, err := l.client()
	if err != nil {
		return UnitRun{}, err
	}
	return c.RunUnit(ctx, unit)
}
