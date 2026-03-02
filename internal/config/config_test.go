package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExtractFrontmatter(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name: "basic",
			input: `#!/usr/bin/env rc
# socket  = "acme-styles"
# after   = ["acme"]
# restart = "on-failure"

exec acme-styles -v
`,
			want: "socket  = \"acme-styles\"\nafter   = [\"acme\"]\nrestart = \"on-failure\"\n",
		},
		{
			name: "no shebang",
			input: `# socket = "plumb"

exec plumber -f
`,
			want: "socket = \"plumb\"\n",
		},
		{
			name: "bare hash becomes toml comment",
			input: `#!/usr/bin/env rc
# socket = "foo"
#
# watch  = true

exec foo
`,
			// A bare "#" has no "=", so it is re-prefixed as a TOML comment "#".
			want: "socket = \"foo\"\n#\nwatch  = true\n",
		},
		{
			name: "explanatory comment without = is passed as toml comment",
			input: `#!/usr/bin/env rc
# socket  = "acme-hotkeys"
# restart = "always"
# Pipeline: this comment has a colon but no equals sign.

9p read acme-hotkey/keys | cat
`,
			want: "socket  = \"acme-hotkeys\"\nrestart = \"always\"\n#Pipeline: this comment has a colon but no equals sign.\n",
		},
		{
			name:  "empty file",
			input: "",
			want:  "",
		},
		{
			name: "no frontmatter",
			input: `#!/usr/bin/env rc

exec something
`,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractFrontmatter([]byte(tt.input))
			if got != tt.want {
				t.Errorf("extractFrontmatter:\ngot:  %q\nwant: %q", got, tt.want)
			}
		})
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()

	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("fontsrv.rc", `#!/usr/bin/env rc
# socket  = "font"
# restart = "on-failure"

exec fontsrv
`)
	write("acme.rc", `#!/usr/bin/env rc
# watch  = true
# socket = "acme"
`)
	write("acme-styles.rc", `#!/usr/bin/env rc
# socket  = "acme-styles"
# after   = ["acme"]
# restart = "on-failure"
# timeout = "15s"

exec acme-styles -v
`)
	write("README.md", "not a service") // should be ignored

	svcs, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(svcs) != 3 {
		t.Fatalf("got %d services, want 3", len(svcs))
	}

	byName := map[string]*Service{}
	for _, s := range svcs {
		byName[s.Name] = s
	}

	t.Run("fontsrv", func(t *testing.T) {
		s := byName["fontsrv"]
		if s == nil {
			t.Fatal("missing fontsrv")
		}
		if s.Socket != "font" {
			t.Errorf("socket = %q, want %q", s.Socket, "font")
		}
		if s.Watch {
			t.Error("watch should be false")
		}
		if s.Restart != RestartOnFailure {
			t.Errorf("restart = %q, want %q", s.Restart, RestartOnFailure)
		}
		if s.Timeout != 30*time.Second {
			t.Errorf("timeout = %v, want 30s", s.Timeout)
		}
	})

	t.Run("acme watch", func(t *testing.T) {
		s := byName["acme"]
		if s == nil {
			t.Fatal("missing acme")
		}
		if !s.Watch {
			t.Error("watch should be true")
		}
		if s.Socket != "acme" {
			t.Errorf("socket = %q, want %q", s.Socket, "acme")
		}
	})

	t.Run("acme-styles", func(t *testing.T) {
		s := byName["acme-styles"]
		if s == nil {
			t.Fatal("missing acme-styles")
		}
		if len(s.After) != 1 || s.After[0] != "acme" {
			t.Errorf("after = %v, want [acme]", s.After)
		}
		if s.Timeout != 15*time.Second {
			t.Errorf("timeout = %v, want 15s", s.Timeout)
		}
	})
}

func TestValidation(t *testing.T) {
	dir := t.TempDir()

	// Socket required for ready=socket.
	os.WriteFile(filepath.Join(dir, "bad.rc"), []byte(`#!/usr/bin/env rc
# restart = "on-failure"

exec something
`), 0o644)

	_, err := LoadDir(dir)
	if err == nil {
		t.Error("expected error for missing socket field, got nil")
	}
}

func TestReadyStartedNoSocketRequired(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "pipeline.rc"), []byte(`#!/usr/bin/env rc
# after   = ["acme-hotkey"]
# ready   = "started"
# restart = "always"

9p read acme-hotkey/keys | cat
`), 0o644)

	svcs, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(svcs) != 1 {
		t.Fatalf("got %d services", len(svcs))
	}
	if svcs[0].Ready != ReadyStarted {
		t.Errorf("ready = %v, want ReadyStarted", svcs[0].Ready)
	}
}
