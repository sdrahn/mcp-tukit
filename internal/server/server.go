// Package server is the MCP side of mcp-server-tukit: the tools, their
// schemas and what each does with tukit.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/sdrahn/mcp-tukit/internal/mcp"
	"github.com/sdrahn/mcp-tukit/internal/tukit"
)

// Name is the server's name in serverInfo.
const Name = "mcp-server-tukit"

// Config is what the command line sets.
type Config struct {
	// Version is reported in serverInfo.
	Version string
	// Zypper is the package manager's path inside the snapshot.
	Zypper string
	// DefaultBase is the base of new snapshots when a call names none.
	DefaultBase string
	// Timeout bounds a change: opening the snapshot, the command, closing.
	Timeout time.Duration
	// AllowCommands offers run_in_snapshot, which runs any command as
	// root in a new snapshot.
	AllowCommands bool
	// MaxOutput bounds the command output in a result.
	MaxOutput int
	// RebootUnit is the systemd unit the reboot tool starts (it runs
	// transactional-update reboot); empty: no reboot tool.
	RebootUnit string
	// ApplyUnit is the systemd unit the apply tool runs (it runs
	// transactional-update apply); empty: no apply tool.
	ApplyUnit string
	// RebootConfig are transactional-update's configuration files, in the
	// order it reads them, for the reboot method the result names.
	RebootConfig []string
	// About, if set, opens the instructions.
	About string
	// Log receives a line for every change (the journal, on stderr).
	Log io.Writer
}

// Server answers MCP requests with tukit.
type Server struct {
	cfg Config
	tk  tukit.Backend

	// change serializes the changes of this instance (lock.go).
	change changeLock
	// byName and listing are the tools and the tools/list answer, built
	// once: neither changes while the server runs.
	byName  map[string]*tool
	listing json.RawMessage
	// interval between progress notifications while a command runs.
	interval time.Duration
	// logMu serializes the lines written to cfg.Log: requests are handled
	// concurrently, and the writer need not be safe for that.
	logMu sync.Mutex
}

// New returns a server using tk.
func New(cfg Config, tk tukit.Backend) *Server {
	if cfg.Zypper == "" {
		cfg.Zypper = "/usr/bin/zypper"
	}
	if cfg.DefaultBase == "" {
		cfg.DefaultBase = "default"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Hour
	}
	if cfg.MaxOutput <= 0 {
		cfg.MaxOutput = 32 << 10
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	s := &Server{cfg: cfg, tk: tk, interval: 15 * time.Second, change: newChangeLock()}
	s.register()
	return s
}

// serverInfo is the server's name, title and version, as initialize and
// server/discover give them.
func (s *Server) serverInfo() map[string]any {
	return map[string]any{"name": Name, "title": "Transactional updates (tukit)", "version": s.cfg.Version}
}

// Handle answers one MCP request.
func (s *Server) Handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(params, &p)
		return map[string]any{
			"protocolVersion": mcp.Negotiate(p.ProtocolVersion),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      s.serverInfo(),
			"instructions":    s.instructions(),
		}, nil
	case "server/discover":
		// MCP 2026-07-28: what initialize told, without the handshake.
		req := mcp.RequestOf(ctx)
		client := req.Client
		if client == "" {
			client = "a client"
		}
		version := req.Version
		if version == "" {
			version = "no version given"
		}
		s.logf("server/discover from %s (MCP %s)", client, version)
		return map[string]any{
			"supportedVersions": mcp.ModernVersions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"instructions":      s.instructions(),
			"_meta": map[string]any{
				mcp.MetaServerInfo: s.serverInfo(),
			},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.listing}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &mcp.Error{Code: mcp.CodeInvalidParams, Message: "invalid params"}
		}
		t := s.byName[p.Name]
		if t == nil {
			return nil, &mcp.Error{Code: mcp.CodeInvalidParams, Message: "unknown tool " + strconv.Quote(p.Name)}
		}
		args := p.Arguments
		if len(args) == 0 || string(args) == "null" {
			args = json.RawMessage("{}")
		}
		return t.call(ctx, args), nil
	}
	return nil, &mcp.Error{Code: mcp.CodeMethodNotFound, Message: "method not found"}
}

func (s *Server) instructions() string {
	out := "Changes this transactional system (openSUSE MicroOS, Leap Micro, SL Micro, SLES 16 in immutable mode) " +
		"the way transactional-update does, through tukit: each change runs in a new snapshot of the root file system, " +
		"and the running system stays as it is. The new snapshot becomes the default and takes effect at the next reboot; " +
		"until then the change is not visible in the running system. A change that fails is discarded and leaves no snapshot. " +
		"New snapshots build on the default snapshot (\"" + s.cfg.DefaultBase + "\" unless a call names a base), " +
		"so changes made one after the other before a reboot add up. " +
		"Use get_status to see whether a reboot is pending, list_snapshots for the snapshots, " +
		"and rollback to boot an earlier snapshot next time. " +
		"/etc, /var and /home are not part of the read-only root and are changed directly, not with this server."
	if s.cfg.RebootUnit != "" {
		out += " reboot activates the default snapshot, the way transactional-update reboot does (the configured reboot method: " +
			"at once, or in rebootmgr's maintenance window); it takes the system and this connection down: " +
			"call it only when asked to reboot, or when a change should take effect now and that was agreed, and name the snapshot you expect to boot."
	} else {
		out += " This server does not reboot the system; say that a reboot is needed when a change was applied."
	}
	if s.cfg.ApplyUnit != "" {
		out += " apply makes the default snapshot's /usr, /etc and /boot those of the running system without a reboot " +
			"(transactional-update apply), so that newly installed programs can be used at once; running programs and services " +
			"keep their old files until restarted, and a new kernel needs a reboot."
	}
	if s.cfg.AllowCommands {
		out += " run_in_snapshot runs any command as root in a new snapshot; prefer the package tools where they do the job."
	}
	if s.cfg.About != "" {
		out = s.cfg.About + "\n\n" + out
	}
	return out
}

func (s *Server) logf(format string, a ...any) {
	// One line, whatever a caller put into a command or a name.
	line := Name + ": " + shorten(printable(fmt.Sprintf(format, a...)), maxLogLine) + "\n"
	s.logMu.Lock()
	defer s.logMu.Unlock()
	_, _ = io.WriteString(s.cfg.Log, line)
}
