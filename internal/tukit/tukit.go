// Package tukit talks to tukitd, the D-Bus service of libtukit
// (https://github.com/openSUSE/transactional-update, dbus/), on the
// system bus.
//
// The server does none of the privileged work itself: tukitd creates
// the snapshot, mounts it, runs the command in it (chrooted) and sets it
// as the next default. That keeps the server's own process unprivileged
// (no capabilities, no mounts, no btrfs ioctls), so it can run in the
// gateway's sandbox and a small SELinux domain whose only way to the
// system is the D-Bus chat with tukitd.
package tukit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	busName          = "org.opensuse.tukit"
	transactionPath  = "/org/opensuse/tukit/Transaction"
	transactionIface = "org.opensuse.tukit.Transaction"
	snapshotPath     = "/org/opensuse/tukit/Snapshot"
	snapshotIface    = "org.opensuse.tukit.Snapshot"
)

// Columns are the snapshot list columns tukit knows (tukit snapshots
// --fields).
var Columns = []string{"number", "default", "active", "date", "description"}

// CallResult is the end of a command run in a snapshot.
type CallResult struct {
	// ExitCode is the command's exit status, or for Failed tukit's
	// error code.
	ExitCode int
	// Output is the command's output (stdout and stderr), or for Failed
	// tukit's error message.
	Output string
	// Failed is set when tukitd reported an Error signal: the command
	// could not be run, or the snapshot could not be kept.
	Failed bool
}

// Backend is what the server needs of tukit; Client is the D-Bus one,
// tests use a fake.
type Backend interface {
	// List returns the snapshots, each with the given columns.
	List(ctx context.Context, columns []string) ([]map[string]string, error)
	// Open creates a new snapshot from base ("active", "default" or a
	// number) and returns its id.
	Open(ctx context.Context, base, description string) (string, error)
	// Call runs argv chrooted in the open snapshot and waits for its end.
	// When ctx ends first, it returns ctx's error: the command goes on in
	// tukitd.
	Call(ctx context.Context, snapshot string, argv []string) (CallResult, error)
	// Close finalizes the snapshot: it becomes the default at next boot.
	Close(ctx context.Context, snapshot string) error
	// Abort discards the snapshot.
	Abort(ctx context.Context, snapshot string) error
	// Delete deletes a snapshot.
	Delete(ctx context.Context, snapshot string) error
	// Cleanup removes snapshots that are no longer needed.
	Cleanup(ctx context.Context) error
	// RollbackTo makes snapshot the default at next boot and returns the
	// snapshot that will boot (a copy on some systems).
	RollbackTo(ctx context.Context, snapshot string) (string, error)
	// StartUnit asks systemd to start a unit (the reboot unit) and returns
	// the job's object path. tukitd has no reboot of its own.
	StartUnit(ctx context.Context, unit string) (string, error)
}

// Quote returns argv as a command line for tukitd, which splits it with
// wordexp(3): every word single-quoted, so that nothing in it is
// expanded or substituted.
func Quote(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(words, " ")
}

// Client is a Backend on the system bus.
type Client struct {
	conn *dbus.Conn
	mu   sync.Mutex // guards noOpts, owner, waiters
	// noOpts: tukitd lacks the …WithOpts methods (before 6.0); set on
	// the first UnknownMethod.
	noOpts  bool
	owner   string // unique name of org.opensuse.tukit, as last seen
	waiters map[string]chan CallResult
}

// Connect connects to the system bus and subscribes to tukitd's signals.
func Connect() (*Client, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("system bus: %w", err)
	}
	return NewClient(conn)
}

// NewClient returns a Client on conn, a connection to the bus tukitd is
// on (tests: a private bus), and subscribes to tukitd's signals. The
// Client owns conn.
func NewClient(conn *dbus.Conn) (*Client, error) {
	c := &Client{conn: conn, waiters: map[string]chan CallResult{}}
	for _, member := range []string{"CommandExecuted", "Error"} {
		// Only tukitd's: the bus resolves the well-known name in the
		// match to its owner (signals from any other peer are dropped
		// again in dispatch, should the owner change).
		if err := conn.AddMatchSignal(dbus.WithMatchSender(busName), dbus.WithMatchInterface(transactionIface),
			dbus.WithMatchMember(member)); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("subscribing to tukitd's %s signal: %w", member, err)
		}
	}
	ch := make(chan *dbus.Signal, 16)
	conn.Signal(ch)
	go c.dispatch(ch)
	return c, nil
}

// CloseBus closes the bus connection.
func (c *Client) CloseBus() error { return c.conn.Close() }

func (c *Client) dispatch(ch chan *dbus.Signal) {
	for sig := range ch {
		var r CallResult
		switch sig.Name {
		case transactionIface + ".CommandExecuted":
		case transactionIface + ".Error":
			r.Failed = true
		default:
			continue
		}
		if len(sig.Body) != 3 || !c.fromTukitd(sig.Sender) {
			continue
		}
		snapshot, ok1 := sig.Body[0].(string)
		code, ok2 := sig.Body[1].(int32)
		output, ok3 := sig.Body[2].(string)
		if !ok1 || !ok2 || !ok3 {
			continue
		}
		r.ExitCode, r.Output = int(code), output
		c.mu.Lock()
		w := c.waiters[snapshot]
		delete(c.waiters, snapshot)
		c.mu.Unlock()
		if w != nil {
			w <- r
		}
	}
}

// fromTukitd tells whether sender is the current owner of tukitd's name.
func (c *Client) fromTukitd(sender string) bool {
	c.mu.Lock()
	owner := c.owner
	c.mu.Unlock()
	if sender == owner && owner != "" {
		return true
	}
	var now string
	if err := c.conn.BusObject().Call("org.freedesktop.DBus.GetNameOwner", 0, busName).Store(&now); err != nil {
		return false
	}
	c.mu.Lock()
	c.owner = now
	c.mu.Unlock()
	return sender == now
}

func (c *Client) call(ctx context.Context, path dbus.ObjectPath, method string, args []any, out ...any) error {
	call := c.conn.Object(busName, path).CallWithContext(ctx, method, 0, args...)
	if call.Err != nil {
		return wrap(call.Err)
	}
	if len(out) > 0 {
		return call.Store(out...)
	}
	return nil
}

// callOpts calls method+"WithOpts" with options where tukitd has it,
// else method without them.
func (c *Client) callOpts(ctx context.Context, path dbus.ObjectPath, iface, method string, args []any, opts map[string]dbus.Variant, out ...any) error {
	c.mu.Lock()
	noOpts := c.noOpts
	c.mu.Unlock()
	if !noOpts && len(opts) > 0 {
		err := c.call(ctx, path, iface+"."+method+"WithOpts", append(args, opts), out...)
		var te *Error
		if !errors.As(err, &te) || te.Name != "org.freedesktop.DBus.Error.UnknownMethod" {
			return err
		}
		c.mu.Lock()
		c.noOpts = true
		c.mu.Unlock()
	}
	return c.call(ctx, path, iface+"."+method, args, out...)
}

// Error is an error reported by tukitd (org.opensuse.tukit.Error) or the
// bus.
type Error struct {
	Name    string
	Message string
}

func (e *Error) Error() string {
	switch e.Name {
	case "org.opensuse.tukit.Error":
		return "tukit: " + e.Message
	case "org.freedesktop.DBus.Error.AccessDenied":
		return "tukitd refused access (D-Bus policy: the account needs to be allowed org.opensuse.tukit): " + e.Message
	case "org.freedesktop.DBus.Error.ServiceUnknown", "org.freedesktop.DBus.Error.NameHasNoOwner":
		return "tukitd is not available (is the tukit D-Bus service, package tukitd, installed?): " + e.Message
	}
	return e.Name + ": " + e.Message
}

func wrap(err error) error {
	var de dbus.Error
	if errors.As(err, &de) {
		msg := ""
		if len(de.Body) > 0 {
			msg, _ = de.Body[0].(string)
		}
		return &Error{Name: de.Name, Message: msg}
	}
	return err
}

func (c *Client) List(ctx context.Context, columns []string) ([]map[string]string, error) {
	var list []map[string]string
	if err := c.call(ctx, snapshotPath, snapshotIface+".List", []any{strings.Join(columns, ",")}, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (c *Client) Open(ctx context.Context, base, description string) (string, error) {
	var id string
	var opts map[string]dbus.Variant
	if description != "" {
		opts = map[string]dbus.Variant{"Description": dbus.MakeVariant(description)}
	}
	err := c.callOpts(ctx, transactionPath, transactionIface, "Open", []any{base}, opts, &id)
	return id, err
}

func (c *Client) Call(ctx context.Context, snapshot string, argv []string) (CallResult, error) {
	// Wait for the signal before calling: the command may end before the
	// method returns.
	done := make(chan CallResult, 1)
	c.mu.Lock()
	c.waiters[snapshot] = done
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		if c.waiters[snapshot] == done {
			delete(c.waiters, snapshot)
		}
		c.mu.Unlock()
	}
	// The method returns once tukitd started the command.
	start, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err := c.call(start, transactionPath, transactionIface+".Call", []any{snapshot, Quote(argv)})
	cancel()
	if err != nil {
		forget()
		return CallResult{}, err
	}
	select {
	case r := <-done:
		return r, nil
	case <-ctx.Done():
		forget()
		return CallResult{}, ctx.Err()
	}
}

func (c *Client) Close(ctx context.Context, snapshot string) error {
	return c.call(ctx, transactionPath, transactionIface+".Close", []any{snapshot})
}

func (c *Client) Abort(ctx context.Context, snapshot string) error {
	return c.call(ctx, transactionPath, transactionIface+".Abort", []any{snapshot})
}

func (c *Client) Delete(ctx context.Context, snapshot string) error {
	return c.call(ctx, snapshotPath, snapshotIface+".Delete", []any{snapshot})
}

func (c *Client) Cleanup(ctx context.Context) error {
	return c.call(ctx, snapshotPath, snapshotIface+".Cleanup", nil)
}

func (c *Client) RollbackTo(ctx context.Context, snapshot string) (string, error) {
	// Before 6.2 RollbackTo returns nothing: the snapshot itself boots.
	call := c.conn.Object(busName, snapshotPath).CallWithContext(ctx, snapshotIface+".RollbackTo", 0, snapshot)
	if call.Err != nil {
		return "", wrap(call.Err)
	}
	if len(call.Body) == 1 {
		if id, ok := call.Body[0].(string); ok && id != "" {
			return id, nil
		}
	}
	return snapshot, nil
}

// ValidSnapshot tells whether id can name a snapshot: a number (snapper),
// or for the base of a new one, "active" or "default".
func ValidSnapshot(id string, base bool) bool {
	if base && (id == "active" || id == "default") {
		return true
	}
	if id == "" || len(id) > 10 {
		return false
	}
	_, err := strconv.ParseUint(id, 10, 32)
	return err == nil
}
