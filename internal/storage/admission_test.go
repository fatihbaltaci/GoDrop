package storage

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// gatedReader holds an upload open at a chosen moment: Create has already put
// the file on disk and reserved its quota by the time started is closed, and
// nothing more happens until release is closed.
type gatedReader struct {
	started chan struct{}
	release chan struct{}
	data    string
	sent    bool
}

func (g *gatedReader) Read(p []byte) (int, error) {
	if g.sent {
		return 0, io.EOF
	}
	close(g.started)
	<-g.release
	g.sent = true
	return copy(p, g.data), nil
}

func newGatedReader(data string) *gatedReader {
	return &gatedReader{started: make(chan struct{}), release: make(chan struct{}), data: data}
}

type createResult struct {
	file *File
	err  error
}

// startUpload begins an upload that is paused inside Create.
func startUpload(s *Store, data string, maxSize int64) (*gatedReader, <-chan createResult) {
	gate := newGatedReader(data)
	done := make(chan createResult, 1)
	go func() {
		f, err := s.Create("txt", gate, maxSize)
		done <- createResult{f, err}
	}()
	<-gate.started
	return gate, done
}

// --------------------------------------------------------------- retention

func TestRetentionNeverDeletesServiceState(t *testing.T) {
	// The token database and the telemetry markers live in the same directory
	// as the uploads. Deleting them by age would revoke every token and turn
	// telemetry back on behind the operator's back.
	s := newTestStore(t, 0)
	upload, err := s.Create("txt", strings.NewReader("old"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	kept := []string{
		filepath.Join(s.Root(), "tokens.json"),
		filepath.Join(s.Root(), ".telemetry-off"),
		filepath.Join(s.Root(), ".install_id"),
		// Not identifier-shaped, so not an upload however old it gets.
		filepath.Join(filepath.Dir(upload.Path), "notes.txt"),
	}
	past := time.Now().Add(-48 * time.Hour)
	for _, path := range kept {
		if err := os.WriteFile(path, []byte("state"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(upload.Path, past, past); err != nil {
		t.Fatal(err)
	}
	s.now = time.Now

	removed, freed, err := s.Cleanup(24 * time.Hour)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if removed != 1 || freed != 3 {
		t.Errorf("Cleanup = %d files / %d bytes, want only the expired upload (1/3)", removed, freed)
	}
	for _, path := range kept {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("retention deleted %s, which is not an upload", filepath.Base(path))
		}
	}
	if files, bytes := s.Stats(); files != 0 || bytes != 0 {
		t.Errorf("counters after cleanup = %d/%d, want 0/0", files, bytes)
	}
}

func TestRetentionLeavesUploadsThatAreStillBeingWritten(t *testing.T) {
	s := newTestStore(t, 0)
	gate, done := startUpload(s, "in flight", 1<<20)

	// From this clock every file in the tree is past the cutoff, so only the
	// in-flight mark can save the one still being streamed.
	s.now = func() time.Time { return time.Now().Add(time.Hour) }
	removed, _, err := s.Cleanup(time.Minute)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if removed != 0 {
		t.Errorf("Cleanup removed %d files while an upload was in flight", removed)
	}

	close(gate.release)
	got := <-done
	if got.err != nil {
		t.Fatalf("Create: %v", got.err)
	}
	if _, err := os.Stat(got.file.Path); err != nil {
		t.Errorf("the finished upload must still be on disk: %v", err)
	}
	if files, bytes := s.Stats(); files != 1 || bytes != int64(len("in flight")) {
		t.Errorf("counters = %d/%d, want the completed upload to be counted once", files, bytes)
	}
}

func TestRetentionNeverFollowsASymbolicLink(t *testing.T) {
	// A link named exactly like an upload must be left alone. Following it
	// would let anything able to write into the data directory aim retention
	// at a file outside the tree.
	s := newTestStore(t, 0)
	upload, err := s.Create("txt", strings.NewReader("real"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("not ours to delete"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(upload.Path), "20260815-143022-"+strings.Repeat("a", 32)+".txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symbolic links are unavailable here: %v", err)
	}

	// Everything in the tree is past the cutoff from this clock, so nothing but
	// the type check stands between the link and os.Remove.
	s.now = func() time.Time { return time.Now().Add(time.Hour) }
	removed, _, err := s.Cleanup(time.Minute)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if removed != 1 {
		t.Errorf("Cleanup removed %d files, want only the real upload", removed)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the link itself should be left alone: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("the file the link points at must survive: %v", err)
	}
}

func TestStartupCountsOnlyUploads(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return fixedTime }
	upload, err := s.Create("txt", strings.NewReader("hello"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(dir, "tokens.json"),
		filepath.Join(filepath.Dir(upload.Path), "stray.bin"),
	} {
		if err := os.WriteFile(path, []byte("not an upload"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	again, err := New(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if files, bytes := again.Stats(); files != 1 || bytes != 5 {
		t.Errorf("Stats after restart = %d files / %d bytes, want only the upload (1/5)", files, bytes)
	}
}

// ------------------------------------------------------------------- quota

func TestConcurrentUploadsNeverExceedTheQuota(t *testing.T) {
	// Each upload used to read the remaining capacity before writing and only
	// charge it afterwards, so overlapping uploads could all spend the same
	// last bytes and the quota was exceeded by a multiple.
	const quota = 4 << 10
	s := newTestStore(t, quota)
	payload := strings.Repeat("x", quota)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = s.Create("bin", strings.NewReader(payload), quota)
		}()
	}
	close(start)
	wg.Wait()

	if _, used := s.Stats(); used > quota {
		t.Errorf("stored %d bytes against a %d byte quota", used, quota)
	}
	var onDisk int64
	err := filepath.WalkDir(s.Root(), func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		onDisk += info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if onDisk > quota {
		t.Errorf("%d bytes on disk against a %d byte quota", onDisk, quota)
	}
}

func TestAnUploadInFlightHoldsItsShareOfTheQuota(t *testing.T) {
	s := newTestStore(t, 10)
	gate, done := startUpload(s, "0123456789", 10)

	if _, err := s.Create("txt", strings.NewReader("x"), 10); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("err = %v, want ErrQuotaExceeded while the whole quota is promised", err)
	}

	close(gate.release)
	if got := <-done; got.err != nil {
		t.Fatalf("the first upload should still succeed: %v", got.err)
	}
}

func TestAFailedUploadReturnsItsQuotaPromise(t *testing.T) {
	s := newTestStore(t, 10)
	if _, err := s.Create("bin", strings.NewReader(strings.Repeat("x", 11)), 100); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v, want ErrQuotaExceeded", err)
	}
	// The promise the failed upload made must not still be held against the
	// quota, or one oversized request would lock the store out permanently.
	if _, err := s.Create("txt", strings.NewReader("small"), 100); err != nil {
		t.Errorf("a later upload should fit: %v", err)
	}
}
