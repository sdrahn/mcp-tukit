package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The real server behind a stand-in for mcp-connect, without a system
// bus: listing works, a call is a tool error.
func TestRun(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "mcp-server-tukit")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/mcp-server-tukit").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	connect := filepath.Join(dir, "mcp-connect")
	script := "#!/bin/sh\n[ \"$1 $2\" = \"--server tukit\" ] || exit 9\nDBUS_SYSTEM_BUS_ADDRESS=unix:path=" + dir + "/nobus exec " + bin + " 2>/dev/null\n"
	if err := os.WriteFile(connect, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code, err := run(connect, "tukit", "tools/list", json.RawMessage(`{}`), time.Minute, &out)
	if code != 0 || err != nil || !strings.Contains(out.String(), `"name":"pkg_install"`) {
		t.Fatalf("tools/list: %d %v %s", code, err, out.String())
	}
	out.Reset()
	code, err = run(connect, "tukit", "tools/call", json.RawMessage(`{"name":"get_status","arguments":{}}`), time.Minute, &out)
	if code != 2 || err != nil || !strings.Contains(out.String(), "system bus") {
		t.Fatalf("get_status: %d %v %s", code, err, out.String())
	}
	out.Reset()
	if code, _ = run(connect, "tukit", "nosuch/method", json.RawMessage(`{}`), time.Minute, &out); code != 1 {
		t.Fatalf("unknown method: %d %s", code, out.String())
	}
	if code, _ = run(connect, "other", "tools/list", json.RawMessage(`{}`), time.Minute, &out); code != 1 {
		t.Fatalf("connect failing: %d", code)
	}
}
