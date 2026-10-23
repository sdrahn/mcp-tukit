package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sdrahn/mcp-tukit/internal/tukit"
)

// fake is a tukit.Backend that records what is asked of it.
type fake struct {
	mu       sync.Mutex
	list     []map[string]string
	next     int
	calls    []string // "open 5 default", "call 5 ...", "close 5", "abort 5", ...
	result   tukit.CallResult
	callErr  error
	openErr  error
	block    chan struct{} // Call waits for it, if set
	rollback string
}

func (f *fake) log(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fake) logged() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fake) List(ctx context.Context, columns []string) ([]map[string]string, error) {
	return f.list, nil
}

func (f *fake) Open(ctx context.Context, base, description string) (string, error) {
	if f.openErr != nil {
		return "", f.openErr
	}
	f.mu.Lock()
	f.next++
	id := strings.TrimSpace(string(rune('0' + f.next)))
	f.mu.Unlock()
	f.log("open " + id + " " + base + " " + description)
	return id, nil
}

func (f *fake) Call(ctx context.Context, snapshot string, argv []string) (tukit.CallResult, error) {
	f.log("call " + snapshot + " " + strings.Join(argv, " "))
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return tukit.CallResult{}, ctx.Err()
		}
	}
	return f.result, f.callErr
}

func (f *fake) Close(ctx context.Context, snapshot string) error {
	f.log("close " + snapshot)
	return nil
}
func (f *fake) Abort(ctx context.Context, snapshot string) error {
	f.log("abort " + snapshot)
	return nil
}
func (f *fake) Delete(ctx context.Context, snapshot string) error {
	f.log("delete " + snapshot)
	return nil
}
func (f *fake) Cleanup(ctx context.Context) error { f.log("cleanup"); return nil }
func (f *fake) RollbackTo(ctx context.Context, snapshot string) (string, error) {
	f.log("rollback " + snapshot)
	if f.rollback != "" {
		return f.rollback, nil
	}
	return snapshot, nil
}

func call(t *testing.T, s *Server, ctx context.Context, tool string, args string) map[string]any {
	t.Helper()
	res, err := s.Handle(ctx, "tools/call", json.RawMessage(`{"name":"`+tool+`","arguments":`+args+`}`))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res.(map[string]any)
}

func text(r map[string]any) string {
	return r["content"].([]map[string]any)[0]["text"].(string)
}

func isError(r map[string]any) bool { b, _ := r["isError"].(bool); return b }

func equal(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("tukit calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestInstallApplied(t *testing.T) {
	f := &fake{result: tukit.CallResult{ExitCode: 0, Output: "Installing: vim\n"}}
	s := New(Config{}, f)
	r := call(t, s, context.Background(), "pkg_install", `{"packages":["vim","pattern:base","kernel-default>=6.4"],"no_recommends":true}`)
	if isError(r) {
		t.Fatal(text(r))
	}
	equal(t, f.logged(), []string{
		"open 1 default MCP: pkg install vim pattern:base kernel-default>=6.4",
		"call 1 /usr/bin/zypper --non-interactive install --no-recommends vim pattern:base kernel-default>=6.4",
		"close 1",
	})
	c := r["structuredContent"].(change)
	if c.Status != "applied" || !c.RebootRequired || c.Snapshot != "1" {
		t.Fatalf("result %+v", c)
	}
	if !strings.Contains(text(r), "next reboot") {
		t.Fatalf("text %q", text(r))
	}
}

func TestNothingToDoDiscards(t *testing.T) {
	f := &fake{result: tukit.CallResult{ExitCode: 0, Output: "Loading repository data...\nNothing to do.\n"}}
	s := New(Config{}, f)
	r := call(t, s, context.Background(), "update_system", `{"mode":"dist-upgrade","base":"active"}`)
	if isError(r) || r["structuredContent"].(change).Status != "no_changes" {
		t.Fatalf("result %v", r)
	}
	equal(t, f.logged(), []string{"open 1 active MCP: dup", "call 1 /usr/bin/zypper --non-interactive dist-upgrade", "abort 1"})
}

func TestFailureDiscards(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result tukit.CallResult
		err    error
	}{
		{"exit 104", tukit.CallResult{ExitCode: 104, Output: "No provider of 'nosuch' found."}, nil},
		{"tukit error", tukit.CallResult{ExitCode: -1, Output: "cannot mount", Failed: true}, nil},
		{"bus error", tukit.CallResult{}, errors.New("connection lost")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fake{result: tc.result, callErr: tc.err}
			r := call(t, New(Config{}, f), context.Background(), "pkg_remove", `{"packages":["nosuch"],"clean_deps":true}`)
			if !isError(r) || r["structuredContent"].(change).Status != "failed" {
				t.Fatalf("result %v", r)
			}
			equal(t, f.logged(), []string{"open 1 default MCP: pkg remove nosuch",
				"call 1 /usr/bin/zypper --non-interactive remove --clean-deps nosuch", "abort 1"})
		})
	}
}

func TestRebootNeededIsSuccess(t *testing.T) {
	f := &fake{result: tukit.CallResult{ExitCode: 102, Output: "kernel updated"}}
	r := call(t, New(Config{}, f), context.Background(), "update_system", `{}`)
	if isError(r) || r["structuredContent"].(change).Status != "applied" {
		t.Fatalf("result %v", r)
	}
}

func TestOpenFails(t *testing.T) {
	f := &fake{openErr: errors.New("tukit: snapshot 7 does not exist")}
	r := call(t, New(Config{}, f), context.Background(), "pkg_install", `{"packages":["vim"],"base":"7"}`)
	if !isError(r) || !strings.Contains(text(r), "does not exist") {
		t.Fatalf("result %v", r)
	}
	if len(f.logged()) != 0 {
		t.Fatalf("calls %v", f.logged())
	}
}

func TestRefusedArguments(t *testing.T) {
	for _, args := range []string{
		`{"packages":[]}`,
		`{"packages":["$(reboot)"]}`,
		"{\"packages\":[\"`reboot`\"]}",
		`{"packages":["vim; reboot"]}`,
		`{"packages":["vim reboot"]}`,
		`{"packages":["-y"]}`,
		`{"packages":["--from=evil"]}`,
		`{"packages":["/tmp/evil.rpm"]}`,
		`{"packages":["http://example.com/evil.rpm"]}`,
		`{"packages":["vim'"]}`,
		`{"packages":["vim"],"base":"1 2"}`,
		`{"packages":["vim"],"base":"-1"}`,
		`{"packages":["vim"],"repo":"x"}`,
		`{"packages":"vim"}`,
	} {
		f := &fake{}
		r := call(t, New(Config{}, f), context.Background(), "pkg_install", args)
		if !isError(r) {
			t.Errorf("%s: accepted", args)
		}
		if len(f.logged()) != 0 {
			t.Errorf("%s: tukit called: %v", args, f.logged())
		}
	}
}

func TestRunInSnapshotOnlyWhenAllowed(t *testing.T) {
	f := &fake{result: tukit.CallResult{ExitCode: 0}}
	s := New(Config{}, f)
	if _, err := s.Handle(context.Background(), "tools/call", json.RawMessage(`{"name":"run_in_snapshot","arguments":{"command":["true"]}}`)); err == nil {
		t.Fatal("run_in_snapshot offered without AllowCommands")
	}
	s = New(Config{AllowCommands: true}, f)
	r := call(t, s, context.Background(), "run_in_snapshot", `{"command":["/bin/sh","-c","echo $(id) > /etc/x"]}`)
	if isError(r) {
		t.Fatal(text(r))
	}
	equal(t, f.logged(), []string{"open 1 default MCP: run /bin/sh -c echo $(id) > /etc/x",
		"call 1 /bin/sh -c echo $(id) > /etc/x", "close 1"})
	f.result.ExitCode = 1
	if r := call(t, s, context.Background(), "run_in_snapshot", `{"command":["false"]}`); !isError(r) {
		t.Fatalf("result %v", r)
	}
	if got := f.logged(); got[len(got)-1] != "abort 2" {
		t.Fatalf("calls %v", got)
	}
}

// A cancelled call is not answered, but the change is finished: the
// snapshot is closed once the command ends.
func TestCancelledChangeFinishes(t *testing.T) {
	f := &fake{result: tukit.CallResult{ExitCode: 0}, block: make(chan struct{})}
	s := New(Config{}, f)
	ctx, cancel := context.WithCancel(context.Background())
	answered := make(chan map[string]any)
	go func() { answered <- call(t, s, ctx, "pkg_install", `{"packages":["vim"]}`) }()
	waitFor(t, func() bool { return len(f.logged()) == 2 })
	cancel()
	if r := <-answered; r["structuredContent"].(change).Status != "running" {
		t.Fatalf("result %v", r)
	}
	close(f.block)
	waitFor(t, func() bool { l := f.logged(); return len(l) == 3 && l[2] == "close 1" })
}

func TestTimeoutLeavesSnapshotOpen(t *testing.T) {
	f := &fake{block: make(chan struct{})}
	s := New(Config{Timeout: 50 * time.Millisecond}, f)
	r := call(t, s, context.Background(), "pkg_install", `{"packages":["vim"]}`)
	c := r["structuredContent"].(change)
	if !isError(r) || c.Status != "running" || !strings.Contains(c.Error, "stays open") {
		t.Fatalf("result %v", r)
	}
	if l := f.logged(); len(l) != 2 {
		t.Fatalf("calls %v", l)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

var snaps = []map[string]string{
	{"number": "0", "active": "no", "default": "no", "description": "current"},
	{"number": "10", "active": "no", "default": "yes", "date": "2026-10-23T08:00:00", "description": "MCP: pkg install vim"},
	{"number": "9", "active": "yes", "default": "no", "date": "2026-10-22T08:00:00", "description": "Snapshot Update of #8"},
	{"number": "2", "active": "no", "default": "no", "date": "2026-10-01T08:00:00", "description": "first root filesystem"},
}

func TestStatusAndList(t *testing.T) {
	f := &fake{list: snaps}
	s := New(Config{}, f)
	r := call(t, s, context.Background(), "get_status", `{}`)
	st := r["structuredContent"].(map[string]any)
	if st["active"] != "9" || st["default"] != "10" || st["reboot_pending"] != true || st["snapshots"] != 3 {
		t.Fatalf("status %v", st)
	}
	r = call(t, s, context.Background(), "list_snapshots", `{"limit":2}`)
	l := r["structuredContent"].(map[string]any)
	got := l["snapshots"].([]snapshot)
	if len(got) != 2 || got[0].Number != "9" || got[1].Number != "10" || l["total"] != 3 {
		t.Fatalf("list %v", l)
	}
	if !strings.Contains(text(r), "10\t2026-10-23T08:00:00\tMCP: pkg install vim [default: boots next]") {
		t.Fatalf("text %q", text(r))
	}
}

func TestDeleteRefusesActiveAndDefault(t *testing.T) {
	f := &fake{list: snaps}
	s := New(Config{}, f)
	for _, n := range []string{"9", "10"} {
		if r := call(t, s, context.Background(), "delete_snapshot", `{"snapshot":"`+n+`"}`); !isError(r) {
			t.Fatalf("deleted %s", n)
		}
	}
	if r := call(t, s, context.Background(), "delete_snapshot", `{"snapshot":"2"}`); isError(r) {
		t.Fatal(text(r))
	}
	if r := call(t, s, context.Background(), "delete_snapshot", `{"snapshot":"5"}`); isError(r) {
		t.Fatal(text(r))
	}
	equal(t, f.logged(), []string{"delete 2"})
}

func TestRollback(t *testing.T) {
	f := &fake{rollback: "11"}
	r := call(t, New(Config{}, f), context.Background(), "rollback", `{"snapshot":"9"}`)
	if isError(r) || !strings.Contains(text(r), "snapshot 11") {
		t.Fatalf("result %v", r)
	}
	if r := call(t, New(Config{}, f), context.Background(), "rollback", `{"snapshot":"default"}`); !isError(r) {
		t.Fatal("rollback to a name")
	}
}

func TestToolList(t *testing.T) {
	res, _ := New(Config{}, &fake{}).Handle(context.Background(), "tools/list", nil)
	tools := res.(map[string]any)["tools"].([]map[string]any)
	byName := map[string]map[string]any{}
	for _, tl := range tools {
		byName[tl["name"].(string)] = tl
		// The schemas must be valid JSON.
		if _, err := json.Marshal(tl); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := byName["run_in_snapshot"]; ok {
		t.Fatal("run_in_snapshot listed")
	}
	for name, ro := range map[string]bool{"get_status": true, "list_snapshots": true, "pkg_install": false,
		"pkg_remove": false, "update_system": false, "rollback": false, "delete_snapshot": false, "cleanup_snapshots": false} {
		a := byName[name]["annotations"].(map[string]any)
		if a["readOnlyHint"] != ro || (!ro && a["destructiveHint"] != true) {
			t.Errorf("%s: annotations %v", name, a)
		}
	}
}

func TestClip(t *testing.T) {
	out := strings.Repeat("a", 100) + strings.Repeat("z", 100)
	got, cut := clip(out, 80)
	if !cut || !strings.HasPrefix(got, "aaaaaaaaaa\n[…]\n") || !strings.HasSuffix(got, strings.Repeat("z", 70)) {
		t.Fatalf("clip: %q", got)
	}
}
