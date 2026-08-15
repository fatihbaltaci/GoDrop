package tokens

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T, env ...string) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := Path(dir)
	s, err := New(path, env)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, path
}

func TestCreateAndVerify(t *testing.T) {
	s, path := newTestStore(t)
	plain, tok, err := s.Create("claude-code")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(plain, Prefix) {
		t.Errorf("token %q should carry the %q prefix so it is recognisable", plain, Prefix)
	}
	if len(plain) != len(Prefix)+32 {
		t.Errorf("token length = %d, want %d hex characters after the prefix", len(plain), 32)
	}
	if tok.Name != "claude-code" {
		t.Errorf("name = %q", tok.Name)
	}

	name, ok := s.Verify(plain)
	if !ok || name != "claude-code" {
		t.Errorf("Verify = %q/%t, want claude-code/true", name, ok)
	}
	if _, ok := s.Verify(plain + "x"); ok {
		t.Error("a modified token must not verify")
	}
	if _, ok := s.Verify(""); ok {
		t.Error("an empty token must not verify")
	}

	// The clear-text value must not be recoverable from disk.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), plain) {
		t.Fatal("the token file contains the clear-text token")
	}
	sum := sha256.Sum256([]byte(plain))
	if !strings.Contains(string(data), hex.EncodeToString(sum[:])) {
		t.Error("the token file should contain the SHA-256 digest")
	}
}

func TestTokenFileIsOwnerOnly(t *testing.T) {
	requirePOSIXModes(t)
	s, path := newTestStore(t)
	if _, _, err := s.Create("x"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %#o, want 0600", perm)
	}
}

func TestEnvironmentTokensVerify(t *testing.T) {
	s, _ := newTestStore(t, "env-token-one", "env-token-two")
	for _, tok := range []string{"env-token-one", "env-token-two"} {
		if _, ok := s.Verify(tok); !ok {
			t.Errorf("environment token %q should verify", tok)
		}
	}
	if _, ok := s.Verify("env-token-three"); ok {
		t.Error("an unknown token must not verify")
	}
	if s.EnvCount() != 2 || s.Count() != 2 {
		t.Errorf("EnvCount/Count = %d/%d, want 2/2", s.EnvCount(), s.Count())
	}
}

func TestCreatedTokenIsUsableWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	// One store stands in for the running server, the other for the CLI.
	server, err := New(Path(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := New(Path(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := cli.Create("agent")
	if err != nil {
		t.Fatal(err)
	}
	// Reload is throttled to once a second; move the server's clock forward
	// instead of sleeping.
	server.now = func() time.Time { return time.Now().Add(time.Hour) }
	if name, ok := server.Verify(plain); !ok || name != "agent" {
		t.Fatal("the running server did not pick up a token created by the CLI")
	}
}

func TestRevokeTakesEffectImmediately(t *testing.T) {
	s, _ := newTestStore(t)
	plain, _, err := s.Create("temporary")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Verify(plain); !ok {
		t.Fatal("token should verify before revocation")
	}
	if err := s.Revoke("TEMPORARY"); err != nil {
		t.Fatalf("Revoke should be case-insensitive: %v", err)
	}
	if _, ok := s.Verify(plain); ok {
		t.Error("a revoked token must stop working at once")
	}
	if err := s.Revoke("temporary"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second revoke = %v, want ErrNotFound", err)
	}
}

func TestDuplicateNamesAreRejected(t *testing.T) {
	s, _ := newTestStore(t)
	if _, _, err := s.Create("ci"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create("CI"); !errors.Is(err, ErrNameExists) {
		t.Errorf("err = %v, want ErrNameExists for a case-insensitive duplicate", err)
	}
}

func TestGeneratedNames(t *testing.T) {
	s, _ := newTestStore(t)
	first, _, err := s.Create("")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := s.Create("")
	if err != nil {
		t.Fatal(err)
	}
	_ = first
	_ = second
	list := s.List()
	if len(list) != 2 || list[0].Name != "token-1" || list[1].Name != "token-2" {
		t.Errorf("generated names = %+v, want token-1 and token-2", list)
	}
}

func TestInvalidNames(t *testing.T) {
	s, _ := newTestStore(t)
	for _, name := range []string{"has space", "sla/sh", "..", "tab\t", strings.Repeat("a", 65), "emoji🙂"} {
		if _, _, err := s.Create(name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Create(%q) = %v, want ErrInvalidName", name, err)
		}
	}
	for _, name := range []string{"ci", "claude-code", "prod_2", "a.b", strings.Repeat("a", 64)} {
		if !ValidName(name) {
			t.Errorf("ValidName(%q) = false, want true", name)
		}
	}
	if ValidName("") {
		t.Error("an empty name must be rejected")
	}
}

func TestListIsSortedAndHidesSecrets(t *testing.T) {
	s, _ := newTestStore(t)
	if _, _, err := s.Create("first"); err != nil {
		t.Fatal(err)
	}
	// Force a later creation timestamp for the second token.
	s.now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, _, err := s.Create("second"); err != nil {
		t.Fatal(err)
	}
	list := s.List()
	if len(list) != 2 || list[0].Name != "first" || list[1].Name != "second" {
		t.Fatalf("List = %+v, want creation order", list)
	}
	if list[0].Hash == "" {
		t.Error("the digest should be present in the record")
	}
}

func TestLastUsedIsRecordedAndFlushed(t *testing.T) {
	s, path := newTestStore(t)
	plain, _, err := s.Create("agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Verify(plain); !ok {
		t.Fatal("token should verify")
	}
	if list := s.List(); list[0].LastUsed == nil {
		t.Error("Verify should record last use in memory")
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "last_used") {
		t.Error("Flush should persist the last-used timestamp")
	}
	// A second flush with nothing pending must be a no-op rather than an error.
	if err := s.Flush(); err != nil {
		t.Fatalf("idle Flush: %v", err)
	}
}

func TestVerifyIgnoresCorruptDigests(t *testing.T) {
	s, path := newTestStore(t)
	if _, _, err := s.Create("good"); err != nil {
		t.Fatal(err)
	}
	// Hand-edited or truncated digests must be skipped, not crash the server.
	if err := os.WriteFile(path, []byte(`{"tokens":[
	  {"name":"broken","hash":"zzzz","created":"2026-01-01T00:00:00Z"},
	  {"name":"short","hash":"abcd","created":"2026-01-01T00:00:00Z"}
	]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, ok := s.Verify("anything"); ok {
		t.Error("a corrupt digest must never match")
	}
}

func TestCorruptTokenFileIsReported(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir)
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path, nil); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("err = %v, want a parse error naming the file", err)
	}
}

func TestUnreadableTokenFileIsReported(t *testing.T) {
	requireStrictPermissions(t)
	dir := t.TempDir()
	path := Path(dir)
	if err := os.WriteFile(path, []byte(`{"tokens":[]}`), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := New(path, nil); err == nil {
		t.Fatal("an unreadable token file should be reported")
	}
}

func TestStatErrorIsReported(t *testing.T) {
	requireStrictPermissions(t)
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
	if _, err := New(filepath.Join(blocked, FileName), nil); err == nil {
		t.Fatal("an unreachable token path should be reported")
	}
}

func TestSaveFailsWhenDirectoryIsNotWritable(t *testing.T) {
	requireStrictPermissions(t)
	dir := t.TempDir()
	s, err := New(Path(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if _, _, err := s.Create("x"); err == nil {
		t.Fatal("Create should fail when the token file cannot be written")
	}
	if s.Count() != 0 {
		t.Error("a token that could not be persisted must not stay in memory")
	}
}

func TestCreateFailsWhenRandomnessFails(t *testing.T) {
	s, _ := newTestStore(t)
	s.randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	if _, _, err := s.Create("x"); err == nil {
		t.Fatal("expected an error when the random source fails")
	}
}

func TestFileReplacedOnDiskIsPickedUp(t *testing.T) {
	s, path := newTestStore(t)
	first, _, err := s.Create("first")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an operator editing the file by hand.
	sum := sha256.Sum256([]byte("hand-written-token"))
	content := `{"tokens":[{"name":"manual","hash":"` + hex.EncodeToString(sum[:]) +
		`","created":"2026-01-01T00:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(time.Hour) }

	if name, ok := s.Verify("hand-written-token"); !ok || name != "manual" {
		t.Error("a hand-edited token file should be honoured")
	}
	if _, ok := s.Verify(first); ok {
		t.Error("a token removed from the file must stop working")
	}
}

func TestReloadIsThrottled(t *testing.T) {
	s, path := newTestStore(t)
	if _, _, err := s.Create("first"); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("late-token"))
	content := `{"tokens":[{"name":"late","hash":"` + hex.EncodeToString(sum[:]) +
		`","created":"2026-01-01T00:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// With the clock frozen, the throttle window has not elapsed, so the new
	// file is not read yet. This is what keeps authentication cheap.
	frozen := time.Now()
	s.now = func() time.Time { return frozen }
	s.checked = frozen
	if _, ok := s.Verify("late-token"); ok {
		t.Error("the store should not re-read the file within the throttle window")
	}
}

func TestConcurrentVerifyIsSafe(t *testing.T) {
	s, _ := newTestStore(t, "env-token")
	plain, _, err := s.Create("agent")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := s.Verify(plain); !ok {
				t.Error("Verify failed under concurrency")
			}
			if _, ok := s.Verify("env-token"); !ok {
				t.Error("environment token failed under concurrency")
			}
			s.List()
		}()
	}
	wg.Wait()
}

func TestMissingFileMeansNoTokens(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Path(dir), nil)
	if err != nil {
		t.Fatalf("a missing token file is normal on first start: %v", err)
	}
	if s.Count() != 0 || len(s.List()) != 0 {
		t.Error("a fresh store should be empty")
	}
}

func TestPath(t *testing.T) {
	if got := Path("/var/lib/godrop"); got != filepath.Join("/var/lib/godrop", FileName) {
		t.Errorf("Path = %q", got)
	}
}

func TestDeletedFileClearsTokens(t *testing.T) {
	s, path := newTestStore(t)
	plain, _, err := s.Create("gone")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, ok := s.Verify(plain); ok {
		t.Error("removing the token file must revoke its tokens")
	}
}

func TestWriteFileAtomicReportsRenameFailures(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	// A directory in the way makes the rename fail after the temporary file has
	// already been written.
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(target, []byte("data")); err == nil {
		t.Fatal("expected the rename to fail")
	}
	if left := tempLeftovers(t, dir); len(left) > 0 {
		t.Errorf("temporary files left behind after a failed rename: %v", left)
	}
}

func TestWriteFileAtomicReportsWriteFailures(t *testing.T) {
	dir := t.TempDir()
	original := writeAll
	writeAll = func(*os.File, []byte) error { return errors.New("disk full") }
	t.Cleanup(func() { writeAll = original })

	if err := writeFileAtomic(filepath.Join(dir, "target"), []byte("data")); err == nil {
		t.Fatal("a failed write should be reported")
	}
	if left := tempLeftovers(t, dir); len(left) > 0 {
		t.Errorf("temporary files left behind after a failed write: %v", left)
	}
}

func TestWriteFileAtomicReportsAnUnusableDirectory(t *testing.T) {
	requireStrictPermissions(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := writeFileAtomic(filepath.Join(dir, "target"), []byte("data")); err == nil {
		t.Fatal("an unwritable directory should be reported")
	}
}

// tempLeftovers lists the temporary files still sitting in dir. A unique name
// is only safe if it is always cleaned up.
func tempLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// corruptFileAfterLoad writes invalid JSON and moves the clock past the reload
// throttle, so the next operation must notice the damage.
func corruptFileAfterLoad(t *testing.T, s *Store, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(time.Hour) }
}

func TestCreateReportsCorruptFile(t *testing.T) {
	s, path := newTestStore(t)
	corruptFileAfterLoad(t, s, path)
	if _, _, err := s.Create("x"); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("err = %v, want Create to refuse to overwrite a corrupt file", err)
	}
}

func TestRevokeReportsCorruptFile(t *testing.T) {
	s, path := newTestStore(t)
	corruptFileAfterLoad(t, s, path)
	if err := s.Revoke("x"); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("err = %v, want Revoke to report the corrupt file", err)
	}
}

func TestListToleratesCorruptFile(t *testing.T) {
	s, path := newTestStore(t)
	if _, _, err := s.Create("kept"); err != nil {
		t.Fatal(err)
	}
	corruptFileAfterLoad(t, s, path)
	// List is informational: it must not panic or lose what it already knows.
	if list := s.List(); len(list) != 1 || list[0].Name != "kept" {
		t.Errorf("List = %+v, want the last good state", list)
	}
}

func TestStoreUsesTheRealClockByDefault(t *testing.T) {
	s, _ := newTestStore(t)
	before := time.Now().UTC().Add(-2 * time.Second)
	_, tok, err := s.Create("agent")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Created.Before(before) || tok.Created.After(time.Now().UTC().Add(2*time.Second)) {
		t.Errorf("Created = %v, want the current UTC time", tok.Created)
	}
}

func TestWriteFailsWhenTheDirectoryCannotBeCreated(t *testing.T) {
	requireStrictPermissions(t)
	parent := t.TempDir()
	// Traversable but not writable: the token file is reported as missing, and
	// then its directory cannot be created.
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	s, err := New(Path(filepath.Join(parent, "sub")), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create("x"); err == nil || !strings.Contains(err.Error(), "create token dir") {
		t.Fatalf("err = %v, want a directory creation failure", err)
	}
}

func TestFlushNeverResurrectsARevokedToken(t *testing.T) {
	// The server and the CLI hold separate views of the same file. A revocation
	// through the CLI must survive the server's next periodic flush, otherwise
	// a token could come back to life moments after being revoked.
	dir := t.TempDir()
	server, err := New(Path(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := New(Path(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := cli.Create("compromised")
	if err != nil {
		t.Fatal(err)
	}

	// The server sees the token and records a use, leaving a pending write.
	server.now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, ok := server.Verify(plain); !ok {
		t.Fatal("the server should accept the new token")
	}

	if err := cli.Revoke("compromised"); err != nil {
		t.Fatal(err)
	}
	if err := server.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	after, err := New(Path(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after.Verify(plain); ok {
		t.Fatal("a revoked token was resurrected by the server's flush")
	}
	if len(after.List()) != 0 {
		t.Errorf("token file = %+v, want empty", after.List())
	}
}

func TestFlushKeepsTheNewestUsage(t *testing.T) {
	dir := t.TempDir()
	server, err := New(Path(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := server.Create("agent")
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Hour)
	server.now = func() time.Time { return later }
	if _, ok := server.Verify(plain); !ok {
		t.Fatal("token should verify")
	}
	if err := server.Flush(); err != nil {
		t.Fatal(err)
	}

	// A second flush with an older timestamp must not move the record backwards.
	server.now = func() time.Time { return later.Add(-time.Hour) }
	if _, ok := server.Verify(plain); !ok {
		t.Fatal("token should verify")
	}
	if err := server.Flush(); err != nil {
		t.Fatal(err)
	}
	list := server.List()
	if len(list) != 1 || list[0].LastUsed == nil || list[0].LastUsed.Before(later) {
		t.Errorf("last used = %+v, want the newest timestamp", list[0].LastUsed)
	}
}

func TestFlushReportsACorruptFile(t *testing.T) {
	s, path := newTestStore(t)
	plain, _, err := s.Create("agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Verify(plain); !ok {
		t.Fatal("token should verify")
	}
	// The file is damaged before the pending write lands.
	if err := os.WriteFile(path, []byte("{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err == nil {
		t.Fatal("Flush should refuse to overwrite a file it cannot read")
	}
}

func TestFlushSkipsTokensThatVanished(t *testing.T) {
	dir := t.TempDir()
	server, err := New(Path(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := New(Path(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	kept, _, err := cli.Create("kept")
	if err != nil {
		t.Fatal(err)
	}
	removed, _, err := cli.Create("removed")
	if err != nil {
		t.Fatal(err)
	}

	server.now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, ok := server.Verify(removed); !ok {
		t.Fatal("token should verify")
	}
	if err := cli.Revoke("removed"); err != nil {
		t.Fatal(err)
	}
	// Only "removed" was used, so after the revocation there is nothing left to
	// write and the flush must be a no-op rather than an error.
	if err := server.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	after, err := New(Path(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after.Verify(kept); !ok {
		t.Error("the surviving token should still work")
	}
	if _, ok := after.Verify(removed); ok {
		t.Error("the revoked token came back")
	}
}

// requireStrictPermissions skips a test that depends on POSIX permission
// semantics. As root every mode is writable anyway, and on Windows chmod only
// toggles a read-only bit, so the situations these tests create cannot exist.
func requireStrictPermissions(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are advisory on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("permission checks are meaningless as root")
	}
}

// requirePOSIXModes skips a test that asserts exact file modes. Windows has no
// POSIX permission bits, so a file created with 0600 does not report 0600.
func requirePOSIXModes(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not POSIX bits on Windows")
	}
}

func TestGenerateMakesAUsableTokenWithoutAStore(t *testing.T) {
	// Under docker compose the token has nowhere to be stored on the host: it
	// goes into .env and the server reads it from the environment.
	first, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	second, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("two generated tokens should differ")
	}
	if !strings.HasPrefix(first, Prefix) || len(first) != len(Prefix)+32 {
		t.Errorf("token = %q, want %s and 128 bits of hex", first, Prefix)
	}
	// It has to be accepted the way an environment token is.
	store, err := New(Path(t.TempDir()), []string{first})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Verify(first); !ok {
		t.Error("a generated token should verify")
	}
}

func TestGenerateReportsAFailingRandomSource(t *testing.T) {
	if _, err := generate(func([]byte) (int, error) { return 0, errors.New("no entropy") }); err == nil {
		t.Error("a token that is not random must not be handed out")
	}
}
