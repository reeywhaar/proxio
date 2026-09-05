package tokens

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T, dir string) *Store {
	t.Helper()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return st
}

// A fresh volume has no file in it, and that is a state rather than a failure.
func TestMissingFileIsNoTokens(t *testing.T) {
	st := open(t, t.TempDir())

	list, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("List = %v, want empty", list)
	}
	if _, ok, _ := st.Verify("px_anything"); ok {
		t.Error("an empty store accepted a token")
	}
}

func TestCreateThenVerify(t *testing.T) {
	st := open(t, t.TempDir())

	tok, secret, err := st.Create("some_service")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(secret, Prefix) {
		t.Errorf("secret = %q, want the %q prefix", secret, Prefix)
	}
	if strings.Contains(secret, tok.Hash) || tok.Hash == secret {
		t.Error("the stored hash is the secret")
	}

	label, ok, err := st.Verify(secret)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok || label != "some_service" {
		t.Errorf("Verify = %q, %v; want some_service, true", label, ok)
	}
	if _, ok, _ := st.Verify(secret + "x"); ok {
		t.Error("a token with a character appended was accepted")
	}
	if _, ok, _ := st.Verify(""); ok {
		t.Error("an empty token was accepted")
	}
}

// The secret exists once, in the return of Create. Nothing on disk can reconstruct it.
func TestTheFileHoldsNoSecret(t *testing.T) {
	dir := t.TempDir()
	st := open(t, dir)
	_, secret, err := st.Create("some_service")
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("the token file contains the secret")
	}
	if strings.Contains(string(raw), strings.TrimPrefix(secret, Prefix)) {
		t.Fatal("the token file contains the secret without its prefix")
	}
}

func TestDuplicateLabelRefused(t *testing.T) {
	st := open(t, t.TempDir())
	if _, _, err := st.Create("some_service"); err != nil {
		t.Fatal(err)
	}

	_, _, err := st.Create("some_service")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second Create returned %v, want ErrConflict", err)
	}
}

func TestDelete(t *testing.T) {
	st := open(t, t.TempDir())
	_, secret, err := st.Create("some_service")
	if err != nil {
		t.Fatal(err)
	}

	if err := st.Delete("some_service"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := st.Verify(secret); ok {
		t.Error("a deleted token still verifies")
	}
	if err := st.Delete("some_service"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete returned %v, want ErrNotFound", err)
	}
}

func TestDeleteLeavesTheOthers(t *testing.T) {
	st := open(t, t.TempDir())
	_, keep, err := st.Create("keeper")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Create("goner"); err != nil {
		t.Fatal(err)
	}

	if err := st.Delete("goner"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.Verify(keep); !ok {
		t.Error("deleting one token withdrew another")
	}
}

// The reason this package is a file rather than a variable: `docker exec proxio proxio token
// create x` is a second process, and the server has to notice.
func TestASecondProcessIsSeenWithoutARestart(t *testing.T) {
	dir := t.TempDir()
	server := open(t, dir) // the long-running copy, as held by serve
	shell := open(t, dir)  // the one `docker exec` makes

	_, secret, err := shell.Create("some_service")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := server.Verify(secret); !ok {
		t.Fatalf("the server did not see a token created beside it (err %v)", err)
	}

	if err := shell.Delete("some_service"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := server.Verify(secret); ok {
		t.Fatal("the server still honours a token that was deleted beside it")
	}
}

// Two shells, each with its own view, must not overwrite each other's work.
func TestCreateDoesNotClobberATokenItNeverSaw(t *testing.T) {
	dir := t.TempDir()
	first := open(t, dir)
	second := open(t, dir)

	_, a, err := first.Create("alpha")
	if err != nil {
		t.Fatal(err)
	}
	// `second` loaded before alpha existed, so this is the write that would drop it.
	_, b, err := second.Create("beta")
	if err != nil {
		t.Fatal(err)
	}

	fresh := open(t, dir)
	if _, ok, _ := fresh.Verify(a); !ok {
		t.Error("alpha was lost when beta was created")
	}
	if _, ok, _ := fresh.Verify(b); !ok {
		t.Error("beta was not written")
	}
}

// A corrupt file at startup is a configuration failure, and one that comes up anyway would
// refuse every request for a reason nobody could see.
func TestOpenRefusesACorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(dir); err == nil {
		t.Fatal("Open accepted a file that is not JSON")
	}
}

func TestOpenRefusesAFutureVersion(t *testing.T) {
	dir := t.TempDir()
	raw, _ := json.Marshal(map[string]any{"version": fileVersion + 1, "tokens": []Token{}})
	if err := os.WriteFile(filepath.Join(dir, FileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Open(dir)
	if err == nil {
		t.Fatal("Open accepted a file version it does not speak")
	}
	if !strings.Contains(err.Error(), "different proxio") {
		t.Errorf("error = %q, want it to name the version mismatch", err)
	}
}

// Once running, the same corruption is survivable: the tokens already loaded keep working
// and the caller is handed something to log.
func TestCorruptionAfterStartupKeepsTheLastGoodList(t *testing.T) {
	dir := t.TempDir()
	st := open(t, dir)
	_, secret, err := st.Create("some_service")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	label, ok, err := st.Verify(secret)
	if !ok || label != "some_service" {
		t.Error("a corrupt file took a working token away")
	}
	if err == nil {
		t.Error("Verify reported no error, so nothing would be logged about the corruption")
	}

	// And the complaint is made once per change to the file, not once per request.
	if _, _, err := st.Verify(secret); err != nil {
		t.Errorf("the second Verify complained again: %v", err)
	}
}

func TestFileIsPrivate(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, where the mode is not the protection")
	}
	dir := t.TempDir()
	st := open(t, dir)
	if _, _, err := st.Create("some_service"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

// The temporary file the atomic write goes through must not be left behind, or a data
// directory accumulates one per token ever created.
func TestNoTemporaryFilesAreLeftBehind(t *testing.T) {
	dir := t.TempDir()
	st := open(t, dir)
	for _, label := range []string{"a", "b", "c"} {
		if _, _, err := st.Create(label); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != FileName {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("data directory holds %v, want just %s", names, FileName)
	}
}

func TestValidLabel(t *testing.T) {
	good := []string{"a", "some_service", "some-service", "svc.one", "A1"}
	for _, label := range good {
		if err := ValidLabel(label); err != nil {
			t.Errorf("ValidLabel(%q) = %v, want nil", label, err)
		}
	}
	bad := []string{"", "has space", "has\ttab", "has\nnewline", `has"quote`, "emoji😀", strings.Repeat("x", labelMax+1)}
	for _, label := range bad {
		if err := ValidLabel(label); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidLabel(%q) = %v, want ErrInvalid", label, err)
		}
	}
}

func TestListIsOldestFirst(t *testing.T) {
	dir := t.TempDir()
	st := open(t, dir)
	for _, label := range []string{"first", "second", "third"} {
		if _, _, err := st.Create(label); err != nil {
			t.Fatal(err)
		}
	}

	list, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(list))
	for _, tok := range list {
		got = append(got, tok.Label)
	}
	// Created within the same second, so this is really asserting the sort is stable and
	// keeps the order they were written in.
	want := []string{"first", "second", "third"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List = %v, want %v", got, want)
		}
	}
}

func TestSecretsDiffer(t *testing.T) {
	st := open(t, t.TempDir())
	seen := map[string]bool{}
	for _, label := range []string{"a", "b", "c", "d"} {
		_, secret, err := st.Create(label)
		if err != nil {
			t.Fatal(err)
		}
		if seen[secret] {
			t.Fatal("mint produced the same secret twice")
		}
		seen[secret] = true
	}
}

// --- nonced tokens ---------------------------------------------------------------------

func TestSignedValueVerifies(t *testing.T) {
	st := open(t, t.TempDir())
	tok, secret, err := st.Create("some_service")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	wire := Sign(secret, now)
	if !strings.HasPrefix(wire, NoncedPrefix) {
		t.Fatalf("Sign = %q, want the %q prefix", wire, NoncedPrefix)
	}
	// The whole point: the secret is not in what goes on the wire.
	if strings.Contains(wire, secret) || strings.Contains(wire, strings.TrimPrefix(secret, Prefix)) {
		t.Fatal("the signed value contains the secret")
	}
	// Nor is the hash, which is the key.
	if strings.Contains(wire, tok.Hash) {
		t.Fatal("the signed value contains the whole key")
	}

	label, ok, err := st.VerifyNonced(wire, now)
	if err != nil {
		t.Fatalf("VerifyNonced: %v", err)
	}
	if !ok || label != "some_service" {
		t.Fatalf("VerifyNonced = %q, %v; want some_service, true", label, ok)
	}
}

// The id is derived from the hash, so a listing and a wire value name the same token.
func TestTheWireValueCarriesTheID(t *testing.T) {
	st := open(t, t.TempDir())
	tok, secret, err := st.Create("some_service")
	if err != nil {
		t.Fatal(err)
	}

	parts := strings.Split(strings.TrimPrefix(Sign(secret, time.Now()), NoncedPrefix), Sep)
	if len(parts) != 3 {
		t.Fatalf("got %d fields, want 3", len(parts))
	}
	if parts[1] != tok.ID() {
		t.Errorf("wire id = %q, want the token's %q", parts[1], tok.ID())
	}
}

func TestNoncedWindow(t *testing.T) {
	st := open(t, t.TempDir())
	_, secret, err := st.Create("some_service")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	cases := map[string]struct {
		signedAt time.Duration
		want     bool
	}{
		"now":           {0, true},
		"just inside":   {-Window + time.Second, true},
		"just outside":  {-Window - time.Second, false},
		"long expired":  {-time.Hour, false},
		"ahead, inside": {Window - time.Second, true},
		// A clock ahead of proxio's is refused as readily as one behind, or the window is
		// only half a window.
		"ahead, outside":    {Window + time.Second, false},
		"far in the future": {time.Hour, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			wire := Sign(secret, now.Add(c.signedAt))
			_, ok, err := st.VerifyNonced(wire, now)
			if err != nil {
				t.Fatal(err)
			}
			if ok != c.want {
				t.Errorf("VerifyNonced = %v, want %v for a value signed %s from now", ok, c.want, c.signedAt)
			}
		})
	}
}

func TestNoncedRejectsTampering(t *testing.T) {
	st := open(t, t.TempDir())
	tok, secret, err := st.Create("some_service")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	good := Sign(secret, now)
	parts := strings.Split(strings.TrimPrefix(good, NoncedPrefix), Sep)
	nonce, id, mac := parts[0], parts[1], parts[2]

	other := strings.Repeat("0", IDLen)
	if other == id {
		other = strings.Repeat("1", IDLen)
	}
	cases := map[string]string{
		"no prefix":        strings.TrimPrefix(good, NoncedPrefix),
		"wrong prefix":     Prefix + strings.TrimPrefix(good, NoncedPrefix),
		"two fields":       NoncedPrefix + nonce + Sep + id,
		"four fields":      good + Sep + "extra",
		"unknown id":       NoncedPrefix + nonce + Sep + other + Sep + mac,
		"flipped digest":   NoncedPrefix + nonce + Sep + id + Sep + flip(mac),
		"moved nonce":      NoncedPrefix + "1" + nonce + Sep + id + Sep + mac,
		"nonce not digits": NoncedPrefix + "12x4" + Sep + id + Sep + mac,
		"id not hex":       NoncedPrefix + nonce + Sep + "zzzzzzzz" + Sep + mac,
		"short id":         NoncedPrefix + nonce + Sep + id[:4] + Sep + mac,
		"short digest":     NoncedPrefix + nonce + Sep + id + Sep + mac[:32],
		"uppercase digest": NoncedPrefix + nonce + Sep + id + Sep + strings.ToUpper(mac),
		"empty":            NoncedPrefix,
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok, _ := st.VerifyNonced(wire, now); ok {
				t.Errorf("VerifyNonced accepted %q", wire)
			}
		})
	}

	// And the key really is the stored hash rather than the secret, so a digest built over
	// the secret is not accepted by accident.
	overSecret := hashOf(nonce + Sep + id + Sep + secret)
	if _, ok, _ := st.VerifyNonced(NoncedPrefix+nonce+Sep+id+Sep+overSecret, now); ok {
		t.Error("a digest keyed on the secret was accepted")
	}
	_ = tok
}

func flip(hex string) string {
	b := []byte(hex)
	if b[0] == '0' {
		b[0] = '1'
	} else {
		b[0] = '0'
	}
	return string(b)
}

// The two kinds are told apart by prefix, so neither can be presented as the other.
func TestTheTwoKindsAreNotInterchangeable(t *testing.T) {
	st := open(t, t.TempDir())
	_, secret, err := st.Create("some_service")
	if err != nil {
		t.Fatal(err)
	}
	wire := Sign(secret, time.Now())

	if _, ok, _ := st.Verify(wire); ok {
		t.Error("a nonced value was accepted as a bearer token")
	}
	if _, ok, _ := st.VerifyNonced(secret, time.Now()); ok {
		t.Error("a raw secret was accepted as a nonced value")
	}
	// Both still work the way they are meant to.
	if _, ok, _ := st.Verify(secret); !ok {
		t.Error("the raw secret stopped working as a bearer token")
	}
	if _, ok, _ := st.VerifyNonced(wire, time.Now()); !ok {
		t.Error("the nonced value stopped working")
	}
}

// Replay inside the window is accepted, deliberately. A test says so, because somebody will
// otherwise read the absence of one as an oversight.
func TestReplayInsideTheWindowIsAccepted(t *testing.T) {
	st := open(t, t.TempDir())
	_, secret, err := st.Create("some_service")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	wire := Sign(secret, now)

	for i := range 3 {
		if _, ok, _ := st.VerifyNonced(wire, now.Add(time.Duration(i)*time.Second)); !ok {
			t.Fatalf("use %d was refused; replay inside the window is meant to work", i+1)
		}
	}
}

func TestDeletedTokenStopsSigning(t *testing.T) {
	st := open(t, t.TempDir())
	_, secret, err := st.Create("some_service")
	if err != nil {
		t.Fatal(err)
	}
	wire := Sign(secret, time.Now())
	if err := st.Delete("some_service"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.VerifyNonced(wire, time.Now()); ok {
		t.Error("a nonced value for a withdrawn token still verifies")
	}
}

func TestExplainNamesTheReasonWithoutNamingTheToken(t *testing.T) {
	st := open(t, t.TempDir())
	_, secret, err := st.Create("some_service")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	if got := Explain(secret, now); got != "" {
		t.Errorf("Explain(<raw secret>) = %q, want empty — it is not a nonced value", got)
	}
	if got := Explain(Sign(secret, now), now); got != "" {
		t.Errorf("Explain(<good value>) = %q, want empty", got)
	}

	// The reason an operator actually needs: a clock that is out.
	skewed := Explain(Sign(secret, now.Add(-time.Hour)), now)
	if !strings.Contains(skewed, "clock") {
		t.Errorf("Explain(<expired>) = %q, want it to point at the clock", skewed)
	}

	// And it must never become an oracle for which tokens exist: a well-formed, in-window
	// value naming an id that does not exist is indistinguishable from one that does.
	unknown := NoncedPrefix + strconv.FormatInt(now.Unix(), 10) + Sep + strings.Repeat("a", IDLen) + Sep + strings.Repeat("b", 64)
	if got := Explain(unknown, now); got != "" {
		t.Errorf("Explain(<unknown id>) = %q, want empty — it must not say whether the id exists", got)
	}
}

// --- the hint --------------------------------------------------------------------------

func TestHintIsTheSecretsFirstCharacters(t *testing.T) {
	st := open(t, t.TempDir())
	tok, secret, err := st.Create("some_service")
	if err != nil {
		t.Fatal(err)
	}
	if len(tok.Hint) != HintLen {
		t.Fatalf("hint = %q, want %d characters", tok.Hint, HintLen)
	}
	// After the prefix, not including it. Every secret carries the prefix, so eight
	// characters counted from the front would be five doing the work.
	if strings.HasPrefix(tok.Hint, Prefix) {
		t.Errorf("hint = %q, which spends characters on the prefix every token has", tok.Hint)
	}
	body := strings.TrimPrefix(secret, Prefix)
	if !strings.HasPrefix(body, tok.Hint) {
		t.Errorf("hint %q is not the start of the secret's random part %q", tok.Hint, body)
	}
	// It is an identifier, not a shortcut: what is left is still unguessable.
	if len(body)-len(tok.Hint) < 32 {
		t.Errorf("the hint gives away too much of a %d-character secret", len(body))
	}
}

// Both identifiers are unique, and they are independent — a fresh secret that clears one can
// still clash on the other, which is why the minting loop checks both.
func TestBothIdentifiersAreUnique(t *testing.T) {
	st := open(t, t.TempDir())

	first, firstSecret, err := st.Create("first")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(firstSecret, Prefix))
	if err != nil {
		t.Fatal(err)
	}

	// Force the next mint to reproduce the first token's secret exactly, then fall back to
	// real randomness. Without the loop this writes a second row with the same id and the
	// same hint as the first.
	used := false
	realRand := randRead
	randRead = func(b []byte) (int, error) {
		if !used {
			used = true
			copy(b, raw)
			return len(b), nil
		}
		return realRand(b)
	}
	t.Cleanup(func() { randRead = realRand })

	second, secondSecret, err := st.Create("second")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !used {
		t.Fatal("the test never reached the minting loop, so it proved nothing")
	}
	if secondSecret == firstSecret {
		t.Fatal("the collision was accepted rather than re-minted")
	}
	if second.ID() == first.ID() {
		t.Error("two tokens share an id")
	}
	if second.Hint == first.Hint {
		t.Error("two tokens share a hint")
	}
	// And both still work, which is what a re-mint has to leave true.
	for name, secret := range map[string]string{"first": firstSecret, "second": secondSecret} {
		if _, ok, _ := st.Verify(secret); !ok {
			t.Errorf("the %s token does not verify", name)
		}
	}
}

// The hint check on its own is load-bearing, and a duplicated whole secret cannot prove it —
// that trips the id check too. Base64url packs 3 bytes into 4 characters, so the eight
// characters after the prefix come from exactly the first six random bytes: copy those and let
// the rest be fresh, and the result collides on the hint while its hash is unrelated.
func TestAHintCollisionAloneIsReMinted(t *testing.T) {
	st := open(t, t.TempDir())

	first, firstSecret, err := st.Create("first")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(firstSecret, Prefix))
	if err != nil {
		t.Fatal(err)
	}

	attempts := 0
	realRand := randRead
	randRead = func(b []byte) (int, error) {
		n, err := realRand(b)
		if attempts == 0 {
			attempts++
			// Six bytes is HintLen base64 characters exactly. Fewer would fix only part of
			// the hint, and the test would pass without ever forcing a collision.
			copy(b[:6], raw[:6])
		}
		return n, err
	}
	t.Cleanup(func() { randRead = realRand })

	second, secondSecret, err := st.Create("second")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if attempts == 0 {
		t.Fatal("the test never reached the minting loop")
	}
	if second.ID() == first.ID() {
		t.Fatal("the forced secret collided on the id too, so this proves nothing about the hint")
	}
	if second.Hint == first.Hint {
		t.Errorf("two tokens share the hint %q; only the id is being checked", second.Hint)
	}
	if _, ok, _ := st.Verify(secondSecret); !ok {
		t.Error("the re-minted token does not verify")
	}
}

// And the id check on its own, which a duplicated secret cannot prove either — that trips the
// hint check.
//
// A real hash prefix collision is a 32-bit search, so it is arranged rather than found: a file
// is seeded with the hash a known secret would be stored under, but with a hint that
// deliberately does not match it. Minting that same secret can then collide on the id and on
// nothing else.
func TestAnIDCollisionAloneIsReMinted(t *testing.T) {
	dir := t.TempDir()

	raw := make([]byte, secretBytes)
	if _, err := randRead(raw); err != nil {
		t.Fatal(err)
	}
	secret := Prefix + base64.RawURLEncoding.EncodeToString(raw)
	hash := hashOf(secret)
	seededID := Token{Hash: hash}.ID()

	seeded := `{"version":1,"tokens":[{"label":"seeded","hash":"` + hash +
		`","created_at":1,"hint":"ZZZZZZZZ"}]}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(seeded), 0o600); err != nil {
		t.Fatal(err)
	}
	st := open(t, dir)

	used := false
	realRand := randRead
	randRead = func(b []byte) (int, error) {
		if !used {
			used = true
			copy(b, raw)
			return len(b), nil
		}
		return realRand(b)
	}
	t.Cleanup(func() { randRead = realRand })

	minted, mintedSecret, err := st.Create("fresh")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !used {
		t.Fatal("the test never reached the minting loop")
	}
	if minted.Hint == "ZZZZZZZZ" {
		t.Fatal("the forced secret collided on the hint too, so this proves nothing about the id")
	}
	if minted.ID() == seededID {
		t.Errorf("two tokens share the id %q; only the hint is being checked", seededID)
	}
	if mintedSecret == secret {
		t.Error("the colliding secret was written down rather than re-minted")
	}
}

func TestAFileWithoutHintsStillLoads(t *testing.T) {
	dir := t.TempDir()
	raw := `{"version":1,"tokens":[{"label":"old","hash":"` + strings.Repeat("a", 64) + `","created_at":1757116800}]}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	st := open(t, dir)
	list, err := st.List()
	if err != nil {
		t.Fatalf("a file from before hints existed did not load: %v", err)
	}
	if len(list) != 1 || list[0].Hint != "" {
		t.Fatalf("list = %+v, want one token with no hint", list)
	}
	// The id is derived, so an old token has one anyway.
	if list[0].ID() != strings.Repeat("a", IDLen) {
		t.Errorf("id = %q, want it derived from the hash", list[0].ID())
	}
}
