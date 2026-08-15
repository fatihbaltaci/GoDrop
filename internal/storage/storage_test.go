package storage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixedTime pins the clock so identifiers and directory layout are predictable.
var fixedTime = time.Date(2026, 8, 15, 14, 30, 22, 0, time.UTC)

func newTestStore(t *testing.T, maxTotal int64) *Store {
	t.Helper()
	s, err := New(t.TempDir(), maxTotal)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.now = func() time.Time { return fixedTime }
	return s
}

func TestCreateStoresFileUnderDateDirectory(t *testing.T) {
	s := newTestStore(t, 0)
	f, err := s.Create("jpg", strings.NewReader("hello"), 1<<20)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(f.ID, "20260815-143022-") {
		t.Errorf("ID = %q, want the UTC timestamp prefix", f.ID)
	}
	if !ValidID(f.ID) {
		t.Errorf("generated ID %q does not satisfy ValidID", f.ID)
	}
	if f.Size != 5 {
		t.Errorf("Size = %d, want 5", f.Size)
	}
	want := filepath.Join(s.Root(), "2026", "08", "15", f.ID+".jpg")
	if f.Path != want {
		t.Errorf("Path = %q, want %q", f.Path, want)
	}
	data, err := os.ReadFile(f.Path)
	if err != nil || string(data) != "hello" {
		t.Errorf("stored content = %q (%v)", data, err)
	}
	info, err := os.Stat(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %#o, want 0600", perm)
	}
	if files, bytes := s.Stats(); files != 1 || bytes != 5 {
		t.Errorf("Stats = %d files / %d bytes", files, bytes)
	}
}

func TestCreateWithoutExtension(t *testing.T) {
	s := newTestStore(t, 0)
	f, err := s.Create("", strings.NewReader("x"), 1<<20)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.Name() != f.ID {
		t.Errorf("Name = %q, want the bare id", f.Name())
	}
	if _, _, err := s.Open(f.ID, ""); err != nil {
		t.Errorf("Open without extension: %v", err)
	}
}

func TestCreateRejectsOversizedFile(t *testing.T) {
	s := newTestStore(t, 0)
	_, err := s.Create("bin", strings.NewReader(strings.Repeat("a", 101)), 100)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if files, bytes := s.Stats(); files != 0 || bytes != 0 {
		t.Errorf("a rejected upload must leave nothing behind, got %d files / %d bytes", files, bytes)
	}
	entries, _ := os.ReadDir(filepath.Join(s.Root(), "2026", "08", "15"))
	if len(entries) != 0 {
		t.Errorf("partial file left on disk: %v", entries)
	}
}

func TestCreateAcceptsExactlyTheLimit(t *testing.T) {
	s := newTestStore(t, 0)
	if _, err := s.Create("bin", strings.NewReader(strings.Repeat("a", 100)), 100); err != nil {
		t.Fatalf("a file exactly at the limit must be accepted: %v", err)
	}
}

func TestQuotaBlocksWriteBeyondRemainingSpace(t *testing.T) {
	s := newTestStore(t, 10)
	if _, err := s.Create("bin", strings.NewReader("12345"), 1<<20); err != nil {
		t.Fatalf("first write: %v", err)
	}
	_, err := s.Create("bin", strings.NewReader("123456"), 1<<20)
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v, want ErrQuotaExceeded", err)
	}
	if _, bytes := s.Stats(); bytes != 5 {
		t.Errorf("usage = %d, want the failed upload not to count", bytes)
	}
}

func TestQuotaFullRejectsImmediately(t *testing.T) {
	s := newTestStore(t, 5)
	if _, err := s.Create("bin", strings.NewReader("12345"), 1<<20); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := s.Create("bin", strings.NewReader("x"), 1<<20); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v, want ErrQuotaExceeded", err)
	}
}

func TestDeleteFreesQuotaAndPrunesDirectories(t *testing.T) {
	s := newTestStore(t, 100)
	f, err := s.Create("txt", strings.NewReader("hello"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(f.ID, f.Ext); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if files, bytes := s.Stats(); files != 0 || bytes != 0 {
		t.Errorf("Stats after delete = %d/%d", files, bytes)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "2026")); !os.IsNotExist(err) {
		t.Error("empty date directories should be pruned")
	}
	if err := s.Delete(f.ID, f.Ext); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
}

func TestOpenAndDeleteRejectMalformedIdentifiers(t *testing.T) {
	s := newTestStore(t, 0)
	bad := []struct{ id, ext string }{
		{"../../etc/passwd", "jpg"},
		{"20260815-143022-SHORT", "jpg"},
		{"20260815-143022-8F4E2C91B7934B38A72D1C0E5B6A4F3D", "jpg"}, // uppercase hex
		{"20260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3", "jpg"},  // 31 chars
		{"20260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3d", "../x"},
		{"20260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3d", "toolongextension"},
		{"", ""},
	}
	for _, b := range bad {
		if _, _, err := s.Open(b.id, b.ext); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Open(%q,%q) = %v, want ErrInvalidID", b.id, b.ext, err)
		}
		if err := s.Delete(b.id, b.ext); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Delete(%q,%q) = %v, want ErrInvalidID", b.id, b.ext, err)
		}
		if _, err := s.Path(b.id, b.ext); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Path(%q,%q) = %v, want ErrInvalidID", b.id, b.ext, err)
		}
	}
}

func TestOpenMissingFile(t *testing.T) {
	s := newTestStore(t, 0)
	id := "20260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3d"
	if _, _, err := s.Open(id, "jpg"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open = %v, want ErrNotFound", err)
	}
	if err := s.Delete(id, "jpg"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete = %v, want ErrNotFound", err)
	}
}

func TestOpenRejectsDirectory(t *testing.T) {
	s := newTestStore(t, 0)
	id := "20260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3d"
	path, err := s.Path(id, "jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Open(id, "jpg"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a directory in place of a file should read as missing, got %v", err)
	}
}

func TestCreatePropagatesReaderError(t *testing.T) {
	s := newTestStore(t, 0)
	want := errors.New("network died")
	_, err := s.Create("bin", iotest{err: want}, 1<<20)
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want it to wrap %v", err, want)
	}
	if files, _ := s.Stats(); files != 0 {
		t.Error("a failed read must not leave a counted file")
	}
	entries, _ := os.ReadDir(filepath.Join(s.Root(), "2026", "08", "15"))
	if len(entries) != 0 {
		t.Errorf("partial file left behind: %v", entries)
	}
}

func TestCreatePropagatesFinalizeError(t *testing.T) {
	s := newTestStore(t, 0)
	s.finish = func(f *os.File) error {
		_ = f.Close()
		return errors.New("disk went away")
	}
	_, err := s.Create("bin", strings.NewReader("data"), 1<<20)
	if err == nil || !strings.Contains(err.Error(), "finalize file") {
		t.Fatalf("err = %v, want a finalize failure", err)
	}
	if files, _ := s.Stats(); files != 0 {
		t.Error("a file that could not be flushed must not be counted")
	}
}

func TestCreatePropagatesRandomnessError(t *testing.T) {
	s := newTestStore(t, 0)
	s.randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	if _, err := s.Create("bin", strings.NewReader("x"), 1<<20); err == nil {
		t.Fatal("expected an error when the random source fails")
	}
}

func TestCreateRetriesOnIdentifierCollision(t *testing.T) {
	s := newTestStore(t, 0)
	// A random source that repeats its first value forces a collision, which
	// the exclusive create must detect rather than overwrite.
	calls := 0
	s.randRead = func(b []byte) (int, error) {
		calls++
		for i := range b {
			b[i] = byte(calls / 2) // same bytes for two consecutive calls
		}
		return len(b), nil
	}
	first, err := s.Create("bin", strings.NewReader("one"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Create("bin", strings.NewReader("two"), 1<<20)
	if err != nil {
		t.Fatalf("Create after collision: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("collision produced a duplicate identifier")
	}
	if data, _ := os.ReadFile(first.Path); string(data) != "one" {
		t.Errorf("the first file was overwritten: %q", data)
	}
}

func TestCreateGivesUpAfterRepeatedCollisions(t *testing.T) {
	s := newTestStore(t, 0)
	s.randRead = func(b []byte) (int, error) {
		for i := range b {
			b[i] = 0x11
		}
		return len(b), nil
	}
	if _, err := s.Create("bin", strings.NewReader("one"), 1<<20); err != nil {
		t.Fatal(err)
	}
	_, err := s.Create("bin", strings.NewReader("two"), 1<<20)
	if err == nil || !strings.Contains(err.Error(), "unique file id") {
		t.Fatalf("err = %v, want a give-up error after repeated collisions", err)
	}
}

func TestConcurrentCreatesAreUnique(t *testing.T) {
	s := newTestStore(t, 0)
	const n = 50
	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, err := s.Create("bin", strings.NewReader(fmt.Sprint(i)), 1<<20)
			if err != nil {
				errs[i] = err
				return
			}
			ids[i] = f.ID
		}()
	}
	wg.Wait()
	seen := make(map[string]bool, n)
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("concurrent Create: %v", errs[i])
		}
		if seen[ids[i]] {
			t.Fatalf("duplicate id %q", ids[i])
		}
		seen[ids[i]] = true
	}
	if files, _ := s.Stats(); files != n {
		t.Errorf("Stats = %d files, want %d", files, n)
	}
}

func TestNewRescansExistingFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return fixedTime }
	if _, err := s.Create("txt", strings.NewReader("12345"), 1<<20); err != nil {
		t.Fatal(err)
	}
	// A fresh Store over the same directory must recover the counters, which is
	// what makes the quota survive a restart.
	again, err := New(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if files, bytes := again.Stats(); files != 1 || bytes != 5 {
		t.Errorf("rescan = %d files / %d bytes, want 1/5", files, bytes)
	}
}

func TestNewIgnoresBookkeepingFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".install_id"), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := s.Stats(); files != 0 {
		t.Errorf("hidden files must not count towards the quota, got %d", files)
	}
}

func TestNewFailsOnUnusablePath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(filepath.Join(file, "data"), 0); err == nil {
		t.Fatal("expected New to fail when the path cannot be a directory")
	}
}

func TestWritable(t *testing.T) {
	s := newTestStore(t, 0)
	if err := s.Writable(); err != nil {
		t.Fatalf("Writable: %v", err)
	}
	requireNonRoot(t)
	if err := os.Chmod(s.Root(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.Root(), 0o700) })
	if err := s.Writable(); err == nil {
		t.Error("Writable must report a read-only data directory")
	}
}

func TestCleanupRemovesOldFilesOnly(t *testing.T) {
	s := newTestStore(t, 0)
	old, err := s.Create("txt", strings.NewReader("old"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Create("txt", strings.NewReader("fresh"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old.Path, past, past); err != nil {
		t.Fatal(err)
	}
	s.now = time.Now

	removed, freed, err := s.Cleanup(24 * time.Hour)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if removed != 1 || freed != 3 {
		t.Errorf("Cleanup = %d files / %d bytes, want 1/3", removed, freed)
	}
	if _, err := os.Stat(old.Path); !os.IsNotExist(err) {
		t.Error("the expired file should be gone")
	}
	if _, err := os.Stat(fresh.Path); err != nil {
		t.Error("the recent file must survive")
	}
	if files, bytes := s.Stats(); files != 1 || bytes != 5 {
		t.Errorf("counters after cleanup = %d/%d", files, bytes)
	}
}

func TestCleanupDisabled(t *testing.T) {
	s := newTestStore(t, 0)
	if _, err := s.Create("txt", strings.NewReader("keep"), 1<<20); err != nil {
		t.Fatal(err)
	}
	removed, freed, err := s.Cleanup(0)
	if err != nil || removed != 0 || freed != 0 {
		t.Errorf("Cleanup(0) = %d/%d/%v, want a no-op", removed, freed, err)
	}
	if files, _ := s.Stats(); files != 1 {
		t.Error("Cleanup(0) must not delete anything")
	}
}

func TestCleanupPrunesEmptyDirectories(t *testing.T) {
	s := newTestStore(t, 0)
	f, err := s.Create("txt", strings.NewReader("old"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(f.Path, past, past); err != nil {
		t.Fatal(err)
	}
	s.now = time.Now
	if _, _, err := s.Cleanup(time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "2026")); !os.IsNotExist(err) {
		t.Error("empty year directory should have been pruned")
	}
}

func TestCleanupReportsWalkErrors(t *testing.T) {
	requireNonRoot(t)
	s := newTestStore(t, 0)
	if _, err := s.Create("txt", strings.NewReader("x"), 1<<20); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.Root(), "2026", "08", "15")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	s.now = time.Now
	if _, _, err := s.Cleanup(time.Nanosecond); err == nil {
		t.Error("Cleanup should surface an unreadable directory")
	}
}

func TestOrphans(t *testing.T) {
	s := newTestStore(t, 0)
	good, err := s.Create("txt", strings.NewReader("fine"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(good.Path)
	mustWrite(t, filepath.Join(dir, "notanid.txt"), "x")
	mustWrite(t, filepath.Join(dir, good.ID+".txt.bak"), "x")
	mustWrite(t, filepath.Join(dir, "20260101-000000-8f4e2c91b7934b38a72d1c0e5b6a4f3d.txt"), "wrong date dir")
	mustWrite(t, filepath.Join(dir, "20260815-143022-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.txt"), "")
	mustWrite(t, filepath.Join(dir, ".hidden"), "ignored")
	// Service files live in the root, never under a date directory.
	mustWrite(t, filepath.Join(s.Root(), "tokens.json"), `{"tokens":[]}`)
	mustWrite(t, filepath.Join(s.Root(), "notes.txt"), "operator notes")

	orphans, err := s.Orphans()
	if err != nil {
		t.Fatalf("Orphans: %v", err)
	}
	reasons := map[string]string{}
	for _, o := range orphans {
		reasons[filepath.Base(o.Path)] = o.Reason
	}
	if len(orphans) != 4 {
		t.Fatalf("found %d orphans, want 4: %+v", len(orphans), reasons)
	}
	if reasons["notanid.txt"] != "invalid file name" {
		t.Errorf("notanid.txt: %q", reasons["notanid.txt"])
	}
	if reasons["20260101-000000-8f4e2c91b7934b38a72d1c0e5b6a4f3d.txt"] != "wrong directory for its id" {
		t.Errorf("misplaced file not detected: %+v", reasons)
	}
	if reasons["20260815-143022-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.txt"] != "empty file" {
		t.Errorf("empty file not detected: %+v", reasons)
	}
	if _, found := reasons[".hidden"]; found {
		t.Error("hidden bookkeeping files must be ignored")
	}
	for _, name := range []string{"tokens.json", "notes.txt"} {
		if _, found := reasons[name]; found {
			t.Errorf("%s sits in the root and is not an upload; it must not be reported", name)
		}
	}
}

func TestOrphansReportsWalkErrors(t *testing.T) {
	requireNonRoot(t)
	s := newTestStore(t, 0)
	if _, err := s.Create("txt", strings.NewReader("x"), 1<<20); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.Root(), "2026", "08", "15")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := s.Orphans(); err == nil {
		t.Error("Orphans should surface an unreadable directory")
	}
}

func TestQuotaAccessor(t *testing.T) {
	s := newTestStore(t, 4096)
	if s.Quota() != 4096 {
		t.Errorf("Quota = %d", s.Quota())
	}
}

func TestValidExt(t *testing.T) {
	t.Parallel()
	valid := []string{"", "jpg", "png", "tar", "webm", "a1", "1234567890"}
	for _, e := range valid {
		if !ValidExt(e) {
			t.Errorf("ValidExt(%q) = false, want true", e)
		}
	}
	invalid := []string{"JPG", "j pg", "jp/g", "../x", "12345678901", "jpg\x00", "şe"}
	for _, e := range invalid {
		if ValidExt(e) {
			t.Errorf("ValidExt(%q) = true, want false", e)
		}
	}
}

func TestValidID(t *testing.T) {
	t.Parallel()
	if !ValidID("20260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3d") {
		t.Error("a well-formed id was rejected")
	}
	bad := []string{
		"",
		"20260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3",
		"20260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3d2",
		"2026081-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3d",
		"20260815-14302-8f4e2c91b7934b38a72d1c0e5b6a4f3d",
		"20260815_143022_8f4e2c91b7934b38a72d1c0e5b6a4f3d",
		"20260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3d9z",
		"../260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3d",
		"20260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3d\n",
	}
	for _, id := range bad {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true, want false", id)
		}
	}
}

func TestSplitAndJoinName(t *testing.T) {
	t.Parallel()
	id, ext := SplitName("abc.jpg")
	if id != "abc" || ext != "jpg" {
		t.Errorf("SplitName = %q/%q", id, ext)
	}
	if id, ext := SplitName("noext"); id != "noext" || ext != "" {
		t.Errorf("SplitName without a dot = %q/%q", id, ext)
	}
	if id, ext := SplitName("a.tar.gz"); id != "a.tar" || ext != "gz" {
		t.Errorf("SplitName should split on the last dot, got %q/%q", id, ext)
	}
	if got := JoinName("abc", "jpg"); got != "abc.jpg" {
		t.Errorf("JoinName = %q", got)
	}
	if got := JoinName("abc", ""); got != "abc" {
		t.Errorf("JoinName without an extension = %q", got)
	}
	f := File{ID: "abc", Ext: "png"}
	if f.Name() != "abc.png" {
		t.Errorf("File.Name = %q", f.Name())
	}
}

// --------------------------------------------------------------- helpers

type iotest struct{ err error }

func (r iotest) Read([]byte) (int, error) { return 0, r.err }

var _ io.Reader = iotest{}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// requireNonRoot skips permission-based tests when running as root, where
// chmod cannot take away access.
func requireNonRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission checks are meaningless as root")
	}
}
