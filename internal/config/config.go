// Package config models proxio's configuration, which is entirely environment-driven.
//
// There is no config file and does not need to be one. What this program *is* — its name,
// its version, the address it listens on — lives in internal/app instead.
//
// Notably absent is a public URL. proxio builds no links, sets no cookies and signs
// nothing, so it never needs to know its own address — which also means moving it costs
// nothing. Its sibling projects all need one and this one does not.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment variables read by this package.
const (
	// DataDirEnv is where tokens.json lives. It must exist and be writable, and proxio
	// refuses to start otherwise — see [requireDataDir].
	DataDirEnv = "PROXIO_DATA_DIR"

	// LogLevelEnv sets the slog level: debug, info, warn or error.
	LogLevelEnv = "PROXIO_LOG_LEVEL"

	// DialTimeoutEnv bounds the TCP connect and the TLS handshake to the target.
	DialTimeoutEnv = "PROXIO_DIAL_TIMEOUT"

	// ResponseTimeoutEnv bounds how long a target has to *begin* answering.
	//
	// Not a cap on the response. The whole point of proxio is that a body may take an hour
	// to arrive; what can be bounded without breaking that is the wait for the first byte.
	ResponseTimeoutEnv = "PROXIO_RESPONSE_TIMEOUT"

	// MaxRedirectsEnv is how many 3xx hops proxio will follow on the caller's behalf.
	// Zero turns following off, and the redirect passes through untouched.
	MaxRedirectsEnv = "PROXIO_MAX_REDIRECTS"
)

// Defaults for everything that has one.
const (
	DefaultDataDir         = "/data"
	DefaultLogLevel        = slog.LevelInfo
	DefaultDialTimeout     = 10 * time.Second
	DefaultResponseTimeout = 30 * time.Second

	// DefaultMaxRedirects is Go's own default, and there is no reason to disagree with it.
	DefaultMaxRedirects = 10
)

// Config is everything the process was told at startup.
type Config struct {
	DataDir  string
	LogLevel slog.Level

	DialTimeout     time.Duration
	ResponseTimeout time.Duration

	// MaxRedirects is 0 when a 3xx should pass through to the caller untouched.
	MaxRedirects int
}

// Load reads the environment and proves the data directory is usable. The returned error is
// written for somebody looking at a container that refused to start.
func Load() (*Config, error) {
	cfg := &Config{
		DataDir:         DefaultDataDir,
		LogLevel:        DefaultLogLevel,
		DialTimeout:     DefaultDialTimeout,
		ResponseTimeout: DefaultResponseTimeout,
		MaxRedirects:    DefaultMaxRedirects,
	}

	if dir := strings.TrimSpace(os.Getenv(DataDirEnv)); dir != "" {
		cfg.DataDir = dir
	}
	if v := strings.TrimSpace(os.Getenv(LogLevelEnv)); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			return nil, fmt.Errorf("%s: %q is not a level (debug, info, warn, error)", LogLevelEnv, v)
		}
	}

	var err error
	if cfg.DialTimeout, err = duration(DialTimeoutEnv, cfg.DialTimeout); err != nil {
		return nil, err
	}
	if cfg.ResponseTimeout, err = duration(ResponseTimeoutEnv, cfg.ResponseTimeout); err != nil {
		return nil, err
	}
	if cfg.MaxRedirects, err = redirects(cfg.MaxRedirects); err != nil {
		return nil, err
	}

	if err := requireDataDir(cfg.DataDir); err != nil {
		return nil, err
	}
	return cfg, nil
}

func duration(env string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(env))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration; write it like 30s, 2m or 1h500ms", env, raw)
	}
	// Zero would mean "no timeout at all", which is not a thing to reach by typing 0 into a
	// variable whose name says timeout. Anyone who genuinely wants no ceiling can write a
	// number large enough to be obviously deliberate.
	if d <= 0 {
		return 0, fmt.Errorf("%s: %q is not positive", env, raw)
	}
	return d, nil
}

func redirects(fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(MaxRedirectsEnv))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", MaxRedirectsEnv, raw)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s: %q is negative; 0 means a redirect passes through untouched", MaxRedirectsEnv, raw)
	}
	return n, nil
}

// requireDataDir proves the data directory is there and can be written to.
//
// This is the check the image is shaped around: the Dockerfile deliberately does not declare
// `VOLUME /data` and does not create the directory, because with either of those Docker
// makes an anonymous volume, /data always exists, and this can never fire — which leaves the
// tokens living somewhere that disappears with the container.
//
// The probe is a real file rather than a look at the mode bits. A read-only mount and a
// directory owned by somebody else both look writable to a permission check run as root, and
// the failure they produce otherwise arrives at the first `token create`, weeks later, on
// somebody else's shift.
func requireDataDir(dir string) error {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s does not exist: mount a volume there, e.g. `docker run -v proxio-data:%s`, or set %s", dir, dir, DataDirEnv)
	case err != nil:
		return fmt.Errorf("%s: %w", dir, err)
	case !info.IsDir():
		return fmt.Errorf("%s is not a directory", dir)
	}

	probe, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return fmt.Errorf("%s is not writable: %w", dir, err)
	}
	name := probe.Name()
	probe.Close()
	os.Remove(name)
	return nil
}
