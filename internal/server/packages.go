package server

// Changes in a new snapshot: packages, system updates, commands.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sdrahn/mcp-tukit/internal/tukit"
)

func (s *Server) checkPackages(pkgs []string) error {
	if len(pkgs) == 0 {
		return errors.New("packages: none given")
	}
	if len(pkgs) > 100 {
		return errors.New("packages: at most 100")
	}
	for _, p := range pkgs {
		if len(p) > 200 || !packageRE.MatchString(p) {
			return fmt.Errorf("package %q: not a package name (letters, digits and _.+:~-, optionally a version relation such as foo>=1.2)", p)
		}
	}
	return nil
}

func (s *Server) base(b string) (string, error) {
	if b == "" {
		return s.cfg.DefaultBase, nil
	}
	if !tukit.ValidSnapshot(b, true) {
		return "", fmt.Errorf("base %q: must be active, default or a snapshot number", b)
	}
	return b, nil
}

func (s *Server) zypper(args ...string) []string {
	return append([]string{s.cfg.Zypper, "--non-interactive"}, args...)
}

func (s *Server) pkgInstall(ctx context.Context, args json.RawMessage) map[string]any {
	var a struct {
		Packages       []string `json:"packages"`
		Base           string   `json:"base"`
		AcceptLicenses bool     `json:"accept_licenses"`
		NoRecommends   bool     `json:"no_recommends"`
	}
	if err := decode(args, &a); err != nil {
		return toolError("%v", err)
	}
	if err := s.checkPackages(a.Packages); err != nil {
		return toolError("%v", err)
	}
	base, err := s.base(a.Base)
	if err != nil {
		return toolError("%v", err)
	}
	argv := s.zypper("install")
	if a.AcceptLicenses {
		argv = append(argv, "--auto-agree-with-licenses")
	}
	if a.NoRecommends {
		argv = append(argv, "--no-recommends")
	}
	argv = append(argv, a.Packages...)
	return s.transact(ctx, base, "pkg install "+strings.Join(a.Packages, " "), argv, zypperOutcome)
}

func (s *Server) pkgRemove(ctx context.Context, args json.RawMessage) map[string]any {
	var a struct {
		Packages  []string `json:"packages"`
		Base      string   `json:"base"`
		CleanDeps bool     `json:"clean_deps"`
	}
	if err := decode(args, &a); err != nil {
		return toolError("%v", err)
	}
	if err := s.checkPackages(a.Packages); err != nil {
		return toolError("%v", err)
	}
	base, err := s.base(a.Base)
	if err != nil {
		return toolError("%v", err)
	}
	argv := s.zypper("remove")
	if a.CleanDeps {
		argv = append(argv, "--clean-deps")
	}
	argv = append(argv, a.Packages...)
	return s.transact(ctx, base, "pkg remove "+strings.Join(a.Packages, " "), argv, zypperOutcome)
}

func (s *Server) updateSystem(ctx context.Context, args json.RawMessage) map[string]any {
	var a struct {
		Mode           string `json:"mode"`
		Base           string `json:"base"`
		AcceptLicenses bool   `json:"accept_licenses"`
		NoRecommends   bool   `json:"no_recommends"`
	}
	if err := decode(args, &a); err != nil {
		return toolError("%v", err)
	}
	base, err := s.base(a.Base)
	if err != nil {
		return toolError("%v", err)
	}
	var argv []string
	var what string
	switch a.Mode {
	case "", "update":
		argv, what = s.zypper("update"), "up"
	case "dist-upgrade":
		argv, what = s.zypper("dist-upgrade"), "dup"
	case "patch":
		argv, what = s.zypper("patch"), "patch"
	default:
		return toolError("mode %q: must be update, dist-upgrade or patch", a.Mode)
	}
	if a.AcceptLicenses {
		argv = append(argv, "--auto-agree-with-licenses")
	}
	if a.NoRecommends {
		argv = append(argv, "--no-recommends")
	}
	return s.transact(ctx, base, what, argv, zypperOutcome)
}

func (s *Server) runInSnapshot(ctx context.Context, args json.RawMessage) map[string]any {
	var a struct {
		Command     []string `json:"command"`
		Base        string   `json:"base"`
		Description string   `json:"description"`
	}
	if err := decode(args, &a); err != nil {
		return toolError("%v", err)
	}
	if len(a.Command) == 0 || a.Command[0] == "" {
		return toolError("command: none given")
	}
	if len(a.Command) > 256 {
		return toolError("command: at most 256 words")
	}
	for _, w := range a.Command {
		if len(w) > 4096 || strings.ContainsRune(w, 0) {
			return toolError("command: a word is longer than 4096 bytes or contains a NUL byte")
		}
	}
	base, err := s.base(a.Base)
	if err != nil {
		return toolError("%v", err)
	}
	desc := a.Description
	if desc == "" {
		desc = "run " + strings.Join(a.Command, " ")
	}
	return s.transact(ctx, base, desc, a.Command, exitOutcome)
}
