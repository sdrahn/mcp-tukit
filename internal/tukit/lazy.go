package tukit

import (
	"context"
	"sync"
)

// Lazy is a Backend that connects on first use and again after the
// connection was lost (the bus restarted), so that the server starts,
// and lists its tools, without the bus or tukitd; a call then fails with
// the reason.
type Lazy struct {
	dial func() (*Client, error)

	mu sync.Mutex
	c  *Client
}

var (
	_ Backend = (*Lazy)(nil)
	_ Backend = (*Client)(nil)
)

// NewLazy returns a Lazy that connects with dial (Connect: the system
// bus).
func NewLazy(dial func() (*Client, error)) *Lazy { return &Lazy{dial: dial} }

func (l *Lazy) client() (*Client, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.c != nil && l.c.conn.Connected() {
		return l.c, nil
	}
	if l.c != nil {
		_ = l.c.CloseBus()
		l.c = nil
	}
	c, err := l.dial()
	if err != nil {
		return nil, err
	}
	l.c = c
	return c, nil
}

// CloseBus closes the connection, if there is one.
func (l *Lazy) CloseBus() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.c != nil {
		_ = l.c.CloseBus()
		l.c = nil
	}
}

// with calls f on the connected client.
func with[T any](l *Lazy, f func(*Client) (T, error)) (T, error) {
	c, err := l.client()
	if err != nil {
		var zero T
		return zero, err
	}
	return f(c)
}

// do is with for calls that return only an error.
func (l *Lazy) do(f func(*Client) error) error {
	c, err := l.client()
	if err != nil {
		return err
	}
	return f(c)
}

func (l *Lazy) List(ctx context.Context, columns []string) ([]map[string]string, error) {
	return with(l, func(c *Client) ([]map[string]string, error) { return c.List(ctx, columns) })
}

func (l *Lazy) Open(ctx context.Context, base, description string) (string, error) {
	return with(l, func(c *Client) (string, error) { return c.Open(ctx, base, description) })
}

func (l *Lazy) Call(ctx context.Context, snapshot string, argv []string) (CallResult, error) {
	return with(l, func(c *Client) (CallResult, error) { return c.Call(ctx, snapshot, argv) })
}

func (l *Lazy) Close(ctx context.Context, snapshot string) error {
	return l.do(func(c *Client) error { return c.Close(ctx, snapshot) })
}

func (l *Lazy) Abort(ctx context.Context, snapshot string) error {
	return l.do(func(c *Client) error { return c.Abort(ctx, snapshot) })
}

func (l *Lazy) Delete(ctx context.Context, snapshot string) error {
	return l.do(func(c *Client) error { return c.Delete(ctx, snapshot) })
}

func (l *Lazy) Cleanup(ctx context.Context) error {
	return l.do(func(c *Client) error { return c.Cleanup(ctx) })
}

func (l *Lazy) RollbackTo(ctx context.Context, snapshot string) (string, error) {
	return with(l, func(c *Client) (string, error) { return c.RollbackTo(ctx, snapshot) })
}

func (l *Lazy) StartUnit(ctx context.Context, unit string) (string, error) {
	return with(l, func(c *Client) (string, error) { return c.StartUnit(ctx, unit) })
}

func (l *Lazy) RunUnit(ctx context.Context, unit string) (UnitRun, error) {
	return with(l, func(c *Client) (UnitRun, error) { return c.RunUnit(ctx, unit) })
}
