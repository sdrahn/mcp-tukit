// Command tukitcall makes one MCP request to a server behind mcp-gateway,
// as the user running it, and prints the result as JSON. The functional
// tests (test/vm/run-tests.sh, package mcp-server-tukit-tests) use it to
// drive the installed gateway and server.
//
//	tukitcall --method tools/call --params '{"name":"get_status","arguments":{}}'
//
// It speaks MCP over the gateway's own stdio shim, mcp-connect (package
// mcp-gateway), which connects to the gateway's unix socket: the gateway
// identifies the user by the socket's peer credentials, as for any local
// client. initialize and notifications/initialized come first.
//
// It exits with 1 if the request fails (JSON-RPC error, or no answer)
// and with 2 if the result is a tool error (isError), printing the error
// or result.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func main() {
	connect := flag.String("connect", "/usr/bin/mcp-connect", "the gateway's stdio shim")
	server := flag.String("server", "tukit", "MCP server behind the gateway")
	method := flag.String("method", "tools/list", "request method")
	params := flag.String("params", "{}", "request params (JSON)")
	timeout := flag.Duration("timeout", 30*time.Minute, "overall timeout (an update can take long)")
	flag.Parse()
	code, err := run(*connect, *server, *method, json.RawMessage(*params), *timeout, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tukitcall:", err)
	}
	os.Exit(code)
}

func run(connect, server, method string, params json.RawMessage, timeout time.Duration, out io.Writer) (int, error) {
	if !json.Valid(params) {
		return 1, fmt.Errorf("params %s are not valid JSON", params)
	}
	cmd := exec.Command(connect, "--server", server)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 1, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 1, err
	}
	if err := cmd.Start(); err != nil {
		return 1, err
	}
	defer func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	type answer struct {
		m   message
		err error
	}
	answers := make(chan answer)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			var m message
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				answers <- answer{err: fmt.Errorf("invalid message %.200q: %v", sc.Text(), err)}
				return
			}
			if len(m.ID) > 0 && m.Method == "" {
				answers <- answer{m: m}
			}
			// Notifications (progress, logging) and requests from the
			// gateway are not answered: the tests need none.
		}
		err := sc.Err()
		if err == nil {
			err = errors.New("the gateway closed the connection")
		}
		answers <- answer{err: err}
	}()
	deadline := time.After(timeout)
	send := func(m message) error {
		m.JSONRPC = "2.0"
		b, _ := json.Marshal(m)
		_, err := stdin.Write(append(b, '\n'))
		return err
	}
	wait := func(id string) (message, error) {
		for {
			select {
			case a := <-answers:
				if a.err != nil {
					return message{}, a.err
				}
				if string(a.m.ID) == id {
					return a.m, nil
				}
			case <-deadline:
				return message{}, fmt.Errorf("no answer within %s", timeout)
			}
		}
	}

	init := json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"tukitcall","version":"1"}}`)
	if err := send(message{ID: json.RawMessage("1"), Method: "initialize", Params: init}); err != nil {
		return 1, err
	}
	m, err := wait("1")
	if err != nil {
		return 1, fmt.Errorf("initialize: %w", err)
	}
	if m.Error != nil {
		return 1, fmt.Errorf("initialize: %d %s", m.Error.Code, m.Error.Message)
	}
	if err := send(message{Method: "notifications/initialized"}); err != nil {
		return 1, err
	}
	if err := send(message{ID: json.RawMessage("2"), Method: method, Params: params}); err != nil {
		return 1, err
	}
	m, err = wait("2")
	if err != nil {
		return 1, fmt.Errorf("%s: %w", method, err)
	}
	if m.Error != nil {
		b, _ := json.Marshal(m.Error)
		_, _ = fmt.Fprintln(out, string(b))
		return 1, nil
	}
	_, _ = fmt.Fprintln(out, string(m.Result))
	var r struct {
		IsError bool `json:"isError"`
	}
	if json.Unmarshal(m.Result, &r) == nil && r.IsError {
		return 2, nil
	}
	return 0, nil
}
