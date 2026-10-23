package tukit

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// Quote's words come back as they were from a POSIX shell, which splits
// and expands as wordexp(3) does.
func TestQuote(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	argv := []string{"zypper", "in", "it's", "$(touch /tmp/pwned)", "`id`", "${HOME}", "a b", "*", "x>=1", ";|&<>(){}", "\\n", ""}
	out, err := exec.Command(sh, "-c", `for a in `+Quote(argv)+`; do printf '%s\0' "$a"; done`).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if strings.Join(got, "|") != strings.Join(argv, "|") {
		t.Fatalf("got %q\nwant %q", got, argv)
	}
}

func TestValidSnapshot(t *testing.T) {
	for id, want := range map[string][2]bool{
		"12": {true, true}, "0": {true, true}, "active": {false, true}, "default": {false, true},
		"": {false, false}, "-1": {false, false}, "1 2": {false, false}, "12345678901": {false, false}, "x": {false, false},
	} {
		if got := [2]bool{ValidSnapshot(id, false), ValidSnapshot(id, true)}; got != want {
			t.Errorf("%q: %v, want %v", id, got, want)
		}
	}
}

// fakeTukitd answers like tukitd 5.x (no …WithOpts methods).
type fakeTukitd struct {
	conn *dbus.Conn
	mu   sync.Mutex
	log  []string
	next int
}

func (f *fakeTukitd) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, s)
}

func (f *fakeTukitd) Open(base string) (string, *dbus.Error) {
	f.mu.Lock()
	f.next++
	id := string(rune('0' + f.next))
	f.mu.Unlock()
	f.record("Open " + base)
	return id, nil
}

func (f *fakeTukitd) Call(tx, command string) *dbus.Error {
	f.record("Call " + tx + " " + command)
	go func() {
		time.Sleep(20 * time.Millisecond)
		if strings.Contains(command, "fail") {
			_ = f.conn.Emit(transactionPath, transactionIface+".Error", tx, int32(-1), "mount failed")
			return
		}
		_ = f.conn.Emit(transactionPath, transactionIface+".CommandExecuted", tx, int32(0), "ran "+command)
	}()
	return nil
}

func (f *fakeTukitd) Close(tx string) *dbus.Error { f.record("Close " + tx); return nil }
func (f *fakeTukitd) Abort(tx string) *dbus.Error { f.record("Abort " + tx); return nil }

type fakeSnapshots struct{}

func (fakeSnapshots) List(columns string) ([]map[string]string, *dbus.Error) {
	if columns != strings.Join(Columns, ",") {
		return nil, dbus.NewError("org.opensuse.tukit.Error", []any{"Column list contains invalid characters."})
	}
	return []map[string]string{{"number": "1", "active": "yes", "default": "yes"}}, nil
}

func (fakeSnapshots) RollbackTo(s string) *dbus.Error { return nil }

// privateBus starts a dbus-daemon of its own and returns its address.
func privateBus(t *testing.T) string {
	daemon, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("no dbus-daemon")
	}
	dir := t.TempDir()
	conf := filepath.Join(dir, "bus.conf")
	sock := filepath.Join(dir, "bus")
	if err := os.WriteFile(conf, []byte(`<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig><type>session</type><listen>unix:path=`+sock+`</listen><auth>EXTERNAL</auth>
<policy context="default"><allow send_destination="*" eavesdrop="true"/><allow eavesdrop="true"/><allow own="*"/></policy></busconfig>`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(daemon, "--config-file="+conf, "--nofork", "--nopidfile")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			return "unix:path=" + sock
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("dbus-daemon did not start")
	return ""
}

func dial(t *testing.T, addr string) *dbus.Conn {
	conn, err := dbus.Connect(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestClientOnBus(t *testing.T) {
	addr := privateBus(t)
	srv := dial(t, addr)
	f := &fakeTukitd{conn: srv}
	if err := srv.Export(f, transactionPath, transactionIface); err != nil {
		t.Fatal(err)
	}
	if err := srv.Export(fakeSnapshots{}, snapshotPath, snapshotIface); err != nil {
		t.Fatal(err)
	}
	if reply, err := srv.RequestName(busName, dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("name: %v %v", reply, err)
	}
	c, err := NewClient(dial(t, addr))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	list, err := c.List(ctx, Columns)
	if err != nil || len(list) != 1 || list[0]["active"] != "yes" {
		t.Fatalf("list %v %v", list, err)
	}

	// No OpenWithOpts: the description is dropped, Open is used.
	id, err := c.Open(ctx, "default", "MCP: pkg install vim")
	if err != nil || id != "1" {
		t.Fatalf("open %q %v", id, err)
	}

	// A signal for the same snapshot from another peer is ignored.
	spoof := dial(t, addr)
	go func(id string) {
		time.Sleep(5 * time.Millisecond)
		_ = spoof.Emit(transactionPath, transactionIface+".CommandExecuted", id, int32(0), "spoofed")
	}(id)
	r, err := c.Call(ctx, id, []string{"zypper", "in", "it's"})
	if err != nil || r.Failed || r.ExitCode != 0 || r.Output != `ran 'zypper' 'in' 'it'\''s'` {
		t.Fatalf("call %+v %v", r, err)
	}
	if err := c.Close(ctx, id); err != nil {
		t.Fatal(err)
	}

	id, _ = c.Open(ctx, "active", "")
	r, err = c.Call(ctx, id, []string{"fail"})
	if err != nil || !r.Failed || r.Output != "mount failed" {
		t.Fatalf("call %+v %v", r, err)
	}
	if err := c.Abort(ctx, id); err != nil {
		t.Fatal(err)
	}

	// Errors of tukitd and the bus say what is wrong.
	if _, err := c.List(ctx, []string{"bad"}); err == nil || !strings.Contains(err.Error(), "tukit: Column list") {
		t.Fatalf("list error %v", err)
	}
	if err := c.Cleanup(ctx); err == nil {
		t.Fatal("Cleanup not refused by the fake")
	}
	if def, err := c.RollbackTo(ctx, "1"); err != nil || def != "1" {
		t.Fatalf("rollback %q %v", def, err)
	}

	f.mu.Lock()
	got := strings.Join(f.log, "\n")
	f.mu.Unlock()
	want := strings.Join([]string{"Open default", `Call 1 'zypper' 'in' 'it'\''s'`, "Close 1", "Open active", "Call 2 'fail'", "Abort 2"}, "\n")
	if got != want {
		t.Fatalf("tukitd saw:\n%s\nwant:\n%s", got, want)
	}

	// Without tukitd on the bus.
	srv.Close()
	time.Sleep(50 * time.Millisecond)
	if _, err := c.List(ctx, Columns); err == nil || !strings.Contains(err.Error(), "tukitd is not available") {
		t.Fatalf("list without tukitd: %v", err)
	}
}

// Lazy connects on first use, and tries again after a failure.
func TestLazy(t *testing.T) {
	addr := privateBus(t)
	fail := true
	l := NewLazy(func() (*Client, error) {
		if fail {
			return nil, errors.New("system bus: no such file or directory")
		}
		conn, err := dbus.Connect(addr)
		if err != nil {
			return nil, err
		}
		return NewClient(conn)
	})
	defer l.CloseBus()
	ctx := context.Background()
	if _, err := l.List(ctx, Columns); err == nil || !strings.Contains(err.Error(), "system bus") {
		t.Fatalf("list without a bus: %v", err)
	}
	fail = false
	if _, err := l.List(ctx, Columns); err == nil || !strings.Contains(err.Error(), "tukitd is not available") {
		t.Fatalf("list without tukitd: %v", err)
	}
}
