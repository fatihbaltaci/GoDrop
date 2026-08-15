package storage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests drive the failure paths that only appear when the filesystem
// refuses to cooperate: a read-only directory, a file in the way, a handle that
// is already closed. They are the paths that matter most in production and the
// ones that never run by accident.

func TestFinishFileReportsSyncFailure(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "sync-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// Syncing an already-closed descriptor fails, which is the same shape as a
	// disk that disappears mid-write.
	if err := finishFile(f); err == nil {
		t.Fatal("finishFile should report a failed sync")
	}
}

func TestCreateFailsWhenDirectoryCannotBeCreated(t *testing.T) {
	s := newTestStore(t, 0)
	// A regular file where the year directory belongs makes MkdirAll fail.
	if err := os.WriteFile(filepath.Join(s.Root(), "2026"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := s.Create("bin", strings.NewReader("x"), 1<<20)
	if err == nil || !strings.Contains(err.Error(), "create directory") {
		t.Fatalf("err = %v, want a directory creation failure", err)
	}
}

func TestCreateFailsWhenDirectoryIsReadOnly(t *testing.T) {
	requireNonRoot(t)
	s := newTestStore(t, 0)
	dir := filepath.Join(s.Root(), "2026", "08", "15")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, err := s.Create("bin", strings.NewReader("x"), 1<<20)
	if err == nil || !strings.Contains(err.Error(), "create file") {
		t.Fatalf("err = %v, want a file creation failure", err)
	}
}

func TestOpenSurfacesPermissionErrors(t *testing.T) {
	requireNonRoot(t)
	s := newTestStore(t, 0)
	f, err := s.Create("txt", strings.NewReader("secret"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.Path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.Path, 0o600) })

	_, _, err = s.Open(f.ID, f.Ext)
	if err == nil {
		t.Fatal("expected an error for an unreadable file")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a permission problem must not be reported as a missing file")
	}
}

func TestDeleteSurfacesPermissionErrors(t *testing.T) {
	requireNonRoot(t)
	s := newTestStore(t, 0)
	f, err := s.Create("txt", strings.NewReader("secret"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(f.Path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err = s.Delete(f.ID, f.Ext)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want the underlying permission error", err)
	}
}

func TestDeleteSurfacesStatErrors(t *testing.T) {
	requireNonRoot(t)
	s := newTestStore(t, 0)
	f, err := s.Create("txt", strings.NewReader("secret"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(f.Path)
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := s.Delete(f.ID, f.Ext); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want the underlying stat error", err)
	}
}

func TestNewSurfacesScanErrors(t *testing.T) {
	requireNonRoot(t)
	dir := t.TempDir()
	blocked := filepath.Join(dir, "2026")
	if err := os.MkdirAll(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })

	if _, err := New(dir, 0); err == nil || !strings.Contains(err.Error(), "scan data dir") {
		t.Fatalf("err = %v, want the startup scan to fail loudly", err)
	}
}

func TestPruneStopsAtNonEmptyDirectory(t *testing.T) {
	s := newTestStore(t, 0)
	first, err := s.Create("txt", strings.NewReader("one"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Create("txt", strings.NewReader("two"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(first.ID, first.Ext); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(second.Path)); err != nil {
		t.Error("a directory that still holds files must not be pruned")
	}
}

func TestCleanupSurfacesRemoveErrors(t *testing.T) {
	requireNonRoot(t)
	s := newTestStore(t, 0)
	f, err := s.Create("txt", strings.NewReader("old"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(f.Path, past, past); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(f.Path)
	// Readable but not writable: the walk succeeds, the delete does not.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	s.now = time.Now
	if _, _, err := s.Cleanup(time.Hour); err == nil {
		t.Fatal("Cleanup should report that it could not delete an expired file")
	}
}

// listableButNotStatable makes a directory whose entries can be listed but not
// inspected: read permission without execute. Every tree walk then fails at
// DirEntry.Info(), which is otherwise only reachable by racing a delete.
func listableButNotStatable(t *testing.T, dir string) {
	t.Helper()
	requireNonRoot(t)
	if err := os.Chmod(dir, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

func TestRescanSurfacesEntryInfoErrors(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return fixedTime }
	f, err := s.Create("txt", strings.NewReader("x"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	listableButNotStatable(t, filepath.Dir(f.Path))

	if _, err := New(dir, 0); err == nil {
		t.Fatal("New should fail when a stored file cannot be inspected")
	}
}

func TestCleanupSurfacesEntryInfoErrors(t *testing.T) {
	s := newTestStore(t, 0)
	f, err := s.Create("txt", strings.NewReader("x"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	listableButNotStatable(t, filepath.Dir(f.Path))
	s.now = time.Now

	if _, _, err := s.Cleanup(time.Nanosecond); err == nil {
		t.Fatal("Cleanup should report entries it cannot inspect")
	}
}

func TestOrphansSurfacesEntryInfoErrors(t *testing.T) {
	s := newTestStore(t, 0)
	f, err := s.Create("txt", strings.NewReader("x"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	listableButNotStatable(t, filepath.Dir(f.Path))

	if _, err := s.Orphans(); err == nil {
		t.Fatal("Orphans should report entries it cannot inspect")
	}
}

func TestOpenSurfacesStatErrors(t *testing.T) {
	requireNonRoot(t)
	s := newTestStore(t, 0)
	f, err := s.Create("txt", strings.NewReader("secret"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(f.Path)
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if _, _, err := s.Open(f.ID, f.Ext); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want the underlying stat error", err)
	}
}

func TestStoreUsesTheRealClockByDefault(t *testing.T) {
	s, err := New(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC().Add(-2 * time.Second)
	f, err := s.Create("txt", strings.NewReader("now"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := time.Parse(idLayout, f.ID[:len(idLayout)])
	if err != nil {
		t.Fatalf("identifier %q does not start with a timestamp: %v", f.ID, err)
	}
	if stamp.Before(before) || stamp.After(time.Now().UTC().Add(2*time.Second)) {
		t.Errorf("identifier timestamp %v is not the current UTC time", stamp)
	}
}
