// Package storage persists uploaded files on the local filesystem.
//
// There is no database. A file's identifier carries everything needed to find
// it again: "20260815-143022-<32 hex>" maps to <root>/2026/08/15/<id>.<ext>.
// The 128 random bits make identifiers unguessable, the timestamp prefix keeps
// directories small and makes retention a directory-level operation, and the
// extension is the only metadata we keep. The MIME type is derived from it at
// download time.
package storage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Errors returned by Store operations.
var (
	ErrNotFound      = errors.New("file not found")
	ErrTooLarge      = errors.New("file exceeds the maximum allowed size")
	ErrQuotaExceeded = errors.New("storage quota exceeded")
	ErrInvalidID     = errors.New("invalid file id")
)

// idPattern matches "<YYYYMMDD>-<HHMMSS>-<32 lowercase hex>".
var idPattern = regexp.MustCompile(`^(\d{8})-(\d{6})-[0-9a-f]{32}$`)

// idLayout is the time layout used for the identifier prefix (always UTC).
const idLayout = "20060102-150405"

// File describes a stored file.
type File struct {
	ID   string // full identifier, without extension
	Ext  string // extension without a leading dot; may be empty
	Size int64
	Path string // absolute path on disk
}

// Name returns the on-disk file name ("<id>.<ext>" or "<id>").
func (f File) Name() string { return JoinName(f.ID, f.Ext) }

// JoinName builds a file name from an identifier and an extension.
func JoinName(id, ext string) string {
	if ext == "" {
		return id
	}
	return id + "." + ext
}

// Store is a filesystem-backed file store. It is safe for concurrent use.
type Store struct {
	root     string
	maxTotal int64 // 0 means unlimited

	used  atomic.Int64
	count atomic.Int64

	// Admission state for uploads in progress. Quota is promised before the
	// bytes are written and the file is marked until it is committed: without
	// the promise two concurrent uploads would each spend the same remaining
	// capacity, and without the mark retention could unlink a file that is
	// still being streamed.
	mu       sync.Mutex
	reserved int64
	writing  map[string]struct{}

	// Seams, replaced in tests to exercise error paths deterministically.
	now      func() time.Time
	randRead func([]byte) (int, error)
	finish   func(*os.File) error
}

// finishFile flushes a freshly written file to disk and closes it. A failure
// here means the bytes may not have survived, so the caller discards the file
// rather than handing out a URL for it.
func finishFile(f *os.File) error {
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// New opens (creating if needed) a store rooted at dir and scans it once to
// establish the current usage counters.
func New(dir string, maxTotal int64) (*Store, error) {
	// An absolute root keeps log lines and error messages unambiguous. Abs only
	// fails when the working directory itself is gone, in which case the
	// cleaned path is still a perfectly usable root.
	root := filepath.Clean(dir)
	if abs, err := filepath.Abs(dir); err == nil {
		root = abs
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	s := &Store{
		root:     root,
		maxTotal: maxTotal,
		writing:  make(map[string]struct{}),
		now:      func() time.Time { return time.Now().UTC() },
		randRead: rand.Read,
		finish:   finishFile,
	}
	if err := s.rescan(); err != nil {
		return nil, err
	}
	return s, nil
}

// Root returns the absolute root directory of the store.
func (s *Store) Root() string { return s.root }

// Stats returns the number of stored files and the bytes they occupy.
func (s *Store) Stats() (files, bytes int64) { return s.count.Load(), s.used.Load() }

// Quota returns the configured quota in bytes (0 means unlimited).
func (s *Store) Quota() int64 { return s.maxTotal }

// Writable reports whether the store's root is currently writable. It is used
// by the readiness probe to catch unmounted volumes and read-only filesystems,
// which are the most common deployment failures.
func (s *Store) Writable() error {
	f, err := os.CreateTemp(s.root, ".probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	closeErr := f.Close()
	rmErr := os.Remove(name)
	return errors.Join(closeErr, rmErr)
}

// Create streams r into a new file with the given extension. At most maxSize
// bytes are accepted; anything beyond that aborts the write and returns
// ErrTooLarge. When a quota is configured and the write would exceed it, the
// write is aborted with ErrQuotaExceeded. Partial writes never survive.
func (s *Store) Create(ext string, r io.Reader, maxSize int64) (*File, error) {
	limit, err := s.reserve(maxSize)
	if err != nil {
		return nil, err
	}
	defer s.release(limit)

	id, f, err := s.createUnique(ext)
	if err != nil {
		return nil, err
	}
	path := f.Name()
	s.startWriting(path)
	defer s.stopWriting(path)

	// Read one byte past the limit so we can tell "exactly at the limit" from
	// "over the limit" without buffering the whole body.
	written, copyErr := io.Copy(f, io.LimitReader(r, limit+1))
	finishErr := s.finish(f)

	switch {
	case copyErr != nil:
		s.discard(path)
		return nil, fmt.Errorf("write file: %w", copyErr)
	case written > limit:
		s.discard(path)
		if limit < maxSize {
			return nil, ErrQuotaExceeded
		}
		return nil, ErrTooLarge
	case finishErr != nil:
		s.discard(path)
		return nil, fmt.Errorf("finalize file: %w", finishErr)
	}

	s.used.Add(written)
	s.count.Add(1)
	return &File{ID: id, Ext: ext, Size: written, Path: path}, nil
}

// reserve promises want bytes to an upload that is about to start and returns
// how many it may write. Charging the quota before the bytes exist is what
// stops concurrent uploads from each spending the last of it: the check and
// the charge happen together, and the promise is given back in release once
// the real size is known.
func (s *Store) reserve(want int64) (int64, error) {
	if s.maxTotal <= 0 {
		return want, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	remaining := s.maxTotal - s.used.Load() - s.reserved
	if remaining <= 0 {
		return 0, ErrQuotaExceeded
	}
	if remaining < want {
		want = remaining
	}
	s.reserved += want
	return want, nil
}

// release returns an unused promise. Create calls it after the written bytes
// have been added to the total, so the accounting never dips below the truth.
func (s *Store) release(n int64) {
	if s.maxTotal <= 0 {
		return
	}
	s.mu.Lock()
	s.reserved -= n
	s.mu.Unlock()
}

// startWriting marks a file as being streamed; stopWriting clears the mark.
func (s *Store) startWriting(path string) {
	s.mu.Lock()
	s.writing[path] = struct{}{}
	s.mu.Unlock()
}

func (s *Store) stopWriting(path string) {
	s.mu.Lock()
	delete(s.writing, path)
	s.mu.Unlock()
}

func (s *Store) isWriting(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.writing[path]
	return ok
}

// createUnique reserves a fresh identifier by creating the target file
// exclusively, so two concurrent uploads can never claim the same name.
func (s *Store) createUnique(ext string) (string, *os.File, error) {
	for attempt := 0; attempt < 5; attempt++ {
		id, err := s.newID()
		if err != nil {
			return "", nil, err
		}
		path := s.pathFor(id, ext)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", nil, fmt.Errorf("create directory: %w", err)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return id, f, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", nil, fmt.Errorf("create file: %w", err)
		}
	}
	return "", nil, errors.New("could not allocate a unique file id")
}

func (s *Store) discard(path string) { _ = os.Remove(path) }

// Open returns a readable handle for the given identifier and extension.
func (s *Store) Open(id, ext string) (*os.File, fs.FileInfo, error) {
	path, err := s.Path(id, ext)
	if err != nil {
		return nil, nil, err
	}
	// Stat before opening: it distinguishes "gone" from "unreadable" and yields
	// the metadata the download handler needs, so the open path stays simple.
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}
	if info.IsDir() {
		return nil, nil, ErrNotFound
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return f, info, nil
}

// Delete removes a stored file. It returns ErrNotFound if it does not exist.
func (s *Store) Delete(id, ext string) error {
	path, err := s.Path(id, ext)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	s.used.Add(-info.Size())
	s.count.Add(-1)
	s.pruneEmptyDirs(filepath.Dir(path))
	return nil
}

// Path validates the identifier and returns the absolute path it maps to. The
// identifier alone determines the path: user-supplied names never take part in
// path construction, which is what makes traversal structurally impossible.
func (s *Store) Path(id, ext string) (string, error) {
	if !ValidID(id) {
		return "", ErrInvalidID
	}
	if !ValidExt(ext) {
		return "", ErrInvalidID
	}
	return s.pathFor(id, ext), nil
}

func (s *Store) pathFor(id, ext string) string {
	// idPattern guarantees the layout, so slicing is safe here.
	return filepath.Join(s.root, id[0:4], id[4:6], id[6:8], JoinName(id, ext))
}

func (s *Store) newID() (string, error) {
	buf := make([]byte, 16)
	if _, err := s.randRead(buf); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return s.now().UTC().Format(idLayout) + "-" + hex.EncodeToString(buf), nil
}

// ValidID reports whether s is a well-formed GoDrop identifier.
func ValidID(s string) bool { return idPattern.MatchString(s) }

// ValidExt reports whether ext is an acceptable stored extension: lowercase
// alphanumerics, at most 10 characters, or empty.
func ValidExt(ext string) bool {
	if ext == "" {
		return true
	}
	if len(ext) > MaxExtLen {
		return false
	}
	for _, r := range ext {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// MaxExtLen bounds the stored extension length.
const MaxExtLen = 10

// Cleanup removes uploads older than age and prunes the directories left
// empty. It returns the number of files removed and the bytes reclaimed.
//
// Only recognisable uploads are ever deleted. The service keeps its own state
// in the same directory (the token database, the telemetry markers) and age
// alone would eventually sweep those away, revoking every token or quietly
// turning telemetry back on. Files still being written are left alone too.
func (s *Store) Cleanup(age time.Duration) (removed int, freed int64, err error) {
	if age <= 0 {
		return 0, 0, nil
	}
	cutoff := s.now().Add(-age)
	var dirs []string
	walkErr := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != s.root {
				dirs = append(dirs, path)
			}
			return nil
		}
		if !s.isUpload(path, d) || s.isWriting(path) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		// isUpload has established that this is a regular file at the exact
		// path its own identifier maps to, so there is no symlink to follow
		// and nothing outside the tree to reach.
		if err := os.Remove(path); err != nil { //nolint:gosec // G122
			return err
		}
		removed++
		freed += info.Size()
		s.used.Add(-info.Size())
		s.count.Add(-1)
		return nil
	})
	if walkErr != nil {
		return removed, freed, walkErr
	}
	// Deepest first, so parents become empty before we try to remove them.
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i])
	}
	return removed, freed, nil
}

// isUpload reports whether the entry is a stored file sitting at the exact
// path its own identifier maps to. WalkDir reports symbolic links as such
// rather than following them, and only regular files qualify, so nothing
// outside the tree can be reached through one.
func (s *Store) isUpload(path string, d fs.DirEntry) bool {
	if !d.Type().IsRegular() {
		return false
	}
	id, ext := SplitName(d.Name())
	return ValidID(id) && ValidExt(ext) && s.pathFor(id, ext) == path
}

// rescan recomputes the usage counters by walking the tree once at startup.
// The counters describe stored uploads, so anything else in the tree (the
// token database, a stray file an operator copied in) is left out of them
// and reported by `godrop doctor` instead.
func (s *Store) rescan() error {
	var files, bytes int64
	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !s.isUpload(path, d) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files++
		bytes += info.Size()
		return nil
	})
	if err != nil {
		return fmt.Errorf("scan data dir: %w", err)
	}
	s.count.Store(files)
	s.used.Store(bytes)
	return nil
}

// pruneEmptyDirs removes now-empty date directories up to the store root.
func (s *Store) pruneEmptyDirs(dir string) {
	for dir != s.root && strings.HasPrefix(dir, s.root) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// Orphan describes a file that does not belong to the store's layout. Doctor
// reports these; they are usually crash leftovers or manual copies.
type Orphan struct {
	Path   string
	Reason string
	Size   int64
}

// Orphans scans the tree for files that break the naming or layout rules.
func (s *Store) Orphans() ([]Orphan, error) {
	var out []Orphan
	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		name := d.Name()
		id, ext := SplitName(name)
		switch {
		case strings.HasPrefix(name, "."):
			return nil // bookkeeping files such as .install_id
		case filepath.Dir(path) == s.root && !ValidID(id):
			// Stored files always live under a date directory, so anything in
			// the root that is not identifier-shaped belongs to the service
			// itself: the token database, for instance.
			return nil
		case !ValidID(id) || !ValidExt(ext):
			out = append(out, Orphan{Path: path, Reason: "invalid file name", Size: info.Size()})
		case s.pathFor(id, ext) != path:
			out = append(out, Orphan{Path: path, Reason: "wrong directory for its id", Size: info.Size()})
		case info.Size() == 0:
			out = append(out, Orphan{Path: path, Reason: "empty file", Size: 0})
		}
		return nil
	})
	return out, err
}

// SplitName splits "<id>.<ext>" into its parts. A name without a dot yields an
// empty extension.
func SplitName(name string) (id, ext string) {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return name, ""
	}
	return name[:i], name[i+1:]
}
