// Command mcp-server-tukit is an MCP server (stdio) for transactional
// updates on immutable SUSE systems (openSUSE MicroOS, Leap Micro, SL
// Micro, SLES 16.1 in immutable mode): it installs, removes and updates
// packages in new snapshots, lists them and rolls back, through tukitd,
// the D-Bus service of tukit (openSUSE/transactional-update).
//
// It is meant to run behind mcp-gateway (github.com/sdrahn/mcp-gateway)
// in its sandbox with CAP_SYS_ADMIN only, in its SELinux domain
// mcpsrv_tukit_t (tukitd answers only callers with CAP_SYS_ADMIN): the
// server itself uses no privileges, tukitd does the work, and the
// gateway's policy
// decides who may call which tool, with whose approval.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sdrahn/mcp-tukit/internal/mcp"
	"github.com/sdrahn/mcp-tukit/internal/server"
	"github.com/sdrahn/mcp-tukit/internal/tukit"
)

// rebootConfig are transactional-update's configuration files, in the
// order it reads them (its CONFFILESYSTEM, the snippets, CONFFILE).
var rebootConfig = []string{"/usr/etc/transactional-update.conf", "/usr/etc/transactional-update.conf.d",
	"/etc/transactional-update.conf"}

// version is set at build time (-ldflags "-X main.version=…").
var version = "0.4.0+dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, connect))
}

// connect returns tukitd on the system bus, connected on first use: the
// server starts, and lists its tools, without the bus or tukitd.
func connect() (tukit.Backend, func(), error) {
	l := tukit.NewLazy(tukit.Connect)
	return l, l.CloseBus, nil
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, connect func() (tukit.Backend, func(), error)) int {
	fs := flag.NewFlagSet(server.Name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	allowCommands := fs.Bool("allow-commands", false, "offer run_in_snapshot: any command, as root, in a new snapshot")
	base := fs.String("base", "default", "the snapshot new ones build on when a call names none: default, active or a number")
	timeout := fs.Duration("timeout", 2*time.Hour, "how long a change (a package installation, an update) may take")
	zypper := fs.String("zypper", "/usr/bin/zypper", "zypper's path in the snapshots")
	rebootUnit := fs.String("reboot-unit", "mcp-tukit-reboot.service", "the systemd unit the reboot tool starts (it runs transactional-update reboot); empty: no reboot tool")
	applyUnit := fs.String("apply-unit", "mcp-tukit-apply.service", "the systemd unit the apply tool runs (it runs transactional-update apply); empty: no apply tool")
	about := fs.String("instructions", "", "what the client's model should know about this host first (opens the server's instructions)")
	check := fs.Bool("check", false, "connect to tukitd, list the snapshots and exit")
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "usage: %s [options]\n\nAn MCP server for transactional updates with tukit, on stdin/stdout.\n\n", server.Name)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, server.Name, version)
		return 0
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	if !tukit.ValidSnapshot(*base, true) {
		_, _ = fmt.Fprintf(stderr, "%s: -base %q: must be default, active or a snapshot number\n", server.Name, *base)
		return 2
	}
	for flag, unit := range map[string]string{"-reboot-unit": *rebootUnit, "-apply-unit": *applyUnit} {
		if unit != "" && (!strings.HasSuffix(unit, ".service") || strings.ContainsAny(unit, "/ ")) {
			_, _ = fmt.Fprintf(stderr, "%s: %s %q: must name a .service unit\n", server.Name, flag, unit)
			return 2
		}
	}
	if *timeout < time.Minute {
		_, _ = fmt.Fprintf(stderr, "%s: -timeout %s: at least 1m\n", server.Name, *timeout)
		return 2
	}
	tk, closeBus, err := connect()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, server.Name+":", err)
		return 1
	}
	defer closeBus()
	if *check {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		list, err := tk.List(ctx, tukit.Columns)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, server.Name+":", err)
			return 1
		}
		for _, m := range list {
			_, _ = fmt.Fprintf(stdout, "%s\tactive=%s\tdefault=%s\t%s\t%s\n", m["number"], m["active"], m["default"], m["date"], m["description"])
		}
		return 0
	}
	s := server.New(server.Config{Version: version, Zypper: *zypper, DefaultBase: *base, Timeout: *timeout,
		AllowCommands: *allowCommands, RebootUnit: *rebootUnit, ApplyUnit: *applyUnit, RebootConfig: rebootConfig, About: *about, Log: stderr}, tk)
	// A log line on stderr, which goes to the journal.
	_, _ = fmt.Fprintf(stderr, "%s %s: tukitd on the system bus; new snapshots from %s; run_in_snapshot %s; reboot %s; apply %s\n",
		server.Name, version, *base, map[bool]string{true: "offered", false: "not offered"}[*allowCommands],
		map[bool]string{true: "through " + *rebootUnit, false: "not offered"}[*rebootUnit != ""],
		map[bool]string{true: "through " + *applyUnit, false: "not offered"}[*applyUnit != ""])
	// Requests carry only arguments, which are short.
	if err := mcp.New(s.Handle, stdout).Serve(stdin, 1<<20); err != nil {
		_, _ = fmt.Fprintln(stderr, server.Name+":", err)
		return 1
	}
	return 0
}
