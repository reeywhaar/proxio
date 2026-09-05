// Package tokens is the list of who may use this proxy.
//
// It is a JSON file holding a label and a hash per token, and that is the whole design. A
// database was the obvious alternative and is what the sibling projects use; it costs about
// twelve megabytes of binary and a migration framework to manage one table with five rows in
// it. The one thing the file has to do that a table would do for free is be visible to a
// second process — `docker exec proxio proxio token create x` is not the running server —
// and [Store.refreshLocked] is how.
package tokens

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Errors this package returns, for callers that need to tell them apart from a broken disk.
var (
	ErrNotFound = errors.New("no such token")
	ErrConflict = errors.New("a token with that label already exists")
	ErrInvalid  = errors.New("invalid label")
)

const (
	// FileName is the file inside the data directory.
	FileName = "tokens.json"

	// Prefix marks a proxio token in a log somebody is grepping, and in a config file where
	// it sits beside other opaque strings.
	Prefix = "px_"

	// secretBytes is 32, which is 256 bits, which is why the hash below can be a fast one.
	secretBytes = 32

	// fileVersion is written into the file so that a future format change is a migration
	// rather than a crash on a field that used to mean something else. Adding per-token host
	// rules is the change this is being kept for.
	fileVersion = 1

	// labelMax is generous. A label is written by a person, read by a person in a log line,
	// and never parsed.
	labelMax = 64
)

// Token is one entry: what it is called, and enough to recognise it.
//
// The secret itself is not here and is not anywhere. It exists once, in the output of
// [Store.Create], and a lost one is deleted and minted again rather than recovered.
type Token struct {
	Label     string `json:"label"`
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"created_at"`
}

// Created is CreatedAt as a time, in UTC like everything else here.
func (t Token) Created() time.Time { return time.Unix(t.CreatedAt, 0).UTC() }

// file is the on-disk shape.
type file struct {
	Version int     `json:"version"`
	Tokens  []Token `json:"tokens"`
}

// Store is the token file, and a copy of it held in memory.
type Store struct {
	path string

	mu      sync.Mutex
	tokens  []Token
	modTime time.Time
	size    int64
	loaded  bool
}

// Open reads the token file in dir. A file that is not there is not an error — that is a
// fresh volume, and it means no tokens.
//
// A file that is there and unreadable *is* an error, and deliberately a fatal one: a corrupt
// tokens.json at startup is a configuration failure, and a server that came up anyway would
// refuse every request for a reason nobody could see. Once running, the same corruption is
// handled differently — see [Store.refreshLocked].
func Open(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, FileName)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path is where the tokens live, for an error message that has to name it.
func (s *Store) Path() string { return s.path }

// Create mints a token and writes it down. The returned string is the secret, and this is
// the only time it exists.
func (s *Store) Create(label string) (Token, string, error) {
	if err := ValidLabel(label); err != nil {
		return Token{}, "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Re-read before appending. Without this, a `token create` run against a server that
	// has been handed tokens by some other shell writes back the set this process happened
	// to load, silently deleting them.
	//
	// Two `token create` runs racing each other can still lose one, and that is accepted:
	// token management is a person typing into a terminal, and the file lock that would
	// close the window costs more than the window is worth.
	if err := s.refreshLocked(); err != nil {
		return Token{}, "", err
	}
	if slices.ContainsFunc(s.tokens, func(t Token) bool { return t.Label == label }) {
		return Token{}, "", fmt.Errorf("%q: %w", label, ErrConflict)
	}

	secret, err := mint()
	if err != nil {
		return Token{}, "", err
	}
	tok := Token{Label: label, Hash: hashOf(secret), CreatedAt: time.Now().Unix()}

	s.tokens = append(s.tokens, tok)
	if err := s.writeLocked(); err != nil {
		s.tokens = s.tokens[:len(s.tokens)-1]
		return Token{}, "", err
	}
	return tok, secret, nil
}

// Delete removes a token by label.
func (s *Store) Delete(label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(); err != nil {
		return err
	}

	i := slices.IndexFunc(s.tokens, func(t Token) bool { return t.Label == label })
	if i < 0 {
		return fmt.Errorf("%q: %w", label, ErrNotFound)
	}
	removed := s.tokens[i]
	s.tokens = slices.Delete(slices.Clone(s.tokens), i, i+1)
	if err := s.writeLocked(); err != nil {
		s.tokens = slices.Insert(s.tokens, i, removed)
		return err
	}
	return nil
}

// List returns every token, oldest first, without any secret in it.
func (s *Store) List() ([]Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(); err != nil {
		return nil, err
	}
	out := slices.Clone(s.tokens)
	slices.SortStableFunc(out, func(a, b Token) int { return int(a.CreatedAt - b.CreatedAt) })
	return out, nil
}

// Verify reports which token a presented secret is, if any.
//
// The error is separate from the bool on purpose: it means the file could not be re-read,
// not that the secret was wrong. The set already in memory is still used, so a botched
// hand-edit of tokens.json does not take the proxy down — the caller logs the error and
// carries on with the last good list.
func (s *Store) Verify(secret string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.refreshLocked()

	// Hash first, then walk the whole list without stopping at a match. Returning early
	// would make the reply time depend on how far down the file a token sits, which is a few
	// hundred nanoseconds of information about a secret that is otherwise unguessable.
	want := []byte(hashOf(secret))
	label, found := "", false
	for _, t := range s.tokens {
		if subtle.ConstantTimeCompare([]byte(t.Hash), want) == 1 {
			label, found = t.Label, true
		}
	}
	return label, found, err
}

// refreshLocked re-reads the file when it has changed on disk.
//
// A stat before every authentication, which costs about a microsecond against a proxied
// request that costs milliseconds. The alternatives were a file watcher — a dependency, and
// inotify on an overlay mount is not something to rely on — or a signal, which works and is
// forgotten exactly once, by the person who then spends an hour on why their new token is
// rejected.
//
// On a read or parse failure the in-memory set is kept rather than cleared, so a corrupt
// file does not become an outage; the stamp is recorded anyway so the complaint is made once
// per change to the file rather than once per request.
func (s *Store) refreshLocked() error {
	info, err := os.Stat(s.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.tokens, s.modTime, s.size, s.loaded = nil, time.Time{}, 0, true
		return nil
	case err != nil:
		return fmt.Errorf("%s: %w", s.path, err)
	}

	if s.loaded && info.Size() == s.size && info.ModTime().Equal(s.modTime) {
		return nil
	}
	s.modTime, s.size, s.loaded = info.ModTime(), info.Size(), true

	raw, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("%s: %w", s.path, err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("%s: %w", s.path, err)
	}
	if f.Version != fileVersion {
		return fmt.Errorf("%s: version %d was written by a different proxio; this one speaks version %d", s.path, f.Version, fileVersion)
	}
	s.tokens = f.Tokens
	return nil
}

// writeLocked replaces the file atomically, so the server — which reads it without any lock
// between the two processes — can never see a half-written one.
func (s *Store) writeLocked() error {
	raw, err := json.MarshalIndent(file{Version: fileVersion, Tokens: s.tokens}, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, FileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	defer os.Remove(tmp.Name())

	// 0600 before anything is in it. CreateTemp already makes it 0600, but the file outlives
	// this line and the mode is not something to leave to a default.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	// The rename itself needs a durable directory entry, or a crash here leaves a token that
	// was reported as created and is not there.
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}

	// Our own write must not look like somebody else's change on the next stat.
	if info, err := os.Stat(s.path); err == nil {
		s.modTime, s.size = info.ModTime(), info.Size()
	}
	return nil
}

// ValidLabel reports whether a label can be written down and read back.
//
// A label ends up in an access log line and in JSON, and is the name an operator deletes a
// token by. Spaces, quotes and control characters all make one of those three worse, so the
// alphabet is narrow on purpose.
func ValidLabel(label string) error {
	if label == "" {
		return fmt.Errorf("%w: a label is required, e.g. `token create some_service`", ErrInvalid)
	}
	if len(label) > labelMax {
		return fmt.Errorf("%w: %q is longer than %d characters", ErrInvalid, label, labelMax)
	}
	for _, r := range label {
		ok := r == '_' || r == '-' || r == '.' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return fmt.Errorf("%w: %q may hold only letters, digits, and _ - .", ErrInvalid, label)
		}
	}
	return nil
}

// mint makes a secret. 256 bits from crypto/rand, base64url so it survives a shell, a YAML
// file and a header value without quoting.
func mint() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashOf is SHA-256, hex.
//
// Not bcrypt, and the difference from how the sibling projects hash a password is the whole
// point: bcrypt exists to make a low-entropy secret that a person chose expensive to guess,
// and a 256-bit random string is not guessable at any cost. A slow hash here would buy
// nothing and be paid for by every proxied request.
func hashOf(secret string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(secret)))
	return hex.EncodeToString(sum[:])
}
