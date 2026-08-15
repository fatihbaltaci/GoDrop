// Package updater replaces the running binary with a newer release.
//
// The rule the whole package is built around: a failed update must leave the
// working installation exactly as it was. Nothing touches the installed binary
// until a replacement has been downloaded, its checksum verified against the
// published SHA256SUMS, and the new binary asked for its own version and seen
// to answer. Only then is it moved into place with a rename, which is atomic,
// so there is no moment where the path holds a half-written file.
//
// An installation managed by something else, a package manager, Homebrew, a
// container image, is refused rather than overwritten. Updating those behind
// their manager's back is how a system ends up in a state nobody can explain.
package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Repo is the GitHub repository releases are published from.
const Repo = "fatihbaltaci/GoDrop"

// maxArchive bounds what will be downloaded, well above any real release.
const maxArchive = 256 << 20

// ErrManaged is returned when the installation belongs to a package manager.
var ErrManaged = errors.New("this installation is managed by something else")

// Options configures an update. The zero value is usable in production.
type Options struct {
	// Version is the release to install, "" for the newest.
	Version string
	// BinaryPath is the file to replace, "" for the running executable.
	BinaryPath string
	// HTTP is the client used for GitHub, nil for the default.
	HTTP *http.Client
	// BaseURL overrides GitHub, for tests.
	BaseURL string
	// APIURL overrides the GitHub API, for tests.
	APIURL string
	// Verify runs the downloaded binary and returns its version output. It is
	// a seam: the point of it is that a broken download never gets installed.
	Verify func(path string) (string, error)
	// GOOS and GOARCH default to the running platform.
	GOOS, GOARCH string
	// Now is the clock, for tests.
	Now func() time.Time
}

// now is the clock the package reads.
func now(opts Options) time.Time {
	if opts.Now != nil {
		return opts.Now()
	}
	return time.Now()
}

// Result describes what an update did.
type Result struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Path        string `json:"path,omitempty"`
	Updated     bool   `json:"updated"`
	UpToDate    bool   `json:"up_to_date"`
	Checksum    string `json:"checksum,omitempty"`
	ManagedBy   string `json:"managed_by,omitempty"`
	ManagedHint string `json:"managed_hint,omitempty"`
}

// Latest returns the newest published release tag.
func Latest(ctx context.Context, opts Options) (string, error) {
	api := opts.APIURL
	if api == "" {
		api = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api+"/repos/"+Repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client(opts).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%s has published no releases yet", Repo)
	}
	// A shared address (an office, a CI runner, a NAT) can use up GitHub's
	// anonymous allowance without the person at the keyboard doing anything,
	// and "403 Forbidden" reads like a permission problem rather than a wait.
	if resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return "", fmt.Errorf("github's rate limit for this address is used up%s; "+
			"the release page still works: https://github.com/%s/releases/latest",
			resetIn(resp.Header.Get("X-RateLimit-Reset"), now(opts)), Repo)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github returned %s when asked for the latest release", resp.Status)
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &release); err != nil || release.TagName == "" {
		return "", errors.New("could not read the latest release from github")
	}
	return release.TagName, nil
}

// resetIn turns the rate limit reset header into " (try again in 12m)", or
// nothing at all when the header is missing or already past.
func resetIn(header string, at time.Time) string {
	seconds, err := strconv.ParseInt(header, 10, 64)
	if err != nil {
		return ""
	}
	wait := time.Unix(seconds, 0).Sub(at).Round(time.Minute)
	if wait <= 0 {
		return ""
	}
	// 2m0s and 1h0m0s read like machine output; 2m and 1h read like an answer.
	text := strings.TrimSuffix(wait.String(), "0s")
	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}
	return " (try again in " + text + ")"
}

// Update downloads a release and puts it in place of the current binary.
func Update(ctx context.Context, current string, opts Options) (Result, error) {
	res := Result{From: current}

	target, err := binaryPath(opts)
	if err != nil {
		return res, err
	}
	res.Path = target

	if manager, hint := managedBy(target); manager != "" {
		res.ManagedBy, res.ManagedHint = manager, hint
		return res, fmt.Errorf("%w: %s. %s", ErrManaged, manager, hint)
	}

	version := opts.Version
	if version == "" {
		if version, err = Latest(ctx, opts); err != nil {
			return res, err
		}
	}
	res.To = version
	if SameVersion(current, version) {
		res.UpToDate = true
		return res, nil
	}

	dir := filepath.Dir(target)
	// The replacement is staged in the directory it will land in, because a
	// rename is only atomic within one filesystem.
	staged, sum, err := download(ctx, opts, version, dir)
	if err != nil {
		return res, err
	}
	defer os.Remove(staged) // a no-op once the rename has moved it
	res.Checksum = sum

	verify := opts.Verify
	if verify == nil {
		verify = runVersion
	}
	if out, err := verify(staged); err != nil {
		return res, fmt.Errorf("the downloaded binary does not run, so nothing was replaced: %w", err)
	} else if !strings.Contains(out, "godrop") {
		return res, fmt.Errorf("the downloaded binary does not look like godrop (%q), so nothing was replaced",
			strings.TrimSpace(out))
	}

	if err := replaceOn(runtime.GOOS, staged, target); err != nil {
		return res, err
	}
	res.Updated = true
	return res, nil
}

// replace moves the staged binary over the installed one and puts the old one
// back if anything goes wrong.
//
// On Unix the rename is enough: the running process keeps the inode it started
// from, so an update never disturbs a server that is already serving. Windows
// refuses to replace a file that is open, so the old binary is moved aside
// first and restored if the move fails.
// The operating system is a parameter rather than a build tag so the Windows
// path can be exercised, and reasoned about, from any machine.
func replaceOn(goos, staged, target string) error {
	if goos != "windows" {
		return renameFile(staged, target)
	}
	backup := target + ".old"
	_ = os.Remove(backup)
	if err := renameFile(target, backup); err != nil {
		return err
	}
	if err := renameFile(staged, target); err != nil {
		if restoreErr := renameFile(backup, target); restoreErr != nil {
			return fmt.Errorf("%w (and the previous binary is at %s)", err, backup)
		}
		return err
	}
	// Windows cannot delete a running executable either, so this may fail
	// while the old process is alive. The next update clears it.
	_ = os.Remove(backup)
	return nil
}

// renameFile is a seam. On Windows the swap has to put the old binary back if
// the new one cannot be moved into place, and the case where even that fails
// is the one an operator would most need a useful message for.
var renameFile = os.Rename

// download fetches the release archive for this platform, checks it against the
// published SHA256SUMS and writes the binary into dir. The checksum is verified
// before anything is unpacked.
func download(ctx context.Context, opts Options, version, dir string) (string, string, error) {
	goos, goarch := opts.GOOS, opts.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	name := fmt.Sprintf("godrop_%s_%s_%s.%s", strings.TrimPrefix(version, "v"), goos, goarch, ext)

	base := opts.BaseURL
	if base == "" {
		base = "https://github.com/" + Repo + "/releases/download"
	}
	archive, err := fetch(ctx, opts, base+"/"+version+"/"+name)
	if err != nil {
		return "", "", err
	}
	sums, err := fetch(ctx, opts, base+"/"+version+"/SHA256SUMS")
	if err != nil {
		return "", "", fmt.Errorf("could not fetch SHA256SUMS, refusing to install an unverified binary: %w", err)
	}
	want, err := checksumFor(string(sums), name)
	if err != nil {
		return "", "", err
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != want {
		return "", "", fmt.Errorf("checksum mismatch for %s: expected %s, got %s",
			name, want, hex.EncodeToString(got[:]))
	}

	binary, err := extract(archive, goos)
	if err != nil {
		return "", "", err
	}
	staged, err := os.CreateTemp(dir, ".godrop-update-*")
	if err != nil {
		return "", "", fmt.Errorf("cannot write next to the current binary: %w", err)
	}
	path := staged.Name()
	stageErr := writeStaged(staged, binary)
	closeErr := staged.Close()
	if err := errors.Join(stageErr, closeErr); err != nil {
		_ = os.Remove(path)
		return "", "", err
	}
	return path, want, nil
}

func fetch(ctx context.Context, opts Options, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client(opts).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxArchive))
}

// checksumFor finds one file's digest in a SHA256SUMS listing.
func checksumFor(sums, name string) (string, error) {
	for line := range strings.SplitSeq(sums, "\n") {
		digest, file, ok := strings.Cut(strings.TrimSpace(line), "  ")
		if ok && file == name {
			return digest, nil
		}
	}
	return "", fmt.Errorf("%s is not listed in SHA256SUMS", name)
}

// extract pulls the godrop binary out of a release archive.
func extract(archive []byte, goos string) ([]byte, error) {
	if goos == "windows" {
		return fromZip(archive)
	}
	return fromTarGz(archive)
}

func fromTarGz(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return nil, fmt.Errorf("release archive is not gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if path.Base(header.Name) != "godrop" || header.Typeflag != tar.TypeReg {
			continue
		}
		return io.ReadAll(io.LimitReader(tr, maxArchive))
	}
	return nil, errors.New("no godrop binary inside the release archive")
}

func fromZip(archive []byte) ([]byte, error) {
	zr, err := zip.NewReader(strings.NewReader(string(archive)), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("release archive is not a zip: %w", err)
	}
	for _, f := range zr.File {
		if path.Base(f.Name) != "godrop.exe" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, maxArchive))
	}
	return nil, errors.New("no godrop.exe inside the release archive")
}

// SameVersion compares two version strings, ignoring a leading v.
func SameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

func client(opts Options) *http.Client {
	if opts.HTTP != nil {
		return opts.HTTP
	}
	return http.DefaultClient
}

func binaryPath(opts Options) (string, error) {
	if opts.BinaryPath != "" {
		return opts.BinaryPath, nil
	}
	exe, err := osExecutable()
	if err != nil {
		return "", err
	}
	// Follow a symlink so the real file is replaced rather than the link.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// osExecutable is a seam: the running binary's path is what an update
// replaces, and the failure to find it needs an answer too.
var osExecutable = os.Executable

// writeStaged is a seam. A write or a permission change that fails once the
// file exists is exactly what must not leave a half-written binary behind, and
// it cannot be provoked on a healthy filesystem.
var writeStaged = func(f *os.File, data []byte) error {
	_, err := f.Write(data)
	return errors.Join(err, f.Chmod(0o755)) //nolint:gosec // it has to be executable
}

// runVersion executes a binary and returns what it says about itself.
var runVersion = func(path string) (string, error) {
	return execVersion(path)
}
