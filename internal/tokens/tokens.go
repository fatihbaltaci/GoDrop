// Package tokens manages the API tokens that authorise uploads and deletes.
//
// Tokens are never stored in clear text. The server only ever needs to answer
// "is this token valid?", so it keeps SHA-256 digests: a leaked tokens.json
// cannot be turned back into a working token, and — unlike machine-bound
// encryption — the file survives being restored onto a different host.
//
// A plain digest is the right choice here (rather than bcrypt or argon2)
// because tokens are 128-bit random values, not human-chosen passwords: there
// is nothing to brute force.
package tokens

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Prefix marks GoDrop tokens so they are recognisable in logs, secret scanners
// and pasted snippets.
const Prefix = "gd_"

// FileName is the token database file, stored inside the data directory.
const FileName = "tokens.json"

// Errors returned by Store operations.
var (
	ErrNotFound    = errors.New("token not found")
	ErrNameExists  = errors.New("a token with that name already exists")
	ErrInvalidName = errors.New("invalid token name: use letters, digits, dash, underscore or dot (max 64)")
)

// reloadInterval bounds how often the store stats the token file. Auth happens
// on every upload, and a stat per request would be wasteful.
const reloadInterval = time.Second

// Lock retry policy. Holders keep the lock only for as long as it takes to
// read a small file, rewrite it and rename it into place, so a short spin is
// enough; a lock nobody has touched for lockStaleAfter belonged to a process
// that died and is taken over.
const (
	lockRetryInterval = 20 * time.Millisecond
	lockAttempts      = 100
	lockStaleAfter    = 30 * time.Second
)

// Token is a stored token record. The token itself is not part of it.
type Token struct {
	Name     string     `json:"name"`
	Hash     string     `json:"hash"`
	Created  time.Time  `json:"created"`
	LastUsed *time.Time `json:"last_used,omitempty"`
}

type fileFormat struct {
	Tokens []Token `json:"tokens"`
}

// Store holds tokens from two sources: the environment (GODROP_TOKENS, which
// suits Docker, Fly and Railway) and tokens.json (managed by `godrop token`).
// Both are accepted; the file can be edited while the server runs and is picked
// up without a restart.
type Store struct {
	path string

	mu       sync.RWMutex
	env      []envToken
	tokens   []Token
	modTime  time.Time
	size     int64
	checked  time.Time
	dirty    bool
	lastErr  string
	onError  func(error)
	now      func() time.Time
	randRead func([]byte) (int, error)
	sleep    func(time.Duration)
}

type envToken struct {
	name string
	sum  [32]byte
}

// New creates a store backed by path (may not exist yet) and the given
// environment tokens.
func New(path string, envTokens []string) (*Store, error) {
	s := &Store{
		path:     path,
		now:      func() time.Time { return time.Now().UTC() },
		randRead: rand.Read,
		sleep:    time.Sleep,
	}
	for i, t := range envTokens {
		s.env = append(s.env, envToken{name: fmt.Sprintf("env-%d", i+1), sum: sha256.Sum256([]byte(t))})
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Count returns how many tokens are usable (environment plus file).
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.env) + len(s.tokens)
}

// Verify reports whether presented is a valid token and returns the name it is
// known by. Every candidate is compared in constant time and the loop never
// exits early, so response timing does not reveal how far a guess got.
func (s *Store) Verify(presented string) (string, bool) {
	if presented == "" {
		return "", false
	}
	s.maybeReload()
	sum := sha256.Sum256([]byte(presented))

	s.mu.Lock()
	defer s.mu.Unlock()

	name := ""
	matched := 0
	for _, t := range s.env {
		if subtle.ConstantTimeCompare(sum[:], t.sum[:]) == 1 {
			name, matched = t.name, 1
		}
	}
	idx := -1
	for i, t := range s.tokens {
		stored, err := hex.DecodeString(t.Hash)
		if err != nil || len(stored) != len(sum) {
			continue
		}
		if subtle.ConstantTimeCompare(sum[:], stored) == 1 {
			name, matched, idx = t.Name, 1, i
		}
	}
	if matched == 0 {
		return "", false
	}
	if idx >= 0 {
		now := s.now()
		s.tokens[idx].LastUsed = &now
		s.dirty = true
	}
	return name, true
}

// Create generates a new token, stores its digest and returns the clear-text
// value — the only time it is ever available.
func (s *Store) Create(name string) (string, Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	unlock, err := s.lockFile()
	if err != nil {
		return "", Token{}, err
	}
	defer unlock()

	if err := s.reloadLocked(); err != nil {
		return "", Token{}, err
	}
	if name == "" {
		name = s.uniqueNameLocked("token")
	}
	if !ValidName(name) {
		return "", Token{}, ErrInvalidName
	}
	for _, t := range s.tokens {
		if strings.EqualFold(t.Name, name) {
			return "", Token{}, ErrNameExists
		}
	}

	buf := make([]byte, 16)
	if _, err := s.randRead(buf); err != nil {
		return "", Token{}, fmt.Errorf("generate token: %w", err)
	}
	plain := Prefix + hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(plain))
	tok := Token{Name: name, Hash: hex.EncodeToString(sum[:]), Created: s.now()}
	s.tokens = append(s.tokens, tok)
	if err := s.saveLocked(); err != nil {
		s.tokens = s.tokens[:len(s.tokens)-1]
		return "", Token{}, err
	}
	return plain, tok, nil
}

// List returns the stored tokens (digests included, clear text never).
func (s *Store) List() []Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reloadLocked()
	out := make([]Token, len(s.tokens))
	copy(out, s.tokens)
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// EnvCount returns how many tokens come from the environment. They cannot be
// revoked through the CLI, which matters for the wording of error messages.
func (s *Store) EnvCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.env)
}

// Revoke deletes a token by name. The change takes effect immediately.
func (s *Store) Revoke(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.reloadLocked(); err != nil {
		return err
	}
	for i, t := range s.tokens {
		if strings.EqualFold(t.Name, name) {
			s.tokens = append(s.tokens[:i], s.tokens[i+1:]...)
			return s.saveLocked()
		}
	}
	return ErrNotFound
}

// Flush writes pending last-used timestamps, if any. The server calls this on a
// timer and at shutdown so that recording usage costs no disk I/O per request.
//
// It re-reads the file first and only updates tokens that are still there. The
// server and the CLI hold separate views of the same file, and writing this
// process's view wholesale would resurrect a token that `godrop token revoke`
// had just removed.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}

	pending := make(map[string]time.Time, len(s.tokens))
	for _, t := range s.tokens {
		if t.LastUsed != nil {
			pending[strings.ToLower(t.Name)] = *t.LastUsed
		}
	}

	unlock, err := s.lockFile()
	if err != nil {
		return err
	}
	defer unlock()

	s.size = -1 // force a re-read even within the throttle window
	if err := s.reloadLocked(); err != nil {
		return err
	}

	changed := false
	for i := range s.tokens {
		used, ok := pending[strings.ToLower(s.tokens[i].Name)]
		if !ok {
			continue
		}
		if s.tokens[i].LastUsed == nil || s.tokens[i].LastUsed.Before(used) {
			stamp := used
			s.tokens[i].LastUsed = &stamp
			changed = true
		}
	}
	if !changed {
		s.dirty = false
		return nil
	}
	return s.saveLocked()
}

// lockFile takes an exclusive lock on the token file and returns the function
// that releases it.
//
// The running server and `godrop token` are separate processes with separate
// views of the same file. Without a lock one can read it, be overtaken by the
// other's write, and then rename its own stale copy into place — silently
// undoing a revocation that was reported as successful. Callers already hold
// the in-process mutex, so only one goroutine per process ever waits here.
func (s *Store) lockFile() (func(), error) {
	lock := s.path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		return nil, fmt.Errorf("create token dir: %w", err)
	}
	for range lockAttempts {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(lock) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("lock token file: %w", err)
		}
		// A process killed mid-write would otherwise block every future token
		// operation, so a lock nobody has touched is taken over. The clock here
		// is the filesystem's, not the store's: it is compared against a real
		// modification time.
		if info, statErr := os.Stat(lock); statErr == nil && time.Since(info.ModTime()) > lockStaleAfter {
			_ = os.Remove(lock)
			continue
		}
		s.sleep(lockRetryInterval)
	}
	return nil, fmt.Errorf("token file is locked by another process: %s", lock)
}

// ValidName reports whether a token name is acceptable. Names are labels shown
// in `godrop token list`, so they stay to a conservative character set and must
// contain something readable — ".." is not a name.
func ValidName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	readable := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			readable = true
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return readable
}

func (s *Store) uniqueNameLocked(base string) string {
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		taken := false
		for _, t := range s.tokens {
			if strings.EqualFold(t.Name, candidate) {
				taken = true
				break
			}
		}
		if !taken {
			return candidate
		}
	}
}

func (s *Store) maybeReload() {
	s.mu.RLock()
	fresh := s.now().Sub(s.checked) < reloadInterval
	s.mu.RUnlock()
	if fresh {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		s.reportLocked(err)
	}
}

// SetErrorHandler installs a callback for token file problems noticed while
// the server is running. Reloading deliberately fails open — an unreadable
// file must not lock every client out of a working service — but it must not
// be silent either, because until it is fixed a revoked token stays valid.
func (s *Store) SetErrorHandler(fn func(error)) {
	s.mu.Lock()
	s.onError = fn
	s.mu.Unlock()
}

// reportLocked hands a reload failure to the error handler, but only when it
// differs from the last one. Verify runs on every request, and a broken file
// would otherwise repeat the same line until the log filled up.
func (s *Store) reportLocked(err error) {
	if err.Error() == s.lastErr {
		return
	}
	s.lastErr = err.Error()
	if s.onError != nil {
		s.onError(err)
	}
}

func (s *Store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reloadLocked()
}

// reloadLocked re-reads the token file when its size or modification time
// changed since the last read.
func (s *Store) reloadLocked() error {
	s.checked = s.now()
	info, err := os.Stat(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.tokens = nil
			s.modTime = time.Time{}
			s.size = 0
			return nil
		}
		return fmt.Errorf("read token file: %w", err)
	}
	if info.ModTime().Equal(s.modTime) && info.Size() == s.size {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read token file: %w", err)
	}
	var parsed fileFormat
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	s.tokens = parsed.Tokens
	s.modTime = info.ModTime()
	s.size = info.Size()
	s.dirty = false
	s.lastErr = "" // a later failure is news again
	return nil
}

// saveLocked writes the token file atomically with owner-only permissions:
// write a neighbouring temporary file, then rename it over the target, so a
// crash mid-write can never leave a half-written token database behind. Every
// caller holds the file lock, which has already created the directory.
func (s *Store) saveLocked() error {
	// fileFormat holds only strings, times and pointers to them, so encoding it
	// cannot fail — this error is impossible rather than ignored.
	data, _ := json.MarshalIndent(fileFormat{Tokens: s.tokens}, "", "  ")
	data = append(data, '\n')

	if err := writeFileAtomic(s.path, data); err != nil {
		return fmt.Errorf("write tokens: %w", err)
	}
	if info, err := os.Stat(s.path); err == nil {
		s.modTime = info.ModTime()
		s.size = info.Size()
	}
	s.dirty = false
	return nil
}

// writeFileAtomic writes data to a neighbouring temporary file and renames it
// into place, so a crash or a full disk can never leave a half-written token
// database behind.
//
// The temporary name is unique rather than a fixed "<path>.tmp": two processes
// writing at the same time would otherwise interleave into the same file and
// rename the mixture into place. os.CreateTemp also opens it 0600, which is
// what the token file needs.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	writeErr := writeAll(f, data)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// writeAll is a seam. A write that fails after the temporary file exists but
// before it is renamed is exactly the case this function is here to survive,
// and it cannot be provoked from outside on a healthy filesystem.
var writeAll = func(f *os.File, data []byte) error {
	_, err := f.Write(data)
	return err
}

// Path returns the token file path for a data directory.
func Path(dataDir string) string { return filepath.Join(dataDir, FileName) }
