package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
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
	_ = inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		`{"jsonrpc":"2.0","method":"notifications/progress","params":{"message":"half way","progress":1,"progressToken":7}}`,
		`{"jsonrpc":"2.0","id":2,"result":"done"}`,
		`{"jsonrpc":"2.0","id":3,"result":"done"}`,
		`{"jsonrpc":"2.0","id":4,"error":{"code":-32601,"message":"method not found"}}`,
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"message too large"}}`,
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`,
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("missing %s in\n%s", want, got)
		}
	}
	if strings.Contains(got, "too late") || strings.Count(got, "notifications/progress") != 1 {
		t.Errorf("output:\n%s", got)
	}
}
