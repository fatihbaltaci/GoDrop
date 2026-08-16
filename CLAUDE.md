# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make build          # binary into ./bin
make test           # every test with the race detector
make cover          # coverage, fails below 100%
make lint           # go vet, gofmt check, shellcheck install.sh
make docs           # regenerate everything that is generated (see below)
make run            # dev server on 127.0.0.1:48080 with a throwaway token
make fuzz           # the three input sanitiser fuzz targets, a minute each
make snapshot       # build every release artefact locally, without publishing
```

One package, one test, one fuzz target:

```bash
go test ./internal/storage/
go test ./internal/cli/ -run TestInitNonInteractiveWritesEverything
go test ./internal/server/ -run "^$" -fuzz FuzzSanitizeExt -fuzztime 30s
```

The whole suite runs in about six seconds (eight with `-race`); anything much
slower than that is a bug in the test, not a reason to wait.

golangci-lint is not in the Makefile but CI runs it, pinned:

```bash
golangci-lint run ./...      # v2.12.2, config in .golangci.yml
```

## What CI enforces

- **100% statement coverage of `./internal/...`**, checked by the Quality gate
  job. `cmd/godrop` is excluded deliberately: it is a wiring shim around
  `os.Exit`. When a branch is genuinely unreachable, delete it rather than
  lowering the bar; when it is reachable only through the filesystem, the OS or
  a clock, add a seam (a package-level `var` holding the function) and inject it
  from the test. Existing seams: `now`, `randRead`, `finish`, `renameFile`,
  `osExecutable`, `lookup`, `lookPath`, `runCommand`, `listenOn`, `euid`.
- `gofmt`, `go vet` for linux/amd64, linux/arm64, darwin/arm64 and
  windows/amd64, govulncheck, shellcheck, actionlint, `goreleaser check`, and a
  YAML parse of the embedded OpenAPI document.
- Tests on Linux, macOS **and Windows**. Windows is where path assumptions,
  `filepath` versus `path`, line endings and advisory file modes break; tests
  that depend on POSIX modes skip themselves there.

## Generated files: never edit by hand

`make docs` regenerates all of these, and drift tests fail CI if the committed
copy is stale:

| Generated | From | Guard |
| --- | --- | --- |
| README command line reference | `godrop --help` output | `internal/cli/readme_test.go` |
| `internal/cli/skill/SKILL.md` | `skills/godrop/SKILL.md` | `internal/cli/skill_test.go` |
| `site/logo-lockup.svg` | `site/logo.svg` + `site/wordmark.svg` | `scripts/logo.py` |
| `site/demo.svg` | the session in `scripts/demo.py` | `scripts/demo.py` |
| `internal/wizard/assets/sample.png` | `site/logo-lockup.svg`, rendered by Chrome | `internal/wizard/sample_test.go` compares a stamp of the source |

## Architecture

A request path with no database in it: the identifier *is* the index.
`20260815-143022-<32 hex>` maps to `<data dir>/2026/08/15/<id>.<ext>`, so a
lookup is a `stat`, retention is a directory sweep, and the 128 random bits are
what make a URL unguessable. An optional `-e<base36 unix seconds>` suffix
carries a per-upload expiry, which is why the identifier pattern lives in one
place (`internal/storage`) and everything else asks it.

- **`internal/config`** parses the environment into a `Config` and reports every
  problem at once rather than the first. TLS settings live in `config/tls.go`,
  including `ValidTLSDomain`, the single rule for what Let's Encrypt can issue
  for (the wizard defers to it so it never offers an answer the server will
  refuse at startup).
- **`internal/server`** is the HTTP API: routing, middleware, and handlers that
  hold no state of their own. `docs.go` serves `/llms.txt` and
  `assets/openapi.yaml`, both of which describe the limits *this instance*
  enforces.
- **`internal/storage`** owns identifiers, paths, quota reservation, the
  in-flight upload registry and the retention sweep.
- **`internal/tokens`** stores SHA-256 digests, reloads the file with a
  cross-process lock, and fails open on a read error (with a loud log line)
  rather than locking every client out.
- **`internal/wizard`** is `godrop init` with no UI in it: `Questions()` is the
  whole conversation as data, and both the sequential `Run(Prompter, Answers)`
  and the interactive form are built from that one list, so they cannot drift.
  It also owns the exact text of every generated file (`.env`, compose, unit,
  Caddyfile) and the follow-up steps.
- **`internal/cli`** turns that into commands. `form.go` renders the whole
  wizard as **one** huh form: a program per question means the terminal's reply
  to a background-colour query lands in the next question's field, which is what
  used to eat the first Enter. `preflight.go` checks everything the answers
  depend on (writable directories, sudo, docker, systemd, a free port) *before*
  a single file is written.
- **`internal/doctor`** is the diagnosis every check-shaped thing shares:
  configuration, storage, permissions, certificate expiry, firewall,
  reachability, updates. `godrop init` calls it in process rather than shelling
  out, so it works before `godrop` is on `PATH`.
- **`internal/updater`** replaces the running binary and cannot break a working
  installation: nothing touches the installed file until the download matches
  `SHA256SUMS` *and* the new binary has run and reported its own version, and
  the swap itself is an atomic rename. An installation owned by dpkg, rpm,
  Homebrew or a container image is refused, with the command that does the job.
- **`internal/tlsconf`** turns the TLS settings into an `http.Server` config,
  either from files or from `autocert`. Its plain-http listener redirects to a
  host from the configuration, never from the `Host` header.

## Conventions

- **Everything in the repository is English**: code, comments, README, CLI
  strings, commit messages. Conversation with the user is Turkish.
- **No em dashes**, anywhere.
- Commit messages carry no assistant attribution: no `Co-Authored-By`, no tool
  signature.
- Comments explain *why*, in whole sentences, and are worth as much scrutiny as
  the code. A comment that restates the line above it is noise.
- Error messages name the fix, not just the problem: the string a person reads
  when something goes wrong should contain the command that puts it right.
- Ports in local experiments are deliberately unusual (48080, 47951, …): this
  machine runs many projects at once.
