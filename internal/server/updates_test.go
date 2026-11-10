package server

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sdrahn/mcp-tukit/internal/tukit"
)

// zypper --xmlout list-updates, as zypper 1.14 writes it (src/update.cc),
// after a line on stderr.
const packageXML = `Warning: something on stderr
<?xml version='1.0'?>
<stream>
<message type="info">Loading repository data...</message>
<message type="warning">Repository 'extra' is out-of-date.</message>
<update-status version="0.6">
<update-list>
<update kind="package" name="vim" edition="9.1.1-1.1" arch="x86_64" edition-old="9.0.2-2.1"><summary>Vi IMproved</summary><description>An editor.</description><license></license><source url="http://download.opensuse.org/tumbleweed/repo/oss" alias="repo-oss"/></update>
<update kind="package" name="zypper" edition="1.14.80-1.1" arch="x86_64" edition-old="1.14.79-1.1"><summary>Command line package manager</summary><source url="http://x" alias="repo-update"/></update>
</update-list>
</update-status>
</stream>
`

// zypper --xmlout list-patches with a patch kept back by a lock.
const patchXML = `<?xml version='1.0'?>
<stream>
<update-status version="0.6">
<update-list>
<update kind="patch" name="SUSE-2026-1234" edition="1" arch="noarch" status="needed" category="security" severity="important" pkgmanager="false" restart="true" interactive="false"><summary>Security update for the kernel</summary><source url="http://y" alias="SL-Micro-Updates"/><issue-date time_t="1792800000" text="2026-10-23"/><issue-list><issue type="cve" id="CVE-2026-0001"><title>x</title></issue></issue-list></update>
</update-list>
<blocked-update-list>
<update kind="patch" name="SUSE-2026-9" edition="1" arch="noarch" status="needed" category="recommended" severity="moderate" pkgmanager="false" restart="false" interactive="false"><summary>Locked</summary></update>
</blocked-update-list>
</update-status>
</stream>
`

func structured[T any](t *testing.T, r map[string]any) T {
	t.Helper()
	v, ok := r["structuredContent"].(T)
	if !ok {
		t.Fatalf("structuredContent %T: %v", r["structuredContent"], r)
	}
	return v
}

func TestListUpdates(t *testing.T) {
	f := &fake{result: tukit.CallResult{ExitCode: 0, Output: packageXML}}
	s := New(Config{}, f)
	r := call(t, s, context.Background(), "list_updates", `{}`)
	if isError(r) {
		t.Fatalf("list_updates: %s", text(r))
	}
	// A snapshot from the default, zypper in it, and the snapshot
	// discarded: nothing is kept.
	equal(t, f.logged(), []string{
		"open 1 default MCP: list package updates",
		"call 1 /usr/bin/zypper --non-interactive --xmlout list-updates",
		"abort 1",
	})
	l := structured[updateList](t, r)
	if l.Kind != "package" || l.Base != "default" || l.Count != 2 || len(l.Updates) != 2 {
		t.Fatalf("list %+v", l)
	}
	want := update{Kind: "package", Name: "vim", Version: "9.1.1-1.1", InstalledVersion: "9.0.2-2.1", Arch: "x86_64",
		Repository: "repo-oss", Summary: "Vi IMproved"}
	if l.Updates[0] != want {
		t.Errorf("vim: %+v", l.Updates[0])
	}
	if len(l.Messages) != 1 || l.Messages[0] != "Repository 'extra' is out-of-date." {
		t.Errorf("messages %q", l.Messages)
	}
	if got := text(r); !strings.Contains(got, "2 package updates") || !strings.Contains(got, "vim 9.0.2-2.1 → 9.1.1-1.1 [repo-oss]") ||
		!strings.Contains(got, "zypper: Repository 'extra' is out-of-date.") {
		t.Errorf("text:\n%s", got)
	}

	// Patches; exit 100 says patches are needed.
	f = &fake{result: tukit.CallResult{ExitCode: 100, Output: patchXML}}
	r = call(t, New(Config{}, f), context.Background(), "list_updates", `{"kind":"patch","base":"active"}`)
	if isError(r) {
		t.Fatalf("list_updates patch: %s", text(r))
	}
	equal(t, f.logged(), []string{
		"open 1 active MCP: list patch updates",
		"call 1 /usr/bin/zypper --non-interactive --xmlout list-patches",
		"abort 1",
	})
	l = structured[updateList](t, r)
	p := l.Updates[0]
	if l.Count != 1 || p.Kind != "patch" || p.Category != "security" || p.Severity != "important" || p.Status != "needed" ||
		!p.RebootSuggested || p.Interactive || p.Repository != "SL-Micro-Updates" {
		t.Fatalf("patch %+v", l)
	}
	if len(l.Blocked) != 1 || l.Blocked[0].Name != "SUSE-2026-9" {
		t.Errorf("blocked %+v", l.Blocked)
	}
	if got := text(r); !strings.Contains(got, "1 needed patches") || !strings.Contains(got, "SUSE-2026-1234 (security, important): Security update for the kernel") ||
		!strings.Contains(got, "Kept back by a package lock: 1") {
		t.Errorf("text:\n%s", got)
	}

	// Nothing to update: an empty list, not null.
	f = &fake{result: tukit.CallResult{Output: `<?xml version='1.0'?><stream><update-status version="0.6"><update-list></update-list></update-status></stream>`}}
	r = call(t, New(Config{}, f), context.Background(), "list_updates", `{}`)
	if l := structured[updateList](t, r); isError(r) || l.Count != 0 || l.Updates == nil || !strings.Contains(text(r), "No package updates") {
		t.Fatalf("empty: %+v %s", l, text(r))
	}
}

func TestListUpdatesErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		res  tukit.CallResult
		args string
		want string
	}{
		"zypper error": {tukit.CallResult{ExitCode: 4, Output: `<stream><message type="error">Repository 'x' is invalid.</message></stream>`}, `{}`,
			"no list from zypper: Repository 'x' is invalid."},
		"no XML":       {tukit.CallResult{ExitCode: 0, Output: "zypper: not found"}, `{}`, "zypper gave no XML"},
		"exit code":    {tukit.CallResult{ExitCode: 7, Output: patchXML}, `{"kind":"patch"}`, "zypper exited with 7"},
		"tukit failed": {tukit.CallResult{Failed: true, ExitCode: 1, Output: "cannot run"}, `{}`, "tukit: cannot run"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fake{result: tc.res}
			r := call(t, New(Config{}, f), context.Background(), "list_updates", tc.args)
			if !isError(r) || !strings.Contains(text(r), tc.want) {
				t.Fatalf("got %v: %s", isError(r), text(r))
			}
			// The snapshot is discarded whatever happened.
			if calls := f.logged(); calls[len(calls)-1] != "abort 1" {
				t.Errorf("tukit calls %q", calls)
			}
		})
	}
	r := call(t, New(Config{}, &fake{}), context.Background(), "list_updates", `{"kind":"product"}`)
	if !isError(r) {
		t.Errorf("kind product: %s", text(r))
	}
}

func TestRefreshRepositories(t *testing.T) {
	for _, tc := range []struct {
		args   string
		code   int
		status string
		isErr  bool
		argv   string
	}{
		{`{}`, 0, "refreshed", false, "/usr/bin/zypper --non-interactive refresh"},
		{`{"force":true}`, 0, "refreshed", false, "/usr/bin/zypper --non-interactive refresh --force"},
		{`{}`, 106, "partial", false, "/usr/bin/zypper --non-interactive refresh"},
		{`{}`, 4, "failed", true, "/usr/bin/zypper --non-interactive refresh"},
	} {
		f := &fake{result: tukit.CallResult{ExitCode: tc.code, Output: "Retrieving repository 'repo-oss' metadata ...done\n"}}
		r := call(t, New(Config{}, f), context.Background(), "refresh_repositories", tc.args)
		ref := structured[refresh](t, r)
		if ref.Status != tc.status || isError(r) != tc.isErr || ref.ExitCode == nil || *ref.ExitCode != tc.code {
			t.Errorf("%s exit %d: %+v, error %v: %s", tc.args, tc.code, ref, isError(r), text(r))
		}
		equal(t, f.logged(), []string{"open 1 default MCP: refresh", "call 1 " + tc.argv, "abort 1"})
	}
	// Arguments it does not know are refused.
	if r := call(t, New(Config{}, &fake{}), context.Background(), "refresh_repositories", `{"repos":["x"]}`); !isError(r) {
		t.Errorf("unknown argument: %s", text(r))
	}
}

// The new tools wait for a change of this instance, as changes do: one
// snapshot at a time.
func TestListUpdatesWaitsForChange(t *testing.T) {
	f := &fake{block: make(chan struct{}), result: tukit.CallResult{Output: packageXML}}
	s := New(Config{}, f)
	done := make(chan struct{})
	go func() {
		call(t, s, context.Background(), "pkg_install", `{"packages":["vim"]}`)
		close(done)
	}()
	for !strings.Contains(strings.Join(f.logged(), "\n"), "call 1") {
		runtime.Gosched()
	}
	listed := make(chan map[string]any)
	go func() { listed <- call(t, s, context.Background(), "list_updates", `{}`) }()
	select {
	case <-listed:
		t.Fatal("list_updates ran during a change")
	case <-time.After(100 * time.Millisecond):
	}
	close(f.block)
	<-done
	if r := <-listed; isError(r) {
		t.Fatalf("list_updates: %s", text(r))
	}
}

// limit bounds the updates returned; count stays the total.
func TestListUpdatesLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<stream><update-status version="0.6"><update-list>`)
	for i := 0; i < 700; i++ {
		fmt.Fprintf(&b, `<update kind="package" name="pkg%d" edition="2-1" arch="noarch" edition-old="1-1"/>`, i)
	}
	b.WriteString(`</update-list></update-status></stream>`)
	for _, tc := range []struct {
		args      string
		returned  int
		truncated bool
	}{
		{`{}`, defaultLimit, true},
		{`{"limit":3}`, 3, true},
		{`{"limit":700}`, 700, false},
		{`{"limit":5000}`, 700, false},
	} {
		f := &fake{result: tukit.CallResult{Output: b.String()}}
		r := call(t, New(Config{}, f), context.Background(), "list_updates", tc.args)
		l := structured[updateList](t, r)
		if isError(r) || l.Count != 700 || len(l.Updates) != tc.returned || l.Truncated != tc.truncated || l.Updates[0].Name != "pkg0" {
			t.Errorf("%s: count %d, %d returned, truncated %v: %.80s", tc.args, l.Count, len(l.Updates), l.Truncated, text(r))
		}
		if got := strings.Contains(text(r), "Only the first"); got != tc.truncated {
			t.Errorf("%s: text says truncated: %v", tc.args, got)
		}
	}
	for _, args := range []string{`{"limit":0}`, `{"limit":-1}`, `{"limit":5001}`, `{"limit":"all"}`} {
		f := &fake{}
		if r := call(t, New(Config{}, f), context.Background(), "list_updates", args); !isError(r) || len(f.logged()) != 0 {
			t.Errorf("%s: %s, calls %q", args, text(r), f.logged())
		}
	}
}
