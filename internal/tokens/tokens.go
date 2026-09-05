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
	"strconv"
	"strings"
	"sync"
	"time"
)

// randRead is crypto/rand, as a var so a test can force the collision the minting loop exists
// for. Nothing else reassigns it.
var randRead = rand.Read

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

	// NoncedPrefix marks what a nonced token puts on the wire, which is not a secret at all:
	// pxc_<unix seconds>.<token id>.<sha256 hex>.
	//
	// It exists because proxio's credential travels in a URL, and a URL is logged by every
	// reverse proxy in front of it. A raw token in caddy's access log is reusable forever; one
	// of these is useless in five minutes.
	NoncedPrefix = "pxc_"

	// Sep separates a nonced token's fields. A dot rather than a colon: it is unreserved in a
	// URL, where a colon is a delimiter, so the value survives being put in a query parameter
	// without escaping. No field can contain one — they are digits and hex.
	Sep = "."

	// Window is how far a nonce may be from now, in either direction, to allow for clock
	// skew. Five minutes is what Stripe and Slack settled on.
	Window = 5 * time.Minute

	// IDLen is how much of the hash names a token. Eight hex characters is 32 bits, short
	// enough to read out of a log line and long enough that [Store.Create] re-minting on a
	// clash makes a collision impossible rather than merely unlikely.
	IDLen = 8

	// HintLen is how much of a secret is kept to identify it in a listing.
	//
	// Counted after [Prefix], which every secret carries and which therefore identifies
	// nothing: eight characters including it would be five doing the work. Eight of the
	// forty-three random ones leaves about 210 bits, so this identifies a token without
	// being a head start on guessing it.
	//
	// The same width as [IDLen] and unique for the same reason: [Store.Create] re-mints on a
	// clash, so no two tokens ever share one.
	HintLen = 8

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

	// Hint is the secret's first [HintLen] characters after [Prefix], kept so a listing can
	// be matched against a token written down in somebody's config file.
	//
	// Unique across the file, because [Store.Create] re-mints on a clash. That is what makes
	// it an identifier rather than a decoration: two tokens can never show the same one, so
	// reading it off a listing settles which token you are holding.
	//
	// It is the second of two identifiers and they answer different questions. This one is
	// read off the *secret*, so it matches what a caller has in a config file. [Token.ID] is
	// read off the *hash*, so it matches what appears inside a nonced value — and neither can
	// be computed from the other, which is the point: the id is safe to put on the wire and
	// this is not.
	//
	// Stored without an ellipsis; the listing adds one. Tokens minted before this field
	// existed have none and cannot grow one — it is not derivable from the hash — and ones
	// minted before the prefix was dropped still carry it, which the listing trims.
	Hint string `json:"hint,omitempty"`
}

// ID names a token without naming its secret or its label.
//
// Derived from the hash rather than stored, so every file that already exists has one and
// nothing can drift out of step. It is not a second name for the token — the label is that,
// and the label is what the log carries. This is for the places that need a stable,
// fixed-width, delimiter-free identifier: inside a nonced value, and beside it in a listing.
func (t Token) ID() string {
	if len(t.Hash) < IDLen {
		return t.Hash
	}
	return t.Hash[:IDLen]
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

	// Minted in a loop so that neither identifier can collide, rather than colliding rarely.
	// Both are checked because they are independent: the id comes off the hash and the hint
	// off the secret, so a fresh secret that clears one can still clash on the other. Two
	// attempts is already beyond astronomical; ten is free.
	var secret, hash, hint string
	for attempt := 0; ; attempt++ {
		var err error
		if secret, err = mint(); err != nil {
			return Token{}, "", err
		}
		hash, hint = hashOf(secret), hintOf(secret)
		id := Token{Hash: hash}.ID()
		taken := slices.ContainsFunc(s.tokens, func(t Token) bool {
			return t.ID() == id || t.Hint == hint
		})
		if !taken {
			break
		}
		if attempt > 10 {
			return Token{}, "", errors.New("could not mint a token with an unused id and hint")
		}
	}
	tok := Token{
		Label:     label,
		Hash:      hash,
		CreatedAt: time.Now().Unix(),
		Hint:      hint,
	}

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

// VerifyNonced checks what a nonced token puts on the wire:
//
//	pxc_<unix seconds>.<token id>.<sha256 hex of "<unix seconds>.<token id>.<sha256 of secret>">
//
// Keyed on the hash the file already holds rather than on the secret, so any token can be used
// this way and nothing has to be stored in the clear. The caller has the secret and derives the
// same key; proxio never needs the secret back.
//
// The key goes **last** in the hashed base, which is what keeps SHA-256's length-extension
// property from mattering: a forged digest would be for a shape proxio never builds. Two things
// are load-bearing rather than tidy — the nonce is parsed strictly as digits, which is what
// keeps the base unambiguous, and the comparison is constant time.
//
// Replay inside the window is possible and accepted, as with Stripe and Slack. The goal is to
// stop the credential travelling, not to make a request unrepeatable; a nonce cache would be
// state that dies on restart.
//
// A false result means the value was not good. The error means the file could not be re-read.
func (s *Store) VerifyNonced(wire string, now time.Time) (string, bool, error) {
	nonce, id, mac, ok := parseNonced(wire)
	if !ok {
		return "", false, nil
	}
	ts, err := strconv.ParseInt(nonce, 10, 64)
	if err != nil {
		return "", false, nil
	}
	if d := now.Sub(time.Unix(ts, 0)); d > Window || d < -Window {
		return "", false, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	rerr := s.refreshLocked()

	label, found := "", false
	for _, t := range s.tokens {
		if t.ID() != id {
			continue
		}
		want := hashOf(nonce + Sep + id + Sep + t.Hash)
		if subtle.ConstantTimeCompare([]byte(mac), []byte(want)) == 1 {
			label, found = t.Label, true
		}
	}
	return label, found, rerr
}

// Sign builds what a caller puts on the wire, from the secret alone.
//
// It exists so proxio can prove its own documentation and the tests need not hand-roll the
// format. There is no command that calls it: signing belongs to the caller, who is the only
// one that should have the secret.
func Sign(secret string, now time.Time) string {
	key := hashOf(secret)
	id := Token{Hash: key}.ID()
	nonce := strconv.FormatInt(now.UTC().Unix(), 10)
	return NoncedPrefix + nonce + Sep + id + Sep + hashOf(nonce+Sep+id+Sep+key)
}

// Nonced reports whether a presented value is the nonced kind, so a caller can pick the check
// from the value's own prefix and the two cannot be confused for one another.
func Nonced(presented string) bool { return strings.HasPrefix(presented, NoncedPrefix) }

// Explain says what is wrong with a nonced value, judged on the value alone.
//
// Shape and window only. It never reads the token list, so it cannot say whether an id exists
// and is no oracle for one, and a value it finds nothing wrong with gets an empty string
// rather than a guess.
//
// Clock skew is the reason it exists: a caller whose clock is five minutes out fails every
// request, and a reply saying "missing or names no token" sends somebody looking in exactly
// the wrong place for an afternoon.
func Explain(wire string, now time.Time) string {
	rest, found := strings.CutPrefix(wire, NoncedPrefix)
	if !found {
		return ""
	}
	parts := strings.Split(rest, Sep)
	if len(parts) != 3 {
		return fmt.Sprintf("a nonced token has three %q-separated fields, this one has %d", Sep, len(parts))
	}
	nonce, id, mac := parts[0], parts[1], parts[2]
	switch {
	case !isDigits(nonce) || len(nonce) > 20:
		return "the nonce is not unix seconds"
	case len(id) != IDLen || !isHex(id):
		return fmt.Sprintf("the id is not %d hex characters", IDLen)
	case len(mac) != sha256.Size*2 || !isHex(mac):
		return fmt.Sprintf("the digest is not %d hex characters", sha256.Size*2)
	}
	ts, err := strconv.ParseInt(nonce, 10, 64)
	if err != nil {
		return "the nonce is not a number"
	}
	if d := now.Sub(time.Unix(ts, 0)); d > Window || d < -Window {
		return fmt.Sprintf("the nonce is %s from now, outside the %s window; check the clock on the caller",
			d.Round(time.Second), Window)
	}
	// Well-formed and in window. Whether the digest matches cannot be answered without
	// reading the token list, and answering it would make this an oracle for which ids exist
	// — so there is nothing further to say. A refusal from here is an unknown or wrong token,
	// which is the same event as a bad raw token and is already logged as one.
	return ""
}

// parseNonced splits a wire value and checks each field's shape, before anything is looked up,
// so the hashed base cannot be made ambiguous by a value that merely looks like one.
func parseNonced(wire string) (nonce, id, mac string, ok bool) {
	rest, found := strings.CutPrefix(wire, NoncedPrefix)
	if !found {
		return "", "", "", false
	}
	parts := strings.Split(rest, Sep)
	if len(parts) != 3 {
		return "", "", "", false
	}
	nonce, id, mac = parts[0], parts[1], parts[2]
	switch {
	case !isDigits(nonce), len(nonce) > 20:
		return "", "", "", false
	case len(id) != IDLen, !isHex(id):
		return "", "", "", false
	case len(mac) != sha256.Size*2, !isHex(mac):
		return "", "", "", false
	}
	return nonce, id, mac, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
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

// hintOf is a secret's first [HintLen] characters after [Prefix].
//
// The prefix is on every secret, so keeping it would spend three of the eight characters
// saying something already known. ASCII throughout — prefix and base64url — so slicing bytes
// cannot split a rune.
func hintOf(secret string) string {
	body := strings.TrimPrefix(secret, Prefix)
	if len(body) <= HintLen {
		return body
	}
	return body[:HintLen]
}

// mint makes a secret. 256 bits from crypto/rand, base64url so it survives a shell, a YAML
// file and a header value without quoting.
func mint() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := randRead(buf); err != nil {
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
