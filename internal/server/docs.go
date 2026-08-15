package server

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/fatihbaltaci/GoDrop/internal/config"
)

// handleUsage answers GET / with a short plain-text summary. It is the first
// thing both a human and an agent see, so it states the limits too.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	base := s.cfg.PublicURL(requestScheme(r), r.Host, "")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, s.usageText(base))
}

func (s *Server) usageText(base string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "GoDrop %s — upload a file, get a URL\n\n", s.version)
	fmt.Fprintf(&b, "  POST   %s/upload            multipart field \"file\" (Bearer token)\n", base)
	fmt.Fprintf(&b, "  PUT    %s/upload/{name}     raw request body     (Bearer token)\n", base)
	fmt.Fprintf(&b, "  GET    %s/f/{id}/{name}     public, no auth\n", base)
	fmt.Fprintf(&b, "  DELETE %s/f/{id}/{name}     (Bearer token)\n", base)
	fmt.Fprintf(&b, "  GET    %s/healthz %s/readyz %s/stats\n\n", base, base, base)
	fmt.Fprintf(&b, "  max file size: %s   max files per request: %d\n",
		config.FormatSize(s.cfg.MaxFileSize), s.cfg.MaxFilesPerRequest)
	if s.cfg.MaxTotalSize > 0 {
		fmt.Fprintf(&b, "  storage quota: %s\n", config.FormatSize(s.cfg.MaxTotalSize))
	}
	if s.cfg.Retention > 0 {
		fmt.Fprintf(&b, "  files are deleted after: %s\n", s.cfg.Retention)
	}
	fmt.Fprintf(&b, "\n  curl -X POST -H \"Authorization: Bearer $GODROP_TOKEN\" \\\n")
	fmt.Fprintf(&b, "    -F \"file=@photo.jpg\" %s/upload\n\n", base)
	fmt.Fprintf(&b, "  machine-readable: %s/llms.txt  %s/openapi.yaml\n", base, base)
	return b.String()
}

// handleLLMs serves an llms.txt description of this exact instance. It is
// generated rather than embedded so that the limits an agent reads are the
// limits this server actually enforces.
func (s *Server) handleLLMs(w http.ResponseWriter, r *http.Request) {
	base := s.cfg.PublicURL(requestScheme(r), r.Host, "")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, s.llmsText(base))
}

func (s *Server) llmsText(base string) string {
	var b strings.Builder
	quota := "unlimited"
	if s.cfg.MaxTotalSize > 0 {
		quota = config.FormatSize(s.cfg.MaxTotalSize)
	}
	retention := "files are kept forever"
	if s.cfg.Retention > 0 {
		retention = "files are deleted " + s.cfg.Retention.String() + " after upload"
	}

	fmt.Fprintf(&b, `# GoDrop

> A tiny self-hosted file host. Upload a file with a token, get back a public
> URL that nobody can guess. Downloads need no authentication.

Base URL: %s
Version: %s

## Authentication

Send the API token on every write. Both forms work:

    Authorization: Bearer <token>
    X-API-Key: <token>

Downloads (GET) never require a token.

## Upload one file

    curl -sS -X POST \
      -H "Authorization: Bearer $GODROP_TOKEN" \
      -F "file=@photo.jpg" \
      %s/upload

Response (201):

    {
      "url": "%s/f/20260815-143022-8f4e.../photo.jpg",
      "files": [
        {
          "url": "%s/f/20260815-143022-8f4e.../photo.jpg",
          "id": "20260815-143022-8f4e....jpg",
          "name": "photo.jpg",
          "size": 12345,
          "mime": "image/jpeg"
        }
      ]
    }

Read "url" for the single-file case; read "files" when you sent several. Both
fields are always present, so no branching is needed.

## Upload several files

    curl -sS -X POST -H "Authorization: Bearer $GODROP_TOKEN" \
      -F "file=@a.png" -F "file=@b.pdf" %s/upload

All files succeed or none do: if one is rejected, the others are removed too.
At most %d files per request.

## Upload a raw body (no multipart)

    curl -sS -X PUT --data-binary @report.pdf \
      -H "Authorization: Bearer $GODROP_TOKEN" \
      %s/upload/report.pdf

## Download

    curl -sS -O %s/f/<id>/<name>

Add ?dl=1 to force a download instead of inline rendering. Range requests and
ETags are supported.

## Delete

    curl -sS -X DELETE -H "Authorization: Bearer $GODROP_TOKEN" \
      %s/f/<id>/<name>

Returns 204 on success, 404 if it was already gone.

## Limits

- max file size: %s
- max files per request: %d
- storage quota: %s
- retention: %s

## Status codes

- 201 created — upload succeeded
- 204 no content — delete succeeded
- 400 bad request — malformed multipart body, or too many files
- 401 unauthorized — missing or wrong token
- 404 not found — unknown id, or the name's extension does not match the stored file
- 413 payload too large — the file is bigger than the limit above
- 415 unsupported media type — POST /upload needs multipart/form-data
- 429 too many requests — rate limited; honour the Retry-After header
- 507 insufficient storage — the server's quota is full

## Notes for agents

- Identifiers are unguessable; there is no listing endpoint by design. Keep the
  URL returned by the upload call if you want to delete the file later.
- The stored extension decides the Content-Type. Send a sensible file name.
- HTML and SVG uploads are always served as downloads, never rendered.
- Health: GET %s/healthz (liveness), GET %s/readyz (storage writable).
- Machine-readable schema: %s/openapi.yaml
`,
		base, s.version,
		base, base, base, base, s.cfg.MaxFilesPerRequest, base, base, base,
		config.FormatSize(s.cfg.MaxFileSize), s.cfg.MaxFilesPerRequest, quota, retention,
		base, base, base)
	return b.String()
}
