# GoDrop

**Upload a file, get a hard-to-guess URL.** One binary, no database, files on disk.

[![CI](https://github.com/fatihbaltaci/GoDrop/actions/workflows/ci.yml/badge.svg)](https://github.com/fatihbaltaci/GoDrop/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/fatihbaltaci/GoDrop.svg)](https://pkg.go.dev/github.com/fatihbaltaci/GoDrop)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

```bash
curl -X POST \
  -H "Authorization: Bearer $GODROP_TOKEN" \
  -F "file=@photo.jpg" \
  https://files.example.com/upload
```

```json
{
  "url": "https://files.example.com/f/20260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3d/photo.jpg",
  "files": [
    {
      "url": "https://files.example.com/f/20260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3d/photo.jpg",
      "id": "20260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3d.jpg",
      "name": "photo.jpg",
      "size": 12345,
      "mime": "image/jpeg"
    }
  ]
}
```

That is the whole idea. Downloads need no token; uploads and deletes do.

---

## Install

```bash
curl -fsSL https://godrop.sh/install.sh | sh
```

The script picks the right binary for your machine, **verifies its SHA-256
checksum** against the published `SHA256SUMS`, installs it, and hands over to a
setup wizard that writes your configuration, creates your first token and
checks that the internet can actually reach you.

Other ways:

```bash
# Docker
docker run -d -p 8080:8080 \
  -e GODROP_TOKENS=$(openssl rand -hex 16) \
  -v godrop-data:/data \
  ghcr.io/fatihbaltaci/godrop

# Debian or Ubuntu — installs the systemd service and creates its user
sudo dpkg -i godrop_1.0.0_linux_amd64.deb

# Fedora, RHEL or openSUSE
sudo rpm -i godrop_1.0.0_linux_amd64.rpm

# From source (Go 1.26+). Binaries built this way send no telemetry at all.
go install github.com/fatihbaltaci/GoDrop/cmd/godrop@latest
```

### Platforms

| | Server | CLI | Notes |
| --- | :---: | :---: | --- |
| **Linux** (amd64, arm64) | ✅ | ✅ | Where it is meant to run: `.deb`, `.rpm`, `.apk`, container image, systemd unit |
| **macOS** (Intel, Apple silicon) | ✅ | ✅ | Fine for development and small installs; no systemd, so use Docker or start it yourself |
| **Windows** (amd64, arm64) | ⚠️ | ✅ | The binary works and is tested in CI, but there is no service wrapper, no installer script and no firewall guidance — take the zip from Releases |
| **FreeBSD** | 🔧 | 🔧 | Compiles and passes tests; no binaries published |

The suite runs on Linux, macOS and Windows for every change, and each release
artefact carries [signed build
provenance](https://docs.github.com/actions/security-for-github-actions/using-artifact-attestations):

```bash
gh attestation verify godrop_1.0.0_linux_amd64.tar.gz --repo fatihbaltaci/GoDrop
```

## Guided setup

```console
$ godrop init

  GoDrop 1.0.0 — setup

  Public address
  ? Public URL              https://files.example.com
  Storage
  ? Data directory          /var/lib/godrop
  ? Maximum file size       100MB
  ? Storage quota           20GB
  ? Delete files after      (never)
  Service
  ? Listen port             8080
  ? Deployment style        docker compose
  Finishing up
  ? Anonymous heartbeat     yes
  ? Verify reachability     yes

  Written
  ✓ .env  (chmod 600 — contains your token)
  ✓ docker-compose.yml
  ✓ Caddyfile

  Your API token
  ┌────────────────────────────────────────┐
  │ gd_7f3a9c2e1b8d4a6f0c5e2d9b3a7f1e4c    │
  └────────────────────────────────────────┘
  ⚠ shown once and never again — copy it now

  Verifying
  ✓ 127.0.0.1:8080        listening
  ✓ firewall              ufw allows port 443
  ✓ external access       reachable (HTTP 200, from FRA)
```

Every question shows its default and can be answered with a flag instead, so CI
and agents run the same code path without a terminal. Prompts are skipped
automatically when there is no TTY; `--no-input` makes that explicit:

```bash
godrop init --no-input \
  --base-url https://files.example.com \
  --data-dir /var/lib/godrop \
  --max-total-size 20GB --json
```

The wizard only offers what the host can do: systemd appears on Linux, not on
macOS or Windows, and the commands it prints use the right shell.

## The API

| Method | Path | Auth | Purpose |
| --- | --- | :---: | --- |
| `POST` | `/upload` | ✅ | multipart, one or more `file` fields |
| `PUT` | `/upload/{name}` | ✅ | raw request body |
| `GET`/`HEAD` | `/f/{id}/{name}` · `/f/{id}.{ext}` | — | download; supports Range and ETag |
| `DELETE` | `/f/{id}/{name}` · `/f/{id}.{ext}` | ✅ | delete |
| `GET` | `/healthz` | — | liveness |
| `GET` | `/readyz` | — | readiness: is storage writable |
| `GET` | `/stats` | ✅ | file count, bytes, quota, uptime |
| `GET` | `/llms.txt` · `/openapi.yaml` | — | machine-readable description |

```bash
# Upload several files at once — all succeed or none do
curl -X POST -H "Authorization: Bearer $GODROP_TOKEN" \
  -F "file=@a.png" -F "file=@b.pdf" \
  https://files.example.com/upload

# Upload without multipart
curl -X PUT --data-binary @report.pdf \
  -H "Authorization: Bearer $GODROP_TOKEN" \
  https://files.example.com/upload/report.pdf

# Download (no token), or force a download instead of inline rendering
curl -O https://files.example.com/f/20260815-143022-8f4e…/photo.jpg
curl -O "https://files.example.com/f/20260815-143022-8f4e….jpg?dl=1"

# Delete
curl -X DELETE -H "Authorization: Bearer $GODROP_TOKEN" \
  https://files.example.com/f/20260815-143022-8f4e…/photo.jpg
```

Both `Authorization: Bearer <token>` and `X-API-Key: <token>` are accepted.

**Status codes:** `201` uploaded · `204` deleted · `400` malformed body or too
many files · `401` bad token · `404` unknown id, or the name's extension does
not match the stored file · `413` file too large · `415` not multipart · `429`
rate limited (honour `Retry-After`) · `507` quota full.

## Tokens

```console
$ godrop token create --name claude-code
  gd_7f3a9c2e1b8d4a6f0c5e2d9b3a7f1e4c      # shown once, usable immediately

$ godrop token list
  NAME          CREATED       LAST USED
  claude-code   2 min ago     just now
  ci            12 days ago   3 hours ago

$ godrop token revoke ci                    # effective within a second
```

Tokens are stored as **SHA-256 digests** in `<data-dir>/tokens.json` (mode
`0600`). A leaked file cannot be turned back into a working token, and a backup
restored onto another machine keeps working — which machine-bound encryption
would break for no security gain. A running server notices new and revoked
tokens without a restart.

`GODROP_TOKENS` still works and is the practical choice on Docker, Fly and
Railway; both sources are accepted together.

## Configuration

Everything is an environment variable. Sizes accept `100MB`, `2GB`, `512KB`;
durations accept `30d`, `12h`, `90m`; rates accept `60/m`, `10/s`, `100/h`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `GODROP_TOKENS` | — | Comma-separated API tokens |
| `GODROP_BASE_URL` | *(from request)* | Public URL used in responses |
| `GODROP_ADDR` | `:8080` | Listen address |
| `GODROP_DATA_DIR` | `./data` | Where files live (`/data` in the image) |
| `GODROP_MAX_FILE_SIZE` | `100MB` | Per-file limit → `413` |
| `GODROP_MAX_FILES_PER_REQUEST` | `20` | Files per multipart request |
| `GODROP_MAX_TOTAL_SIZE` | *(unlimited)* | Storage quota → `507` |
| `GODROP_RETENTION` | *(forever)* | Delete uploads older than this |
| `GODROP_RATE_LIMIT` | *(off)* | Uploads per token |
| `GODROP_AUTH_RATE_LIMIT` | *(off)* | Failed authentications per client address |
| `GODROP_CORS_ORIGINS` | `*` | Browser origins allowed to call the API |
| `GODROP_READ_HEADER_TIMEOUT` | `10s` | Slow-header protection |
| `GODROP_READ_TIMEOUT` / `GODROP_WRITE_TIMEOUT` | `0` | Body timeouts, off on purpose |
| `GODROP_IDLE_TIMEOUT` | `120s` | Keep-alive idle timeout |
| `GODROP_SHUTDOWN_TIMEOUT` | `30s` | Grace period for in-flight transfers |
| `GODROP_LOG_FORMAT` / `GODROP_LOG_LEVEL` | `json` / `info` | Logging |
| `GODROP_ACCESS_LOG` | `true` | Per-request log lines |
| `GODROP_TELEMETRY` | `on` | Anonymous daily heartbeat |

> The body timeouts default to `0` deliberately. A 100MB file over a slow
> connection is legitimate and takes minutes; a `WriteTimeout` would cut it off
> mid-transfer. The size limit, not the clock, is what bounds an upload.

See [`.env.example`](.env.example) for the annotated version.

## How it works

```
data/2026/08/15/20260815-143022-8f4e2c91b7934b38a72d1c0e5b6a4f3d.jpg
     └── date ──┘└── identifier: timestamp + 128 random bits ──┘└ext┘
```

The identifier *is* the index. It carries its own location, so a lookup needs
no database and no directory scan; the timestamp keeps directories small and
makes retention a directory-level operation; the 128 random bits make URLs
unguessable. The extension is the only metadata kept — the MIME type is derived
from it at download time.

**There is no listing endpoint, by design.** Keep the URL an upload returns.

Retention only ever deletes uploads. `tokens.json` and the telemetry markers
share the directory, and sweeping those away by age would revoke every token.

## Diagnosis

```console
$ godrop doctor

  Configuration
  ✓ tokens               2 token(s) configured
  ✓ base_url             https://files.example.com
  ⚠ storage quota        no quota set; uploads can fill the disk
      → set GODROP_MAX_TOTAL_SIZE=20GB

  Storage
  ✓ writable             /var/lib/godrop is writable
  ✓ usage                142 file(s), 834.2MB of 20.0GB (4%)
  ✓ disk space           38.2GB free of 79.0GB
  ✗ persistence          /data is inside the container, not a volume
      → mount a volume: docker run -v godrop-data:/data …

  Network
  ✓ dns                  files.example.com → 5.9.1.2
  ✓ tls                  valid, 67 days left (E5)
  ✗ external             not reachable from the internet: connection refused
      → open the port in your provider's firewall (security group)

  End to end
  ✗ proxy body limit     a proxy rejected a tiny upload with 413
      → nginx: client_max_body_size 100m;
```

It exits non-zero when a check fails, so it works as a deployment gate:
`godrop doctor --json | jq '.checks[] | select(.status=="fail")'`.

The reachability check asks <https://godrop.sh/api/check> to fetch your
`/healthz` from the public internet — the only way to catch a cloud firewall,
which is invisible from inside the machine. Only the URL is sent; skip it with
`--offline`.

To diagnose an instance from your own machine, pass its address and a token.
The token goes in the environment, not on the command line, where the process
list and the shell history would both keep a copy:

```bash
GODROP_TOKEN=gd_... godrop doctor --url https://files.example.com
```

## Deploying

### A VPS with automatic TLS

```bash
curl -fsSL https://godrop.sh/install.sh | sh    # installs and runs `godrop init`
# then, for a container setup with Caddy in front:
docker compose -f deploy/docker-compose.caddy.yml up -d
```

`deploy/` also holds [`nginx.conf`](deploy/nginx.conf) and a hardened
[`godrop.service`](deploy/godrop.service) for systemd. Whichever proxy you use,
**raise its body size limit** to match `GODROP_MAX_FILE_SIZE` — `godrop doctor`
tests this for you.

The `.deb` and `.rpm` packages do the systemd part for you: they install the
unit, create an unprivileged `godrop` user, and put the configuration in
`/etc/godrop/godrop.env` (kept across upgrades) with uploads in
`/var/lib/godrop`.

### Fly.io

```bash
fly launch --no-deploy --copy-config
fly volumes create godrop_data --size 10
fly secrets set GODROP_TOKENS=$(openssl rand -hex 16)
fly deploy
```

### Railway

Create a service from this repository, **add a volume mounted at `/data`**, and
set `GODROP_TOKENS` and `GODROP_BASE_URL`. Without the volume, uploads vanish
on the next deploy.

### Render

Use [`render.yaml`](render.yaml) as a blueprint. The persistent disk requires a
paid instance and pins the service to one instance — GoDrop stores files on
local disk and does not scale horizontally, by design.

### Not Vercel, not Cloudflare Workers

Both have ephemeral or absent filesystems: an uploaded file would be gone on
the next deploy or the next request. Making it "work" would mean adding object
storage, which is exactly the complexity this project exists to avoid. Use a
platform with a real disk.

## For AI agents

GoDrop is built to be driven by coding agents. Give one a base URL and a token
and it can discover the rest:

```bash
curl https://files.example.com/llms.txt       # the whole API as plain text
curl https://files.example.com/openapi.yaml   # machine-readable schema
```

- [`SKILL.md`](SKILL.md) — drop-in skill file for Claude Code and similar tools
- Every command accepts `--json`, and in that mode prints **nothing but** the
  document, so parsing never breaks
- Colour and interactive prompts switch themselves off when there is no
  terminal — an agent can never get stuck on a form

```bash
godrop token create --name claude-code --json | jq -r .token
godrop doctor --json | jq '.ok'
```

## Security

- **Unguessable identifiers** — 128 bits from `crypto/rand`; no enumeration
  endpoint exists
- **No path traversal, structurally** — the storage path is derived from the
  validated identifier alone; a client-supplied name never takes part in it
- **Active content is never rendered** — `.html`, `.svg`, `.xml` and friends are
  always sent as downloads, with `nosniff` and `Content-Security-Policy:
  default-src 'none'; sandbox` on every response
- **Names cannot lie** — `/f/<id>/setup.exe` does not resolve to a stored `.jpg`
- **Constant-time token comparison**, digests at rest, tokens never logged
- **Fails closed** — the server refuses to start without a token
- **Bounded everything** — per-file size, per-request size, file count, storage
  quota, and optional per-token and per-address rate limits
- **Logs are not a key ring** — the random half of an identifier is cut short
  before it is written to a log, so a log reader cannot rebuild a download URL

[SECURITY.md](SECURITY.md) sets out what GoDrop defends, what it deliberately
does not, and a hardening checklist. Found something? Open a security advisory
on GitHub rather than an issue.

## Telemetry

GoDrop sends one anonymous heartbeat per day:

```json
{"event":"heartbeat","distinct_id":"a3f19c…",
 "properties":{"version":"1.0.0","os":"linux","arch":"arm64","deploy":"docker"}}
```

That is the entire payload — no file names, no counts, no addresses, no base
URL. It exists to answer "how many installations are there, on what, and how
many are stuck on an old version".

```bash
godrop telemetry status --json   # shows the exact body that would be sent
godrop telemetry off             # or GODROP_TELEMETRY=off
```

Binaries built from source have no telemetry key compiled in and never report.

## Development

```bash
make test         # everything, with the race detector
make cover        # coverage, failing below 100%
make fuzz         # fuzz the input sanitisers
make run          # a local server on port 48080
make docker       # build the image
make snapshot     # build every release artefact locally, without publishing
```

Releases come from [GoReleaser](https://goreleaser.com) via
[`.goreleaser.yaml`](.goreleaser.yaml) — binaries, archives, checksums, Linux
packages and the changelog. CI builds the whole set on every change, so a tag
never fails on something that could have been caught earlier.

Every statement in `internal/` is covered by a test, and CI fails if that ever
slips. `cmd/godrop` is a three-line shim around `os.Exit` and is excluded.
The fuzz corpus in `internal/server/testdata/` includes a case that fuzzing
found: a file name that produced `..` inside a URL segment.

---

<p align="center">
  <sub>Sponsored by</sub><br/><br/>
  <a href="https://gurubase.io/">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="https://gurubase.io/media/gurubase-dark-logo.png">
      <img src="https://gurubase.io/media/gurubase-light-logo.png" alt="Gurubase" height="34">
    </picture>
  </a>
</p>
