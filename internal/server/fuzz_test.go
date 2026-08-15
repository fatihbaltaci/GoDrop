package server

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/fatihbaltaci/GoDrop/internal/storage"
)

// The two sanitisers are the only places where hostile input meets the names we
// build paths and URLs from. Fuzzing states the invariants directly: whatever
// goes in, what comes out can never escape a directory or a URL segment.

func FuzzSanitizeExt(f *testing.F) {
	seeds := []string{
		"photo.jpg", "../../etc/passwd", `C:\x\y.EXE`, ".", "..", "", "a.b.c",
		"x.jp g", "trailing.", "unicode.şey", "long.abcdefghijklmnop",
		"nul.jp\x00g", "newline.jpg\n", "semi.jpg;.exe", "%2e%2e%2fetc.conf",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		ext := SanitizeExt(name)

		if !storage.ValidExt(ext) {
			t.Fatalf("SanitizeExt(%q) = %q, which the storage layer rejects", name, ext)
		}
		if len(ext) > storage.MaxExtLen {
			t.Fatalf("SanitizeExt(%q) = %q, longer than the allowed %d", name, ext, storage.MaxExtLen)
		}
		if strings.ContainsAny(ext, `/\.`+"\x00") {
			t.Fatalf("SanitizeExt(%q) = %q, which contains a path character", name, ext)
		}
		// The decisive property: joining it with a valid identifier can never
		// leave the data directory.
		const id = "20260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3d"
		joined := filepath.Join("/data", "2026", "08", "15", storage.JoinName(id, ext))
		if !strings.HasPrefix(filepath.Clean(joined), "/data/") {
			t.Fatalf("SanitizeExt(%q) produced a path escape: %q", name, joined)
		}
	})
}

func FuzzSanitizeSlug(f *testing.F) {
	seeds := []struct {
		name, ext string
	}{
		{"Yaz Tatili 2026.jpg", "jpg"},
		{"../../etc/passwd", ""},
		{`..\..\windows\system32`, "exe"},
		{"🙂.png", "png"},
		{"Şubat Raporu.pdf", "pdf"},
		{strings.Repeat("a", 500) + ".jpg", "jpg"},
		{"", ""},
		{"...", "txt"},
		{"a%2fb.txt", "txt"},
	}
	for _, s := range seeds {
		f.Add(s.name, s.ext)
	}
	f.Fuzz(func(t *testing.T, name, ext string) {
		ext = SanitizeExt("x." + ext) // only sanitised extensions ever reach it
		slug := SanitizeSlug(name, ext)
		if slug == "" {
			return // the caller falls back to the short URL form
		}
		for _, bad := range []string{"/", `\`, "..", "\x00", " ", "\n", "\r", "?", "#", "%"} {
			if strings.Contains(slug, bad) {
				t.Fatalf("SanitizeSlug(%q, %q) = %q, which contains %q", name, ext, slug, bad)
			}
		}
		if strings.HasPrefix(slug, ".") {
			t.Fatalf("SanitizeSlug(%q, %q) = %q, which would create a hidden file name", name, ext, slug)
		}
		if len(slug) > maxSlugLen+storage.MaxExtLen+1 {
			t.Fatalf("SanitizeSlug(%q, %q) = %q, which is unbounded", name, ext, slug)
		}
		// The cosmetic segment must stay a single URL path segment.
		if strings.Count(slug, "/") != 0 {
			t.Fatalf("SanitizeSlug(%q, %q) = %q, which spans several path segments", name, ext, slug)
		}
	})
}

func FuzzSplitStoredName(f *testing.F) {
	for _, s := range []string{
		"20260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3d.jpg",
		"20260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3d",
		"../../etc/passwd", "", ".", "..", "tokens.json",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		id, ext, ok := SplitStoredName(name)
		if !ok {
			return
		}
		if !storage.ValidID(id) || !storage.ValidExt(ext) {
			t.Fatalf("SplitStoredName(%q) accepted id=%q ext=%q", name, id, ext)
		}
		if strings.ContainsAny(id+ext, `/\.`+"\x00") {
			t.Fatalf("SplitStoredName(%q) produced path characters: %q / %q", name, id, ext)
		}
	})
}
