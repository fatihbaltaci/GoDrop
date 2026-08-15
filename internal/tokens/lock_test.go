package tokens

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The server flushes usage timestamps on a timer while `godrop token revoke`
// may be rewriting the same file from another process. Both read, modify and
// write the whole file, so without a lock the slower one renames its stale
// copy over the other's change, and a revocation that was reported as
// successful comes back to life.

// holdLock creates the lock file the way another process would.
func holdLock(t *testing.T, path string) string {
	t.Helper()
	lock := path + ".lock"
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	return lock
}

func TestAnOperationWaitsForALockedFile(t *testing.T) {
	s, path := newTestStore(t)
	lock := holdLock(t, path)

	// Stand in for the other process finishing its write.
	waited := 0
	s.sleep = func(time.Duration) {
		waited++
		if waited == 3 {
			_ = os.Remove(lock)
		}
	}

	if _, _, err := s.Create("after-the-wait"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if waited != 3 {
		t.Errorf("waited %d times, want the store to retry until the lock was released", waited)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Error("the lock should be released once the operation is done")
	}
}

func TestALockNobodyReleasedIsTakenOver(t *testing.T) {
	s, path := newTestStore(t)
	lock := holdLock(t, path)
	// A process killed mid-write leaves this behind; obeying it forever would
	// make every future token operation fail.
	stale := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lock, stale, stale); err != nil {
		t.Fatal(err)
	}
	s.sleep = func(time.Duration) { t.Error("a stale lock should be taken over, not waited for") }

	if _, _, err := s.Create("after-the-crash"); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func TestAHeldLockIsReportedRatherThanIgnored(t *testing.T) {
	s, path := newTestStore(t)
	if _, _, err := s.Create("original"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	holdLock(t, path)
	s.sleep = func(time.Duration) {}

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"Create", func() error { _, _, err := s.Create("second"); return err }},
		{"Revoke", func() error { return s.Revoke("original") }},
		{"Flush", func() error { s.dirty = true; return s.Flush() }},
	} {
		err := tc.call()
		if err == nil || !strings.Contains(err.Error(), "locked by another process") {
			t.Errorf("%s = %v, want it to refuse rather than overwrite", tc.name, err)
		}
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("the token file must be left exactly as it was")
	}
}

func TestLockingReportsAnUnexpectedFailure(t *testing.T) {
	requireStrictPermissions(t)
	s, path := newTestStore(t)
	dir := filepath.Dir(path)
	// Not "already locked": the lock file cannot be created at all.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if _, _, err := s.Create("x"); err == nil || !strings.Contains(err.Error(), "lock token file") {
		t.Fatalf("err = %v, want the underlying failure reported", err)
	}
}

func TestCreateKeepsNothingWhenTheWriteFails(t *testing.T) {
	s, _ := newTestStore(t)
	original := writeAll
	writeAll = func(*os.File, []byte) error { return errors.New("disk full") }
	t.Cleanup(func() { writeAll = original })

	if _, _, err := s.Create("doomed"); err == nil {
		t.Fatal("a token that could not be stored must not be handed out")
	}
	writeAll = original
	if _, ok := s.Verify("anything"); ok {
		t.Error("nothing should verify")
	}
	if s.Count() != 0 {
		t.Errorf("Count = %d, want the failed token rolled back", s.Count())
	}
}

// ------------------------------------------------------- reload failures

func TestReloadFailuresAreReportedOnceEach(t *testing.T) {
	// Reloading fails open on purpose, because an unreadable file must not lock
	// every client out of a working service. The operator still has to hear
	// about it: until it is fixed, a revoked token stays valid.
	s, path := newTestStore(t, "env-token-value")
	var reported []string
	s.SetErrorHandler(func(err error) { reported = append(reported, err.Error()) })

	corruptFileAfterLoad(t, s, path)
	if _, ok := s.Verify("env-token-value"); !ok {
		t.Error("a broken file must not stop the tokens that still work")
	}
	if len(reported) != 1 || !strings.Contains(reported[0], "parse") {
		t.Fatalf("reported = %v, want the parse failure once", reported)
	}

	// The same failure on every request would fill the log.
	s.checked = time.Time{}
	s.Verify("env-token-value")
	if len(reported) != 1 {
		t.Errorf("reported = %v, want the repeat suppressed", reported)
	}

	// Once it is fixed and breaks again, that is news.
	if err := os.WriteFile(path, []byte(`{"tokens":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s.checked = time.Time{}
	s.Verify("env-token-value")
	corruptFileAfterLoad(t, s, path)
	s.checked = time.Time{}
	s.Verify("env-token-value")
	if len(reported) != 2 {
		t.Errorf("reported = %v, want the failure reported again after a recovery", reported)
	}
}

func TestReloadFailuresAreSilentWithoutAHandler(t *testing.T) {
	s, path := newTestStore(t, "env-token-value")
	corruptFileAfterLoad(t, s, path)
	if _, ok := s.Verify("env-token-value"); !ok {
		t.Error("verification should still work")
	}
}
