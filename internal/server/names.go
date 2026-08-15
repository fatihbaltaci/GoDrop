package server

import (
	"mime"
	"net/http"
	"strings"

	"github.com/fatihbaltaci/GoDrop/internal/storage"
)

// maxSlugLen bounds the cosmetic name segment of a URL.
const maxSlugLen = 60

// turkishFold maps letters that would otherwise be dropped from URL slugs.
// Without it "Şubat Raporu.pdf" would become "ubat-raporu.pdf".
var turkishFold = strings.NewReplacer(
	"ı", "i", "İ", "i", "I", "i",
	"ş", "s", "Ş", "s",
	"ğ", "g", "Ğ", "g",
	"ü", "u", "Ü", "u",
	"ö", "o", "Ö", "o",
	"ç", "c", "Ç", "c",
)

// dangerousExts are extensions whose content the browser would execute in our
// origin. They are always sent as downloads, never rendered inline.
var dangerousExts = map[string]bool{
	"html": true, "htm": true, "xhtml": true, "xht": true, "shtml": true,
	"svg": true, "svgz": true, "xml": true, "xsl": true, "xslt": true,
	"mhtml": true, "mht": true, "xhtm": true,
}

// SanitizeExt extracts a storable extension from a client-supplied file name.
//
// The client's name is hostile input: it may contain "../", NUL bytes, control
// characters or Unicode trickery. We keep only what we can prove is harmless,
// lowercase ASCII alphanumerics and at most storage.MaxExtLen of them, and drop
// everything else. The result never reaches a filesystem path on its own; the
// identifier alone determines the path.
func SanitizeExt(filename string) string {
	// Cut anything that looks like a path, using both separators: a Windows
	// client may send "C:\photos\a.jpg".
	name := filename
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}
	i := strings.LastIndexByte(name, '.')
	if i < 0 || i == len(name)-1 {
		return ""
	}
	ext := strings.ToLower(name[i+1:])
	if len(ext) > storage.MaxExtLen {
		return ""
	}
	for _, r := range ext {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return ""
		}
	}
	return ext
}

// SanitizeSlug turns a client-supplied file name into the cosmetic last segment
// of a public URL. The extension is appended from ext (the stored one), so the
// slug can never disagree with what is on disk.
func SanitizeSlug(filename, ext string) string {
	name := filename
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		name = name[:i]
	}
	name = turkishFold.Replace(name)

	var b strings.Builder
	lastDash, lastDot := false, false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash, lastDot = false, false
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
			lastDash, lastDot = false, false
		case r == '_':
			b.WriteRune(r)
			lastDash, lastDot = false, false
		case r == '.':
			// Runs of dots collapse to one: a slug must never contain "..",
			// which some proxies and clients try to resolve as a path segment.
			if !lastDot && b.Len() > 0 {
				b.WriteByte('.')
				lastDot, lastDash = true, false
			}
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash, lastDot = true, false
			}
		}
		if b.Len() >= maxSlugLen {
			break
		}
	}
	slug := strings.Trim(b.String(), "-._")
	if slug == "" {
		return ""
	}
	return storage.JoinName(slug, ext)
}

// SplitStoredName parses the "<id>.<ext>" form used by short URLs.
func SplitStoredName(name string) (id, ext string, ok bool) {
	id, ext = storage.SplitName(name)
	if !storage.ValidID(id) || !storage.ValidExt(ext) {
		return "", "", false
	}
	return id, ext, true
}

// ContentType resolves the MIME type for a stored extension, falling back to
// sniffing the first bytes and finally to a generic binary type.
func ContentType(ext string, head []byte) string {
	if ext != "" {
		if ct := mime.TypeByExtension("." + ext); ct != "" {
			return ct
		}
	}
	if len(head) > 0 {
		if ct := http.DetectContentType(head); ct != "" {
			return ct
		}
	}
	return "application/octet-stream"
}

// IsDangerousExt reports whether content with this extension must never be
// rendered inline in our origin.
func IsDangerousExt(ext string) bool { return dangerousExts[ext] }

// A download URL is a capability: whoever knows it can fetch the file, and
// nothing else is asked for. Logs travel further than the files they describe
// (shipping pipelines, support tickets, screenshots), so the random half of an
// identifier is cut short before it is written to one.
//
// What is kept is still enough to work with. It identifies the upload uniquely
// among everything stored in that second, so a request line can be matched to
// the upload that created it and the file can be found on disk:
//
//	ls data/2026/08/15/20260815-143022-8f4e2c91*
//
// What is dropped is the 96 bits that make the rest of the identifier
// unguessable, which is exactly what a reader of the log must not learn.
const idLogPrefix = len("20260815-143022-") + 8

// ShortID abbreviates an identifier for logging.
func ShortID(id string) string {
	if !storage.ValidID(id) {
		return id
	}
	return id[:idLogPrefix] + "..."
}

// LogPath abbreviates the identifier inside a request path for logging.
func LogPath(path string) string {
	rest, ok := strings.CutPrefix(path, "/f/")
	if !ok {
		return path
	}
	id, tail := rest, ""
	if i := strings.IndexAny(rest, "./"); i >= 0 {
		id, tail = rest[:i], rest[i:]
	}
	if !storage.ValidID(id) {
		return path
	}
	return "/f/" + ShortID(id) + tail
}
