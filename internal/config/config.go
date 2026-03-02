// Package config parses service definitions from a directory of rc scripts.
//
// Each *.rc file in the directory defines one service. The service name is
// the filename without the .rc extension. Metadata is carried in a TOML
// block embedded in the leading # comment lines of the script:
//
//	#!/usr/bin/env rc
//	# socket  = "acme-styles"
//	# after   = ["acme"]
//	# restart = "on-failure"
//
//	exec acme-styles -styles $home/lib/acme/styles -v
//
// Because # is the comment character in both rc and TOML, the metadata block
// is simultaneously valid rc (ignored) and valid TOML (parsed by 9init after
// stripping the leading # prefix). The first blank line ends the block.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// ReadyMode describes how 9init determines a service is ready to accept
// dependents.
type ReadyMode string

const (
	// ReadySocket waits for $NAMESPACE/<socket> to appear as a Unix socket.
	ReadySocket ReadyMode = "socket"
	// ReadyStarted considers the service ready as soon as exec succeeds.
	// Used for pipeline services that post no socket.
	ReadyStarted ReadyMode = "started"
)

// RestartPolicy describes when 9init restarts a managed service after exit.
type RestartPolicy string

const (
	RestartOnFailure RestartPolicy = "on-failure" // restart on non-zero exit or signal
	RestartAlways    RestartPolicy = "always"      // restart regardless of exit status
	RestartNever     RestartPolicy = "never"       // never restart; log and move on
)

// Service is the parsed and validated definition of a single service.
type Service struct {
	// Name is the service identifier, derived from the filename (sans .rc).
	Name string
	// Path is the absolute path to the .rc script on disk.
	Path string

	// Socket is the basename of the Unix socket in $NAMESPACE/ that
	// signals readiness. Required when Ready == ReadySocket.
	// Always explicit; never inferred from Name.
	Socket string

	// Watch, if true, marks this as an externally-managed service.
	// 9init watches the socket but never starts or restarts the service.
	// The script body is never executed for watched services.
	Watch bool

	// After lists the names of services that must reach their ready state
	// before this service is started.
	After []string

	Ready   ReadyMode
	Restart RestartPolicy

	// Timeout is the maximum time to wait for the socket to appear after
	// exec before treating the service as crashed.
	Timeout time.Duration

	// StopTimeout is the time between SIGTERM and SIGKILL during shutdown.
	StopTimeout time.Duration

	// ReloadSignal, if non-empty, is the signal name sent by "9init reload".
	// Example: "HUP".
	ReloadSignal string

	// Crash budget: if the service exits within MinRuntime more than
	// MaxRestarts times in RestartWindow, it transitions to "failed".
	MaxRestarts   int
	RestartWindow time.Duration
	MinRuntime    time.Duration
}

// frontmatter is the raw TOML structure decoded from a script's header block.
type frontmatter struct {
	Socket        string   `toml:"socket"`
	Watch         bool     `toml:"watch"`
	After         []string `toml:"after"`
	Ready         string   `toml:"ready"`
	Restart       string   `toml:"restart"`
	Timeout       string   `toml:"timeout"`
	StopTimeout   string   `toml:"stop_timeout"`
	ReloadSignal  string   `toml:"reload_signal"`
	MaxRestarts   *int     `toml:"max_restarts"`
	RestartWindow string   `toml:"restart_window"`
	MinRuntime    string   `toml:"min_runtime"`
}

// LoadDir scans dir for *.rc files and returns a validated Service for each.
// Files are read in directory order; use graph.Build for topological ordering.
func LoadDir(dir string) ([]*Service, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read service dir %s: %w", dir, err)
	}
	var services []*Service
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".rc") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".rc")
		path := filepath.Join(dir, e.Name())
		svc, err := parseFile(name, path)
		if err != nil {
			return nil, fmt.Errorf("service %q (%s): %w", name, e.Name(), err)
		}
		services = append(services, svc)
	}
	return services, nil
}

// parseFile reads path, extracts its frontmatter, and returns a Service.
func parseFile(name, path string) (*Service, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	tomlSrc := extractFrontmatter(data)

	var fm frontmatter
	if _, err := toml.Decode(tomlSrc, &fm); err != nil {
		return nil, fmt.Errorf("parse frontmatter: %w", err)
	}
	return buildService(name, path, &fm)
}

// extractFrontmatter collects the leading block of # comment lines from src,
// skips an optional shebang on line 1, strips the "# " prefix from each line,
// and returns the result as a TOML-parseable string.
//
// Extraction stops at the first blank line or the first line that does not
// begin with '#'. A bare '#' (with nothing after it) is passed through as a
// blank line, which TOML silently ignores.
func extractFrontmatter(src []byte) string {
	var out strings.Builder
	for i, line := range bytes.Split(src, []byte("\n")) {
		s := string(line)
		if i == 0 && strings.HasPrefix(s, "#!") {
			continue // shebang
		}
		if strings.TrimSpace(s) == "" {
			break // blank line ends the frontmatter block
		}
		if !strings.HasPrefix(s, "#") {
			break // non-comment line ends the frontmatter block
		}
		// Strip leading "# " or bare "#".
		s = strings.TrimPrefix(s, "#")
		s = strings.TrimPrefix(s, " ")
		// If the line doesn't contain "=" it isn't a TOML key-value pair.
		// Reattach "#" so that TOML treats it as its own comment and ignores it.
		// This lets authors write free-form explanatory comments in the header
		// block without breaking the TOML parse (e.g. "# Note: ...").
		if !strings.Contains(s, "=") {
			s = "#" + s
		}
		out.WriteString(s)
		out.WriteByte('\n')
	}
	return out.String()
}

func buildService(name, path string, fm *frontmatter) (*Service, error) {
	svc := &Service{
		Name:         name,
		Path:         path,
		Socket:       fm.Socket,
		Watch:        fm.Watch,
		After:        fm.After,
		ReloadSignal: fm.ReloadSignal,
	}

	switch fm.Ready {
	case "", "socket":
		svc.Ready = ReadySocket
	case "started":
		svc.Ready = ReadyStarted
	default:
		return nil, fmt.Errorf("unknown ready mode %q (want \"socket\" or \"started\")", fm.Ready)
	}

	switch fm.Restart {
	case "", "on-failure":
		svc.Restart = RestartOnFailure
	case "always":
		svc.Restart = RestartAlways
	case "never":
		svc.Restart = RestartNever
	default:
		return nil, fmt.Errorf("unknown restart policy %q", fm.Restart)
	}

	var err error
	if svc.Timeout, err = parseDur(fm.Timeout, "30s"); err != nil {
		return nil, fmt.Errorf("timeout: %w", err)
	}
	if svc.StopTimeout, err = parseDur(fm.StopTimeout, "5s"); err != nil {
		return nil, fmt.Errorf("stop_timeout: %w", err)
	}
	if svc.RestartWindow, err = parseDur(fm.RestartWindow, "60s"); err != nil {
		return nil, fmt.Errorf("restart_window: %w", err)
	}
	if svc.MinRuntime, err = parseDur(fm.MinRuntime, "5s"); err != nil {
		return nil, fmt.Errorf("min_runtime: %w", err)
	}

	if fm.MaxRestarts != nil {
		svc.MaxRestarts = *fm.MaxRestarts
	} else {
		svc.MaxRestarts = 5
	}

	// Validate: socket is required for socket-ready non-watched services.
	if !svc.Watch && svc.Ready == ReadySocket && svc.Socket == "" {
		return nil, fmt.Errorf("socket is required when ready = %q", svc.Ready)
	}

	return svc, nil
}

func parseDur(s, def string) (time.Duration, error) {
	if s == "" {
		s = def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	return d, nil
}
