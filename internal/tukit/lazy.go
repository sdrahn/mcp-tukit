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

func (l *Lazy) List(ctx context.Context, columns []string) ([]map[string]string, error) {
	c, err := l.client()
	if err != nil {
		return nil, err
	}
	return c.List(ctx, columns)
}

func (l *Lazy) Open(ctx context.Context, base, description string) (string, error) {
	c, err := l.client()
	if err != nil {
		return "", err
	}
	return c.Open(ctx, base, description)
}

func (l *Lazy) Call(ctx context.Context, snapshot string, argv []string) (CallResult, error) {
	c, err := l.client()
	if err != nil {
		return CallResult{}, err
	}
	return c.Call(ctx, snapshot, argv)
}

func (l *Lazy) Close(ctx context.Context, snapshot string) error {
	c, err := l.client()
	if err != nil {
		return err
	}
	return c.Close(ctx, snapshot)
}

func (l *Lazy) Abort(ctx context.Context, snapshot string) error {
	c, err := l.client()
	if err != nil {
		return err
	}
	return c.Abort(ctx, snapshot)
}

func (l *Lazy) Delete(ctx context.Context, snapshot string) error {
	c, err := l.client()
	if err != nil {
		return err
	}
	return c.Delete(ctx, snapshot)
}

func (l *Lazy) Cleanup(ctx context.Context) error {
	c, err := l.client()
	if err != nil {
		return err
	}
	return c.Cleanup(ctx)
}

func (l *Lazy) RollbackTo(ctx context.Context, snapshot string) (string, error) {
	c, err := l.client()
	if err != nil {
		return "", err
	}
	return c.RollbackTo(ctx, snapshot)
}
