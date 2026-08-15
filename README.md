<p align="center">
  <img src="site/logo-lockup.svg" alt="GoDrop" width="420">
</p>

<p align="center">
  <strong>Upload a file, get a hard-to-guess URL.</strong><br>
  Written in Go: one binary, no database, files on disk.
</p>

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
# Docker: comes back up after a reboot, and after a crash
docker run -d --name godrop --restart always \
  -p 8747:8747 \
  -e GODROP_TOKENS=$(openssl rand -hex 16) \
  -v godrop-data:/data \
  ghcr.io/fatihbaltaci/godrop

# Debian or Ubuntu: installs the systemd service and creates its user.
# Download the .deb for your architecture from the releases page:
# https://github.com/fatihbaltaci/GoDrop/releases/latest
sudo dpkg -i godrop_1.0.0_linux_amd64.deb

# Fedora, RHEL or openSUSE
sudo rpm -i godrop_1.0.0_linux_amd64.rpm

# From source (Go 1.26+). Binaries built this way send no telemetry at all.
go install github.com/fatihbaltaci/GoDrop/cmd/godrop@latest
```

Every package, archive and checksum is on the [releases
page](https://github.com/fatihbaltaci/GoDrop/releases/latest), and the
container image is at
[ghcr.io/fatihbaltaci/godrop](https://github.com/fatihbaltaci/GoDrop/pkgs/container/godrop).

### Platforms

| | Server | CLI | Notes |
| --- | :---: | :---: | --- |
| **Linux** (amd64, arm64) | ✅ | ✅ | Where it is meant to run: `.deb`, `.rpm`, `.apk`, container image, systemd unit |
| **macOS** (Intel, Apple silicon) | ✅ | ✅ | Fine for development and small installs; no systemd, so use Docker or start it yourself |
| **Windows** (amd64, arm64) | ⚠️ | ✅ | The binary works and is tested in CI, but there is no service wrapper, no installer script and no firewall guidance. Take the zip from Releases |
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

  GoDrop 1.0.0 setup

  Public address
  ? Public URL              https://files.example.com
  Storage
  ? Data directory          /var/lib/godrop
  ? Maximum file size       100MB
  ? Storage quota           20GB
  ? Delete files after      (never)
  HTTPS
  ? Certificate             automatic, from Let's Encrypt
  Service
  ? Deployment style        docker compose
  Finishing up
  ? Anonymous heartbeat     yes
  ? Verify reachability     yes

  Written
  ✓ .env  (chmod 600, contains your token)
  ✓ docker-compose.yml

  Your API token
  ┌────────────────────────────────────────┐
  │ gd_7f3a9c2e1b8d4a6f0c5e2d9b3a7f1e4c    │
  └────────────────────────────────────────┘
  ⚠ shown once and never again, so copy it now

  Verifying
  ✓ 127.0.0.1:443         listening
  ✓ firewall              ufw allows port 443
  ✓ firewall              ufw allows port 80
  ✓ external access       reachable (HTTP 200, from FRA)
```

Answering "automatic" is all HTTPS takes: GoDrop gets the certificate itself
and renews it, so there is no proxy to install and nothing to configure. The
question only offers it for a name Let's Encrypt can actually issue for, and
the listen port question disappears, because serving TLS means 443.

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
| `GET`/`HEAD` | `/f/{id}/{name}` · `/f/{id}.{ext}` | | download; supports Range and ETag |
| `DELETE` | `/f/{id}/{name}` · `/f/{id}.{ext}` | ✅ | delete |
| `GET` | `/healthz` | | liveness |
| `GET` | `/readyz` | | readiness: is storage writable |
| `GET` | `/stats` | ✅ | file count, bytes, quota, uptime |
| `GET` | `/llms.txt` · `/openapi.yaml` | | machine-readable description |

```bash
# Upload several files at once: all succeed or none do
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
restored onto another machine keeps working, which machine-bound encryption
would break for no security gain. A running server notices new and revoked
tokens without a restart.

`GODROP_TOKENS` still works and is the practical choice on Docker, Fly and
Railway; both sources are accepted together.

## Command line

Every command documents itself. This is that output, generated from the binary
so it cannot drift:

<!-- BEGIN CLI -->

<details>
<summary><code>godrop --help</code></summary>

```console
$ godrop --help
GoDrop is a tiny self-hosted file host.

Running it without a subcommand starts the server, so a container image needs
no command of its own.

Usage:
  godrop [flags]
  godrop [command]

Available Commands:
  completion  Generate the autocompletion script for the specified shell
  doctor      Diagnose the installation and print how to fix what is broken
  health      Probe a running instance (used by the container HEALTHCHECK)
  help        Help about any command
  init        Guided setup: configure, generate a token, start and verify
  serve       Start the HTTP server
  skill       Install the agent skill that teaches a coding agent to use GoDrop
  telemetry   Inspect or change the anonymous heartbeat
  token       Create, list and revoke API tokens
  update      Update GoDrop to the latest release
  version     Print version information

Flags:
  -h, --help       help for godrop
      --json       machine-readable output
      --no-color   disable coloured output

Use "godrop [command] --help" for more information about a command.
```

</details>

<details>
<summary><code>godrop serve --help</code></summary>

```console
$ godrop serve --help
Start the HTTP server.

Everything is configured through the environment, so there are no flags to
learn and the same settings work under systemd, Docker and a bare shell. See
.env.example for the annotated list, or run "godrop doctor" to see what the
current environment actually resolves to.

Plain http is fine on loopback, on a private network or over Tailscale. On a
public address, GODROP_TLS=auto gets a certificate from Let's Encrypt and
renews it, and GODROP_TLS_CERT with GODROP_TLS_KEY uses one you already have.
Neither needs a reverse proxy.

Usage:
  godrop serve [flags]

Flags:
  -h, --help   help for serve

Global Flags:
      --json       machine-readable output
      --no-color   disable coloured output
```

</details>

<details>
<summary><code>godrop init --help</code></summary>

```console
$ godrop init --help
Walks through the handful of decisions GoDrop needs, writes the configuration
files, creates your first API token and, if you like, starts the service and
confirms that the outside world can actually reach it.

Every answer can be supplied as a flag instead; with --no-input the same wizard
runs without asking anything, which is what CI and agents should use. Prompts
are skipped automatically when there is no terminal.

Usage:
  godrop init [flags]

Flags:
      --base-url string         public URL, e.g. https://files.example.com
      --data-dir string         where uploaded files are stored (default "/var/lib/godrop")
      --deployment string       compose, systemd or env (default "compose")
      --force                   overwrite existing configuration files
  -h, --help                    help for init
      --max-file-size string    per-file limit, e.g. 100MB (default "100MB")
      --max-total-size string   storage quota, empty for unlimited (default "20GB")
      --no-external-check       do not ask godrop.sh to verify reachability
      --no-input                never prompt; use flags and defaults (for CI and agents)
      --out-dir string          where to write the generated files (default: working directory)
      --port string             listen port (default "8747")
      --retention string        delete files after this long, e.g. 30d
      --start                   start the service when setup finishes
      --telemetry               send the anonymous daily heartbeat (default true)
      --tls string              auto (Let's Encrypt), file, proxy or none
      --tls-cert string         certificate chain in PEM, with --tls=file
      --tls-key string          private key in PEM, with --tls=file
      --token-name string       name for the generated token (default "default")

Global Flags:
      --json       machine-readable output
      --no-color   disable coloured output
```

</details>

<details>
<summary><code>godrop token --help</code></summary>

```console
$ godrop token --help
Manage the API tokens that authorise uploads and deletes.

Tokens are stored as SHA-256 digests, so the clear-text value is shown exactly
once, when it is created. A running server notices changes within a
second, with no restart needed.

Usage:
  godrop token [command]

Available Commands:
  create      Create a new API token
  list        List tokens (names only, values are not recoverable)
  revoke      Revoke a token by name

Flags:
      --data-dir string   data directory (default $GODROP_DATA_DIR or ./data)
  -h, --help              help for token

Global Flags:
      --json       machine-readable output
      --no-color   disable coloured output

Use "godrop token [command] --help" for more information about a command.
```

</details>

<details>
<summary><code>godrop token create --help</code></summary>

```console
$ godrop token create --help
Create a new API token

Usage:
  godrop token create [flags]

Flags:
  -h, --help          help for create
      --name string   label for this token (e.g. claude-code, ci, blog)

Global Flags:
      --data-dir string   data directory (default $GODROP_DATA_DIR or ./data)
      --json              machine-readable output
      --no-color          disable coloured output
```

</details>

<details>
<summary><code>godrop token list --help</code></summary>

```console
$ godrop token list --help
List tokens (names only, values are not recoverable)

Usage:
  godrop token list [flags]

Flags:
  -h, --help   help for list

Global Flags:
      --data-dir string   data directory (default $GODROP_DATA_DIR or ./data)
      --json              machine-readable output
      --no-color          disable coloured output
```

</details>

<details>
<summary><code>godrop token revoke --help</code></summary>

```console
$ godrop token revoke --help
Revoke a token by name

Usage:
  godrop token revoke <name> [flags]

Flags:
  -h, --help   help for revoke

Global Flags:
      --data-dir string   data directory (default $GODROP_DATA_DIR or ./data)
      --json              machine-readable output
      --no-color          disable coloured output
```

</details>

<details>
<summary><code>godrop doctor --help</code></summary>

```console
$ godrop doctor --help
Checks configuration, storage, security posture, network reachability and
available updates, then prints the exact command that fixes each problem.

Run it on the server for the full picture, or point it at a remote instance
with --url. Exits non-zero when a check fails, so it works as a deployment
gate.

The token for a remote check is read from GODROP_TOKEN, so that it stays out
of the process list and the shell history:

  GODROP_TOKEN=gd_... godrop doctor --url https://files.example.com

Usage:
  godrop doctor [flags]

Flags:
      --check-url string   reachability service (default https://godrop.sh/api/check)
  -h, --help               help for doctor
      --offline            skip every check that needs the network
      --token string       API token for the round-trip check; prefer GODROP_TOKEN, which does not appear in the process list
      --url string         diagnose a remote instance at this base URL

Global Flags:
      --json       machine-readable output
      --no-color   disable coloured output
```

</details>

<details>
<summary><code>godrop skill --help</code></summary>

```console
$ godrop skill --help
Agent skills are folders holding a SKILL.md that tells a coding agent how
to do something. GoDrop ships one, so an agent that has never seen it can
upload a file and hand back a link without being told how.

The skill needs no secrets: it reads GODROP_URL and GODROP_TOKEN from the
environment, so the same file is safe to commit alongside a project.

It can also be installed with the GitHub CLI, which supports every agent:

  gh skill install fatihbaltaci/GoDrop godrop --scope user

Usage:
  godrop skill [command]

Available Commands:
  install     Write the skill into an agent's skill directory
  show        Print the skill, so it can be piped somewhere else

Flags:
  -h, --help   help for skill

Global Flags:
      --json       machine-readable output
      --no-color   disable coloured output

Use "godrop skill [command] --help" for more information about a command.
```

</details>

<details>
<summary><code>godrop skill install --help</code></summary>

```console
$ godrop skill install --help
Write the skill into an agent's skill directory.

Project scope (the default) installs into the working directory, so the skill
travels with the repository. User scope installs into your home directory,
where every project can see it.

Usage:
  godrop skill install [flags]

Flags:
      --agent string   shared (most agents) or claude; use --dir for anything else (default "shared")
      --dir string     install into this directory instead, overriding --agent and --scope
      --force          replace an existing skill
  -h, --help           help for install
      --scope string   project or user (default "project")

Global Flags:
      --json       machine-readable output
      --no-color   disable coloured output
```

</details>

<details>
<summary><code>godrop skill show --help</code></summary>

```console
$ godrop skill show --help
Print the skill, so it can be piped somewhere else

Usage:
  godrop skill show [flags]

Flags:
  -h, --help   help for show

Global Flags:
      --json       machine-readable output
      --no-color   disable coloured output
```

</details>

<details>
<summary><code>godrop update --help</code></summary>

```console
$ godrop update --help
Download the newest release and put it in place of this binary.

Nothing is replaced until the download has been checked against the published
SHA256SUMS and the new binary has been run and seen to report its own version,
so a failed update leaves the working installation exactly as it was. The file
is then swapped with a rename, which is atomic: a server that is already
running keeps serving from the binary it started with, and restarts into the
new one.

Installations owned by something else are refused rather than overwritten. Use
apt, dnf or brew for those, and pull a new image for a container.

Usage:
  godrop update [flags]

Flags:
      --check            report whether a newer release exists, without installing it
  -h, --help             help for update
      --version string   install this release instead of the newest, e.g. v1.2.0

Global Flags:
      --json       machine-readable output
      --no-color   disable coloured output
```

</details>

<details>
<summary><code>godrop telemetry --help</code></summary>

```console
$ godrop telemetry --help
GoDrop sends one anonymous heartbeat per day:

    {install_id, version, os, arch, deploy}

That is the whole payload. No file names, no identifiers, no counters, no
addresses, no base URL. Use `godrop telemetry status --json` to see the exact
body that would be transmitted.

Usage:
  godrop telemetry [command]

Available Commands:
  off         Disable the anonymous heartbeat
  on          Enable the anonymous heartbeat
  status      Show whether telemetry is active and what would be sent

Flags:
      --data-dir string   data directory (default $GODROP_DATA_DIR or ./data)
  -h, --help              help for telemetry

Global Flags:
      --json       machine-readable output
      --no-color   disable coloured output

Use "godrop telemetry [command] --help" for more information about a command.
```

</details>

<details>
<summary><code>godrop health --help</code></summary>

```console
$ godrop health --help
Probe a running instance (used by the container HEALTHCHECK)

Usage:
  godrop health [flags]

Flags:
  -h, --help         help for health
      --url string   URL to probe (default: the local listen address)

Global Flags:
      --json       machine-readable output
      --no-color   disable coloured output
```

</details>

<details>
<summary><code>godrop version --help</code></summary>

```console
$ godrop version --help
Print version information

Usage:
  godrop version [flags]

Flags:
  -h, --help   help for version

Global Flags:
      --json       machine-readable output
      --no-color   disable coloured output
```

</details>

<!-- END CLI -->

## Configuration

Everything is an environment variable. Sizes accept `100MB`, `2GB`, `512KB`;
durations accept `30d`, `12h`, `90m`; rates accept `60/m`, `10/s`, `100/h`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `GODROP_TOKENS` | *(required)* | Comma-separated API tokens |
| `GODROP_BASE_URL` | *(from request)* | Public URL used in responses |
| `GODROP_ADDR` | `:8747` (`:443` with TLS) | Listen address |
| `GODROP_DATA_DIR` | `./data` | Where files live (`/data` in the image) |
| `GODROP_TLS` | `off` | `auto` for Let's Encrypt, `file` for your own certificate |
| `GODROP_TLS_DOMAINS` | *(from base URL)* | Names to get a certificate for |
| `GODROP_TLS_EMAIL` | *(none)* | Expiry warnings from Let's Encrypt |
| `GODROP_TLS_CACHE_DIR` | `<data dir>/acme` | Account key and certificates |
| `GODROP_TLS_CERT` / `GODROP_TLS_KEY` | *(none)* | Full chain and key, in PEM |
| `GODROP_HTTP_ADDR` | `:80` with TLS | Redirect and challenge listener, `off` to disable |
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
unguessable. The extension is the only metadata kept. The MIME type is derived
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
`/healthz` from the public internet, the only way to catch a cloud firewall,
which is invisible from inside the machine. Only the URL is sent; skip it with
`--offline`.

To diagnose an instance from your own machine, pass its address and a token.
The token goes in the environment, not on the command line, where the process
list and the shell history would both keep a copy:

```bash
GODROP_TOKEN=gd_... godrop doctor --url https://files.example.com
```

## Updating

```bash
godrop update --check     # is there a newer release?
godrop update             # install it
```

Nothing is replaced until the download has been checked against the published
`SHA256SUMS` **and** the new binary has been run and seen to report its own
version, so a failed update leaves the working installation exactly as it was.
The swap itself is a rename, which is atomic: a running server keeps serving
from the binary it started with and picks up the new one when it restarts.

An installation that belongs to a package manager is refused rather than
overwritten, with the command that does the job instead:

| Installed with | Update with |
| --- | --- |
| `install.sh`, or a downloaded archive | `godrop update` |
| `.deb` | `sudo apt update && sudo apt install --only-upgrade godrop` |
| `.rpm` | `sudo dnf upgrade godrop` |
| Docker | `docker pull ghcr.io/fatihbaltaci/godrop` |
| `go install` | `go install github.com/fatihbaltaci/GoDrop/cmd/godrop@latest` |

## Deploying

### On your own machine, a LAN or Tailscale

TLS is not required. GoDrop speaks plain http, and on a network where nobody
can read the traffic that is the right choice rather than a compromise:

```bash
GODROP_TOKENS=$(openssl rand -hex 16) \
GODROP_BASE_URL=http://localhost:8747 \
godrop serve
```

Set `GODROP_BASE_URL` to whatever the client will actually type, because that
is what the returned URLs are built from: `http://localhost:8747`,
`http://100.101.102.103:8747` for a Tailscale address, or
`http://nas.local:8747` on a home network. Leave it unset and the URL is
derived from the request, which also works.

`godrop doctor` judges plain http by who could be listening. Loopback and
Tailscale pass, because nothing readable leaves the machine in the first case
and the connection is already encrypted in the second. A LAN address warns:
tokens are readable by anything else on that network. A public address fails.

### A public server

```bash
curl -fsSL https://godrop.sh/install.sh | sh    # installs and runs `godrop init`
```

The wizard asks how you want the certificate, and "GoDrop gets one for me" is
the first answer. See [HTTPS](#https) below for what each answer does.

`deploy/` holds a [Caddy](deploy/Caddyfile) and an [nginx](deploy/nginx.conf)
configuration for anyone who wants a proxy anyway, and a hardened
[`godrop.service`](deploy/godrop.service) for systemd. Whichever proxy you use,
**raise its body size limit** to match `GODROP_MAX_FILE_SIZE`. `godrop doctor`
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
paid instance and pins the service to one instance, because GoDrop stores files
on local disk and does not scale horizontally, by design.

## HTTPS

GoDrop can serve https itself, so a public install needs no proxy at all:

```bash
GODROP_TLS=auto
GODROP_BASE_URL=https://files.example.com
```

That is the whole configuration. On the first request GoDrop gets a
certificate from Let's Encrypt, keeps it in `<data dir>/acme` and renews it
long before it expires.

**Open 443 and 80** to the internet. Port 80 answers the certificate
challenge and redirects anyone who typed `http://`, so an install that opens
only 443 waits for a certificate that never arrives. On a VPS that means both
the host firewall and your provider's, which is invisible from inside the
machine:

```bash
sudo ufw allow 443,80/tcp
# and the same two ports in the AWS security group, Hetzner firewall or GCP rule
```

`godrop init` and `godrop doctor` both check the two ports and say which one
is missing. If port 80 is genuinely unavailable, set `GODROP_HTTP_ADDR=off`
and the certificate is still issued over 443 alone, through `acme-tls/1`.

Already have a certificate, from certbot, your company CA or your cloud
provider? Name the two files and nothing else changes:

```bash
GODROP_TLS_CERT=/etc/letsencrypt/live/files.example.com/fullchain.pem
GODROP_TLS_KEY=/etc/letsencrypt/live/files.example.com/privkey.pem
```

`godrop doctor` then reports how many days that certificate has left, and
whether anyone else on the machine can read the key.

In Docker it is the same two variables and the two ports:

```bash
docker run -d --name godrop --restart always \
  -p 443:443 -p 80:80 \
  -e GODROP_TOKENS=$(openssl rand -hex 16) \
  -e GODROP_TLS=auto \
  -e GODROP_BASE_URL=https://files.example.com \
  -v godrop-data:/data \
  ghcr.io/fatihbaltaci/godrop
```

| Your situation | Setting |
| --- | --- |
| Public domain, nothing in front | `GODROP_TLS=auto` |
| A certificate you already have | `GODROP_TLS_CERT` and `GODROP_TLS_KEY` |
| Caddy, nginx, Traefik or a cloud load balancer in front | leave TLS off |
| Loopback, a LAN, Tailscale, a private network | leave TLS off |

Turning TLS on moves the listener to `:443` and starts a second one on `:80`,
unless you set `GODROP_ADDR` or `GODROP_HTTP_ADDR` yourself. Under systemd
that needs `AmbientCapabilities=CAP_NET_BIND_SERVICE`, which the shipped unit
already has; in Docker, publish `-p 443:443 -p 80:80`.

A certificate can only be issued for a public name that resolves to this
machine. `nas.local`, `10.0.0.5` and a Tailscale name are all refused at
startup, with the reason, rather than failing in a retry loop afterwards.

## For AI agents

GoDrop is built to be driven by coding agents. Give one a base URL and a token
and it can discover the rest:

```bash
curl https://files.example.com/llms.txt       # the whole API as plain text
curl https://files.example.com/openapi.yaml   # machine-readable schema
```

- Every command accepts `--json`, and in that mode prints **nothing but** the
  document, so parsing never breaks
- Colour and interactive prompts switch themselves off when there is no
  terminal, so an agent can never get stuck on a form

```bash
godrop token create --name claude-code --json | jq -r .token
godrop doctor --json | jq '.ok'
```

### Agent skills

GoDrop ships an [agent skill](skills/godrop/SKILL.md): the instructions a
coding agent needs to upload a file and hand back a link, without being told
how. Install it with GoDrop itself:

```bash
godrop skill install --scope user          # available in every project
godrop skill install                       # or just this repository
godrop skill install --agent claude        # Claude Code's own directory
```

Or with the GitHub CLI, which knows where every agent keeps them:

```bash
gh skill install fatihbaltaci/GoDrop godrop --scope user
```

The skill holds no secrets. It reads `GODROP_URL` and `GODROP_TOKEN` from the
environment, so it is safe to commit alongside a project:

```bash
export GODROP_URL=https://files.example.com
export GODROP_TOKEN=$(godrop token create --name claude-code --json | jq -r .token)
```

## Security

- **Unguessable identifiers**: 128 bits from `crypto/rand`; no enumeration
  endpoint exists
- **No path traversal, structurally**: the storage path is derived from the
  validated identifier alone; a client-supplied name never takes part in it
- **Active content is never rendered**: `.html`, `.svg`, `.xml` and friends are
  always sent as downloads, with `nosniff` and `Content-Security-Policy:
  default-src 'none'; sandbox` on every response
- **Names cannot lie**: `/f/<id>/setup.exe` does not resolve to a stored `.jpg`
- **Constant-time token comparison**, digests at rest, tokens never logged
- **Fails closed**: the server refuses to start without a token
- **Bounded everything**: per-file size, per-request size, file count, storage
  quota, and optional per-token and per-address rate limits
- **Logs are not a key ring**: the random half of an identifier is cut short
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

That is the entire payload: no file names, no counts, no addresses, no base
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
make docs         # regenerate the command line reference above
```

The website lives in [`site/`](site) and [`worker/`](worker), and is deployed
separately from the binary. See [`worker/README.md`](worker/README.md).

Releases come from [GoReleaser](https://goreleaser.com) via
[`.goreleaser.yaml`](.goreleaser.yaml): binaries, archives, checksums, Linux
packages and the changelog. CI builds the whole set on every change, so a tag
never fails on something that could have been caught earlier.

Every statement in `internal/` is covered by a test, and CI fails if that ever
slips. `cmd/godrop` is a three-line shim around `os.Exit` and is excluded.
The fuzz corpus in `internal/server/testdata/` includes a case that fuzzing
found: a file name that produced `..` inside a URL segment.

---

<p align="center">
  <sub>Sponsored by</sub>
</p>

<p align="center">
  <a href="https://getanteon.com/">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/getanteon/anteon/master/assets/anteon-logo-db.svg">
      <img src="https://raw.githubusercontent.com/getanteon/anteon/master/assets/anteon-logo-wb.svg" alt="Anteon" height="36">
    </picture>
  </a>
  &nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;
  <a href="https://gurubase.io/">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="https://gurubase.io/media/gurubase-dark-logo.png">
      <img src="https://gurubase.io/media/gurubase-light-logo.png" alt="Gurubase" height="34">
    </picture>
  </a>
</p>

<p align="center">
  <sub>
    The gopher is after the Go mascot by
    <a href="https://reneefrench.blogspot.com/">Renée French</a>, CC BY 3.0.
  </sub>
</p>
