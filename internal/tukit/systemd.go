package tukit

import (
	"context"
	"errors"

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
		return "systemd refused to start " + e.Unit + " (the server must run privileged, and SELinux allows mcpsrv_tukit_t this unit only): " + e.Message
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
	var de dbus.Error
	if errors.As(err, &de) {
		msg := ""
		if len(de.Body) > 0 {
			msg, _ = de.Body[0].(string)
		}
		return "", &SystemdError{Unit: unit, Name: de.Name, Message: msg}
	}
	return string(job), err
}

func (l *Lazy) StartUnit(ctx context.Context, unit string) (string, error) {
	c, err := l.client()
	if err != nil {
		return "", err
	}
	return c.StartUnit(ctx, unit)
}
