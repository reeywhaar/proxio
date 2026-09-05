package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dataDir points the environment at a usable directory, which every Load needs.
func dataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(DataDirEnv, dir)
	return dir
}

func TestLoadDefaults(t *testing.T) {
	dir := dataDir(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataDir != dir {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, dir)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.DialTimeout != DefaultDialTimeout {
		t.Errorf("DialTimeout = %v, want %v", cfg.DialTimeout, DefaultDialTimeout)
	}
	if cfg.ResponseTimeout != DefaultResponseTimeout {
		t.Errorf("ResponseTimeout = %v, want %v", cfg.ResponseTimeout, DefaultResponseTimeout)
	}
	if cfg.MaxRedirects != DefaultMaxRedirects {
		t.Errorf("MaxRedirects = %d, want %d", cfg.MaxRedirects, DefaultMaxRedirects)
	}
}

func TestLoadReadsEverySetting(t *testing.T) {
	dataDir(t)
	t.Setenv(LogLevelEnv, "debug")
	t.Setenv(DialTimeoutEnv, "2s")
	t.Setenv(ResponseTimeoutEnv, "1m30s")
	t.Setenv(MaxRedirectsEnv, "3")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug", cfg.LogLevel)
	}
	if cfg.DialTimeout != 2*time.Second {
		t.Errorf("DialTimeout = %v, want 2s", cfg.DialTimeout)
	}
	if cfg.ResponseTimeout != 90*time.Second {
		t.Errorf("ResponseTimeout = %v, want 90s", cfg.ResponseTimeout)
	}
	if cfg.MaxRedirects != 3 {
		t.Errorf("MaxRedirects = %d, want 3", cfg.MaxRedirects)
	}
}

// Zero redirects is a policy, not a mistake: the 3xx passes through untouched.
func TestZeroRedirectsIsAllowed(t *testing.T) {
	dataDir(t)
	t.Setenv(MaxRedirectsEnv, "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxRedirects != 0 {
		t.Errorf("MaxRedirects = %d, want 0", cfg.MaxRedirects)
	}
}

func TestLoadRefusesNonsense(t *testing.T) {
	cases := []struct {
		name string
		env  string
		val  string
		want string
	}{
		{"level", LogLevelEnv, "chatty", "is not a level"},
		{"duration", DialTimeoutEnv, "ten seconds", "is not a duration"},
		{"zero duration", ResponseTimeoutEnv, "0s", "is not positive"},
		{"negative duration", DialTimeoutEnv, "-5s", "is not positive"},
		{"redirects", MaxRedirectsEnv, "lots", "is not a number"},
		{"negative redirects", MaxRedirectsEnv, "-1", "is negative"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dataDir(t)
			t.Setenv(c.env, c.val)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load accepted %s=%q", c.env, c.val)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to mention %q", err, c.want)
			}
		})
	}
}

// The requirement the image is shaped around: no volume, no start.
func TestMissingDataDirRefusesToStart(t *testing.T) {
	t.Setenv(DataDirEnv, filepath.Join(t.TempDir(), "never-mounted"))

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted a data directory that does not exist")
	}
	if !strings.Contains(err.Error(), "mount a volume") {
		t.Errorf("error = %q, want it to say how to fix it", err)
	}
}

func TestDataDirMustBeADirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(DataDirEnv, file)

	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a file as the data directory")
	}
}

// A read-only mount has to fail here, at startup, rather than at the first `token create`
// weeks later.
func TestReadOnlyDataDirRefusesToStart(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can write to a read-only directory")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	t.Setenv(DataDirEnv, dir)

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted a data directory it cannot write to")
	}
	if !strings.Contains(err.Error(), "not writable") {
		t.Errorf("error = %q, want it to say the directory is not writable", err)
	}
}
