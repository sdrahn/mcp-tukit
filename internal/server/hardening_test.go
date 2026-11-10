package server

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sdrahn/mcp-tukit/internal/tukit"
)

func TestPrintable(t *testing.T) {
	for in, want := range map[string]string{
		"plain text, ünïcödé →":         "plain text, ünïcödé →",
		"two\nlines":                    `two\nlines`,
		"tab\tand\rreturn":              `tab\tand\rreturn`,
		"esc\x1b[31m bell\x07 nul\x00":  `esc\u001b[31m bell\u0007 nul\u0000`,
		"sep\u2028para\u2029next\u0085": `sep\u2028para\u2029next\u0085`,
		"bad\xff utf8":                  "bad\uFFFD utf8",
	} {
		if got := printable(in); got != want {
			t.Errorf("printable(%q) = %q, want %q", in, got, want)
		}
	}
	if got := shorten("abcdef", 6); got != "abcdef" {
		t.Errorf("shorten: %q", got)
	}
	// Cut at a rune boundary, within the bound, marked.
	got := shorten(strings.Repeat("ä", 100), 51)
	if len(got) > 51 || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Errorf("shorten: %d bytes %q", len(got), got)
	}
	if d := description(strings.Repeat("x", 1000)); len(d) != maxDescription || !strings.HasPrefix(d, "MCP: xxx") {
		t.Errorf("description: %d bytes", len(d))
	}
}

// What a caller sends does not shape the journal or the snapshot's
// description: a newline in a command word starts no line of its own,
// and a long command makes no long description.
func TestCallerTextIsContained(t *testing.T) {
	var log bytes.Buffer
	f := &fake{}
	s := New(Config{AllowCommands: true, Log: &log}, f)
	forged := "x\nmcp-server-tukit: reboot into 9: started"
	r := call(t, s, context.Background(), "run_in_snapshot", `{"command":["/bin/true","x\nmcp-server-tukit: reboot into 9: started","`+strings.Repeat("a", 3000)+`"]}`)
	if isError(r) {
		t.Fatalf("run_in_snapshot: %s", text(r))
	}
	for _, line := range strings.Split(strings.TrimSuffix(log.String(), "\n"), "\n") {
		if !strings.HasPrefix(line, "mcp-server-tukit: run ") {
			t.Errorf("a log line the caller started: %q", line)
		}
		if len(line) > maxLogLine+len("mcp-server-tukit: \n") {
			t.Errorf("a log line of %d bytes", len(line))
		}
	}
	if strings.Contains(log.String(), forged) {
		t.Error("the newline reached the log")
	}
	open := f.logged()[0]
	if strings.Contains(open, "\n") || len(open) > len("open 1 default ")+maxDescription {
		t.Errorf("snapshot description: %d bytes %q", len(open), open)
	}
	// The command itself goes to tukit as given.
	if got := f.logged()[1]; !strings.Contains(got, forged) {
		t.Errorf("command changed: %q", got)
	}

	for name, args := range map[string]string{
		"control characters": `{"command":["/bin/true"],"description":"a\nb"}`,
		"too long":           `{"command":["/bin/true"],"description":"` + strings.Repeat("d", 300) + `"}`,
	} {
		f := &fake{}
		if r := call(t, New(Config{AllowCommands: true}, f), context.Background(), "run_in_snapshot", args); !isError(r) || len(f.logged()) != 0 {
			t.Errorf("description with %s: %s, calls %q", name, text(r), f.logged())
		}
	}

	// Many packages: the description is cut, the command is not.
	f = &fake{}
	var pkgs []string
	for i := 0; i < 100; i++ {
		pkgs = append(pkgs, `"package-with-a-long-name-`+strings.Repeat("x", 20)+string(rune('a'+i%26))+string(rune('a'+i/26))+`"`)
	}
	call(t, New(Config{}, f), context.Background(), "pkg_install", `{"packages":[`+strings.Join(pkgs, ",")+`]}`)
	if open := f.logged()[0]; len(open) > len("open 1 default ")+maxDescription {
		t.Errorf("description of %d bytes", len(open))
	}
	if got := strings.Count(f.logged()[1], "package-with-a-long-name-"); got != 100 {
		t.Errorf("%d packages in the command", got)
	}
}

func TestRollbackRefusesSnapshotZero(t *testing.T) {
	f := &fake{}
	if r := call(t, New(Config{}, f), context.Background(), "rollback", `{"snapshot":"0"}`); !isError(r) || len(f.logged()) != 0 {
		t.Fatalf("rollback to 0: %s, calls %q", text(r), f.logged())
	}
}

// A call that waits for a running change ends with its context and does
// nothing, then or later: it neither blocks its place among the requests
// nor starts once the change has ended.
func TestWaitingCallEndsWithItsContext(t *testing.T) {
	for tool, args := range map[string]string{
		"rollback":          `{"snapshot":"3"}`,
		"delete_snapshot":   `{"snapshot":"3"}`,
		"cleanup_snapshots": `{}`,
		"pkg_remove":        `{"packages":["vim"]}`,
		"list_updates":      `{}`,
	} {
		t.Run(tool, func(t *testing.T) {
			f := &fake{block: make(chan struct{}), result: tukit.CallResult{ExitCode: 0}}
			s := New(Config{}, f)
			first := make(chan struct{})
			go func() { call(t, s, context.Background(), "pkg_install", `{"packages":["tree"]}`); close(first) }()
			waitFor(t, func() bool { return len(f.logged()) == 2 })
			ctx, cancel := context.WithCancel(context.Background())
			answered := make(chan map[string]any, 1)
			go func() { answered <- call(t, s, ctx, tool, args) }()
			select {
			case r := <-answered:
				t.Fatalf("%s did not wait: %s", tool, text(r))
			case <-time.After(50 * time.Millisecond):
			}
			cancel()
			select {
			case r := <-answered:
				if !isError(r) || !strings.Contains(text(r), "nothing was done") {
					t.Fatalf("%s: %s", tool, text(r))
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("%s still waits after its context ended", tool)
			}
			close(f.block)
			<-first
			// Only the first change's calls: open, call, close.
			time.Sleep(50 * time.Millisecond)
			if l := f.logged(); len(l) != 3 || l[2] != "close 1" {
				t.Fatalf("tukit calls after the change: %q", l)
			}
		})
	}
}

// The tools and their listing are built once.
func TestToolsBuiltOnce(t *testing.T) {
	s := New(Config{}, &fake{})
	a, _ := s.Handle(context.Background(), "tools/list", nil)
	b, _ := s.Handle(context.Background(), "tools/list", nil)
	la, lb := a.(map[string]any)["tools"], b.(map[string]any)["tools"]
	if &la.(json.RawMessage)[0] != &lb.(json.RawMessage)[0] {
		t.Error("tools/list was built again")
	}
	if s.byName["get_status"] == nil || s.byName["run_in_snapshot"] != nil {
		t.Errorf("tools %v", s.byName)
	}
}
