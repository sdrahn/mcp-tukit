// Package mcp is the protocol core of mcp-server-tukit: newline-delimited
// JSON-RPC messages on a stream (MCP over stdio), requests handled
// concurrently, each with a context that a notifications/cancelled from
// the client ends, and progress notifications for requests that asked
// for them. What a request does is the Handler's.
//
// It speaks both eras of MCP. Legacy clients (MCP 2025-11-25 and
// earlier) begin with initialize. Modern clients (MCP 2026-07-28) send no
// initialize: they ask server/discover, and every request carries its
// protocol version in _meta ("io.modelcontextprotocol/protocolVersion");
// such a request is answered under that version, with resultType in its
// result, and one naming a version the server does not speak is refused
// (CodeUnsupportedVersion). Neither era keeps state per connection here.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sync"
)

// ProtocolVersions are the legacy (initialize-based) protocol versions
// the server speaks, newest first.
var ProtocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// ModernVersions are the modern protocol versions the server speaks, as
// server/discover lists them.
var ModernVersions = []string{"2026-07-28"}

// Reserved _meta keys of the modern per-request protocol fields.
const (
	MetaProtocolVersion = "io.modelcontextprotocol/protocolVersion"
	MetaClientInfo      = "io.modelcontextprotocol/clientInfo"
	MetaServerInfo      = "io.modelcontextprotocol/serverInfo"
)

// Request describes the request a context belongs to.
type Request struct {
	// Version is the modern protocol version of the request, empty for a
	// legacy request.
	Version string
	// Client is the client's name from a modern request's _meta.
	Client string
}

type requestKey struct{}

// RequestOf returns what is known of the request of ctx.
func RequestOf(ctx context.Context) Request {
	r, _ := ctx.Value(requestKey{}).(Request)
	return r
}

// requestMeta reads the modern protocol fields of a request's _meta.
func requestMeta(params json.RawMessage) Request {
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	var r Request
	if json.Unmarshal(params, &p) != nil {
		return r
	}
	_ = json.Unmarshal(p.Meta[MetaProtocolVersion], &r.Version)
	var client struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(p.Meta[MetaClientInfo], &client) == nil {
		r.Client = client.Name
	}
	return r
}

// legacyOnly are the methods modern clients do not send.
var legacyOnly = map[string]bool{"initialize": true, "ping": true}

// Negotiate returns the version to answer an initialize request asking
// for requested: that one if spoken, else the newest (the client then
// decides whether to go on).
func Negotiate(requested string) string {
	for _, v := range ProtocolVersions {
		if v == requested {
			return v
		}
	}
	return ProtocolVersions[0]
}

// MaxConcurrent bounds the requests handled at once; more wait.
const MaxConcurrent = 8

// Error is a JSON-RPC error. A Handler returning one has it sent as is;
// any other error is sent as an internal error.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// JSON-RPC error codes.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	// CodeUnsupportedVersion refuses a modern request naming a protocol
	// version the server does not speak (MCP 2026-07-28).
	CodeUnsupportedVersion = -32022
)

// Handler answers one request. ctx ends when the client cancels it or the
// connection ends; Progress(ctx, …) reports progress on it.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type inMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Server answers MCP requests read from a stream; the answer to a
// cancelled request is not sent.
type Server struct {
	handle Handler

	mu       sync.Mutex // guards out, inflight
	out      *json.Encoder
	inflight map[string]context.CancelFunc

	slots chan struct{}
	wg    sync.WaitGroup
}

// New returns a server that writes its answers to w.
func New(h Handler, w io.Writer) *Server {
	return &Server{handle: h, out: json.NewEncoder(w), inflight: map[string]context.CancelFunc{},
		slots: make(chan struct{}, MaxConcurrent)}
}

// Serve reads messages from r until it ends, then cancels what is still
// running and waits for it. maxMessage bounds a message's size; a longer
// line is answered with an error and skipped.
func (s *Server) Serve(r io.Reader, maxMessage int) error {
	in := bufio.NewReaderSize(r, 64<<10)
	for {
		line, tooLong, err := readLine(in, maxMessage)
		if len(bytes.TrimSpace(line)) > 0 || tooLong {
			if tooLong {
				s.send(message{ID: json.RawMessage("null"), Error: &Error{Code: CodeInvalidRequest, Message: "message too large"}})
			} else {
				s.dispatch(line)
			}
		}
		if err != nil {
			s.mu.Lock()
			for _, cancel := range s.inflight {
				cancel()
			}
			s.mu.Unlock()
			s.wg.Wait()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// readLine returns the next line without its newline; a line longer than
// max is consumed and reported as too long.
func readLine(r *bufio.Reader, max int) ([]byte, bool, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > max+1 {
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = r.ReadSlice('\n')
			}
			return nil, true, err
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimSuffix(line, []byte("\n")), false, err
	}
}

type progressKey struct{}

type progress struct {
	s     *Server
	token json.RawMessage
	mu    sync.Mutex
	n     int
}

// Progress sends a notifications/progress for the request of ctx, if its
// client asked for progress (_meta.progressToken); otherwise it does
// nothing. Each call counts one step.
func Progress(ctx context.Context, msg string) {
	p, _ := ctx.Value(progressKey{}).(*progress)
	if p == nil || ctx.Err() != nil {
		return
	}
	p.mu.Lock()
	p.n++
	n := p.n
	p.mu.Unlock()
	p.s.send(message{Method: "notifications/progress", Params: map[string]any{
		"progressToken": p.token, "progress": n, "message": msg}})
}

func progressToken(params json.RawMessage) json.RawMessage {
	var p struct {
		Meta struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if json.Unmarshal(params, &p) != nil || len(p.Meta.ProgressToken) == 0 || string(p.Meta.ProgressToken) == "null" {
		return nil
	}
	return p.Meta.ProgressToken
}

func (s *Server) dispatch(line []byte) {
	var m inMessage
	if err := json.Unmarshal(line, &m); err != nil {
		s.send(message{ID: json.RawMessage("null"), Error: &Error{Code: CodeParse, Message: "parse error"}})
		return
	}
	switch {
	case m.Method == "" && len(m.ID) > 0:
		return // a response; this server sends no requests
	case m.Method == "":
		s.send(message{ID: json.RawMessage("null"), Error: &Error{Code: CodeInvalidRequest, Message: "invalid request"}})
		return
	case len(m.ID) == 0:
		s.notification(m)
		return
	}
	req := requestMeta(m.Params)
	if req.Version != "" {
		switch {
		case !slices.Contains(ModernVersions, req.Version):
			s.send(message{ID: m.ID, Error: &Error{Code: CodeUnsupportedVersion, Message: "Unsupported protocol version",
				Data: map[string]any{"supported": ModernVersions, "requested": req.Version}}})
			return
		case legacyOnly[m.Method]:
			s.send(message{ID: m.ID, Error: &Error{Code: CodeMethodNotFound, Message: "not in MCP " + req.Version}})
			return
		}
	}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), requestKey{}, req))
	if tok := progressToken(m.Params); tok != nil {
		ctx = context.WithValue(ctx, progressKey{}, &progress{s: s, token: tok})
	}
	key := string(m.ID)
	s.mu.Lock()
	s.inflight[key] = cancel
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.slots <- struct{}{}
		defer func() { <-s.slots }()
		res, err := s.handle(ctx, m.Method, m.Params)
		s.mu.Lock()
		delete(s.inflight, key)
		s.mu.Unlock()
		cancelled := ctx.Err() != nil
		cancel()
		if cancelled {
			return
		}
		resp := message{ID: m.ID}
		var rerr *Error
		switch {
		case errors.As(err, &rerr):
			resp.Error = rerr
		case err != nil:
			resp.Error = &Error{Code: CodeInternal, Message: err.Error()}
		case req.Version != "":
			resp.Result = complete(res)
		default:
			resp.Result = res
		}
		s.send(resp)
	}()
}

// complete marks a modern request's result as final (resultType
// "complete"; this server never asks the client for input), unless it is
// marked already or is not an object.
func complete(res any) any {
	raw, err := json.Marshal(res)
	if err != nil {
		return res
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return res
	}
	if _, ok := obj["resultType"]; !ok {
		obj["resultType"] = json.RawMessage(`"complete"`)
	}
	return obj
}

func (s *Server) notification(m inMessage) {
	if m.Method != "notifications/cancelled" {
		return // initialized, roots/list_changed, ...: nothing to do
	}
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(m.Params, &p) != nil {
		return
	}
	s.mu.Lock()
	cancel := s.inflight[string(p.RequestID)]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Server) send(m message) {
	m.JSONRPC = "2.0"
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.out.Encode(m)
}
