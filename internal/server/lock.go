package server

import (
	"context"
	"errors"
	"time"

	"github.com/sdrahn/mcp-tukit/internal/mcp"
)

// changeLock serializes what this instance does with snapshots: each
// change opens a snapshot from the default, and two at once would race
// for it; zypper's cache is shared as well. Unlike a sync.Mutex, waiting
// for it ends with the caller's context.
type changeLock chan struct{}

func newChangeLock() changeLock { return make(changeLock, 1) }

// TryLock takes the lock if it is free.
func (l changeLock) TryLock() bool {
	select {
	case l <- struct{}{}:
		return true
	default:
		return false
	}
}

func (l changeLock) Unlock() { <-l }

// errNotStarted is returned when a call ended while it waited for a
// running change: it did nothing.
var errNotStarted = errors.New("the call ended while it waited for a running change of this server; nothing was done")

// lockChange takes the change lock, reporting progress on ctx while it
// waits. When ctx ends first, it returns errNotStarted: a call that was
// cancelled before it began must not begin later, and must not keep its
// place among the requests handled at once.
func (s *Server) lockChange(ctx context.Context) error {
	if s.change.TryLock() {
		return nil
	}
	tick := time.NewTicker(s.interval)
	defer tick.Stop()
	start := time.Now()
	for {
		select {
		case s.change <- struct{}{}:
			return nil
		case <-tick.C:
			mcp.Progress(ctx, "waiting for a running change of this server ("+time.Since(start).Round(time.Second).String()+")")
		case <-ctx.Done():
			return errNotStarted
		}
	}
}
