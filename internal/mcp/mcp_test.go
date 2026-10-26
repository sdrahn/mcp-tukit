package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe for the server's writers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestServe(t *testing.T) {
	started := make(chan struct{})
	h := func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		switch method {
		case "slow":
			close(started)
			<-ctx.Done()
			return "too late", nil
		case "progress":
			Progress(ctx, "half way")
			return "done", nil
		}
		return nil, &Error{Code: CodeMethodNotFound, Message: "method not found"}
	}
	inR, inW := io.Pipe()
	var out syncBuffer
	done := make(chan error)
	go func() { done <- New(h, &out).Serve(inR, 200) }()
	write := func(s string) { _, _ = io.WriteString(inW, s+"\n") }
	write(`{"jsonrpc":"2.0","id":1,"method":"slow"}`)
	<-started
	write(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	write(`{"jsonrpc":"2.0","id":2,"method":"progress","params":{"_meta":{"progressToken":7}}}`)
	write(`{"jsonrpc":"2.0","id":3,"method":"progress"}`)
	write(`{"jsonrpc":"2.0","id":4,"method":"nosuch"}`)
	write(`{"jsonrpc":"2.0","id":5,"method":"x","params":"` + strings.Repeat("a", 300) + `"}`)
	write(`not json`)
	want := []string{
		`{"jsonrpc":"2.0","method":"notifications/progress","params":{"message":"half way","progress":1,"progressToken":7}}`,
		`{"jsonrpc":"2.0","id":2,"result":"done"}`,
		`{"jsonrpc":"2.0","id":3,"result":"done"}`,
		`{"jsonrpc":"2.0","id":4,"error":{"code":-32601,"message":"method not found"}}`,
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"message too large"}}`,
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`,
	}
	// The end of input cancels what is still running: wait for the
	// answers first.
	all := func() bool {
		for _, w := range want {
			if !strings.Contains(out.String(), w+"\n") {
				return false
			}
		}
		return true
	}
	for i := 0; i < 500 && !all(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	_ = inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, w := range want {
		if !strings.Contains(got, w+"\n") {
			t.Errorf("missing %s in\n%s", w, got)
		}
	}
	if strings.Contains(got, "too late") || strings.Count(got, "notifications/progress") != 1 {
		t.Errorf("output:\n%s", got)
	}
}

// Modern requests (MCP 2026-07-28) carry their version in _meta: answered
// with resultType, refused for a version not spoken, and never asked to
// initialize; legacy requests are answered as before.
func TestModern(t *testing.T) {
	h := func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		switch method {
		case "who":
			r := RequestOf(ctx)
			return map[string]any{"version": r.Version, "client": r.Client}, nil
		case "progress":
			Progress(ctx, "step")
			return map[string]any{"ok": true}, nil
		case "marked":
			return map[string]any{"resultType": "input_required"}, nil
		case "initialize":
			return map[string]any{"protocolVersion": Negotiate("")}, nil
		}
		return nil, &Error{Code: CodeMethodNotFound, Message: "method not found"}
	}
	const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"agent"},"io.modelcontextprotocol/clientCapabilities":{}}`
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"who","params":{` + meta + `}}`,
		`{"jsonrpc":"2.0","id":2,"method":"progress","params":{"_meta":{"progressToken":"p","io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"marked","params":{` + meta + `}}`,
		`{"jsonrpc":"2.0","id":4,"method":"who","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2027-01-01"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"initialize","params":{` + meta + `}}`,
		`{"jsonrpc":"2.0","id":6,"method":"who"}`,
		`{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
	}, "\n") + "\n"
	// The input stays open until every answer is out: its end cancels
	// what still runs.
	inR, inW := io.Pipe()
	var out syncBuffer
	done := make(chan error)
	go func() { done <- New(h, &out).Serve(inR, 1<<20) }()
	_, _ = io.WriteString(inW, in)
	for deadline := time.Now().Add(5 * time.Second); strings.Count(out.String(), "\n") < 8 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	_ = inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		key := string(m.ID)
		if m.Method != "" {
			key = m.Method
		}
		got[key] = line
	}
	for key, want := range map[string]string{
		"1":                      `{"jsonrpc":"2.0","id":1,"result":{"client":"agent","resultType":"complete","version":"2026-07-28"}}`,
		"notifications/progress": `{"jsonrpc":"2.0","method":"notifications/progress","params":{"message":"step","progress":1,"progressToken":"p"}}`,
		"2":                      `{"jsonrpc":"2.0","id":2,"result":{"ok":true,"resultType":"complete"}}`,
		"3":                      `{"jsonrpc":"2.0","id":3,"result":{"resultType":"input_required"}}`,
		"4":                      `{"jsonrpc":"2.0","id":4,"error":{"code":-32022,"message":"Unsupported protocol version","data":{"requested":"2027-01-01","supported":["2026-07-28"]}}}`,
		"5":                      `{"jsonrpc":"2.0","id":5,"error":{"code":-32601,"message":"not in MCP 2026-07-28"}}`,
		"6":                      `{"jsonrpc":"2.0","id":6,"result":{"client":"","version":""}}`,
		"7":                      `{"jsonrpc":"2.0","id":7,"result":{"protocolVersion":"2025-11-25"}}`,
	} {
		if got[key] != want {
			t.Errorf("%s:\n got %s\nwant %s", key, got[key], want)
		}
	}
}
