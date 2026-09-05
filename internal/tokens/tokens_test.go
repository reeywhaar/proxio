package tokens

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
