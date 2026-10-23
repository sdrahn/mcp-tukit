package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sdrahn/mcp-tukit/internal/tukit"
)

type slowTukit struct{ tukit.Backend }

func (slowTukit) List(ctx context.Context, columns []string) ([]map[string]string, error) {
	return []map[string]string{{"number": "3", "active": "yes", "default": "yes", "description": "first"}}, nil
}
func (slowTukit) Open(ctx context.Context, base, description string) (string, error) { return "4", nil }
func (slowTukit) Call(ctx context.Context, snapshot string, argv []string) (tukit.CallResult, error) {
	time.Sleep(50 * time.Millisecond)
	return tukit.CallResult{Output: "installed " + argv[len(argv)-1]}, nil
}
func (slowTukit) Close(ctx context.Context, snapshot string) error { return nil }

func TestStdio(t *testing.T) {
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"pkg_install","arguments":{"packages":["vim"]},"_meta":{"progressToken":"p1"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nosuch"}}`,
	}, "\n") + "\n"
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var errOut bytes.Buffer
	connect := func() (tukit.Backend, func(), error) { return slowTukit{}, func() {}, nil }
	exit := make(chan int, 1)
	go func() { exit <- run(nil, inR, outW, &errOut, connect); _ = outW.Close() }()
	go func() { _, _ = io.WriteString(inW, in) }()
	answers := map[string]map[string]any{}
	progress := 0
	sc := bufio.NewScanner(outR)
	for len(answers) < 4 && sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("%q: %v", sc.Text(), err)
		}
		if m["method"] == "notifications/progress" {
			progress++
			continue
		}
		id, _ := json.Marshal(m["id"])
		answers[string(id)] = m
	}
	_ = inW.Close()
	go func() { _, _ = io.Copy(io.Discard, outR) }()
	if code := <-exit; code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if v := answers["1"]["result"].(map[string]any)["protocolVersion"]; v != "2025-06-18" {
		t.Errorf("protocol version %v", v)
	}
	if n := len(answers["2"]["result"].(map[string]any)["tools"].([]any)); n != 9 {
		t.Errorf("%d tools", n)
	}
	res := answers["3"]["result"].(map[string]any)
	if res["isError"] != false || res["structuredContent"].(map[string]any)["status"] != "applied" {
		t.Errorf("pkg_install: %v", res)
	}
	if progress == 0 {
		t.Error("no progress notification")
	}
	if answers["4"]["error"] == nil {
		t.Errorf("unknown tool: %v", answers["4"])
	}
	if !strings.Contains(errOut.String(), "pkg install vim: snapshot 4: applied") {
		t.Errorf("log: %s", errOut.String())
	}
}

func TestFlags(t *testing.T) {
	never := func() (tukit.Backend, func(), error) { t.Fatal("connected"); return nil, nil, nil }
	for _, args := range [][]string{{"-base", "latest"}, {"-timeout", "1s"}, {"extra"}, {"-reboot-unit", "reboot.target"}, {"-reboot-unit", "../x.service"}} {
		if code := run(args, strings.NewReader(""), io.Discard, io.Discard, never); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	var out bytes.Buffer
	if code := run([]string{"-version"}, nil, &out, io.Discard, never); code != 0 || !strings.HasPrefix(out.String(), "mcp-server-tukit ") {
		t.Errorf("-version: %d %q", code, out.String())
	}
}
