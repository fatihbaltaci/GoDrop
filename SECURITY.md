# Security

## Reporting a vulnerability

Open a [private security advisory][advisory] on GitHub. Please do not open a
public issue for anything exploitable.

Include what you did, what happened and what you expected; a `curl` command is
worth more than a paragraph. Expect an acknowledgement within a few days.

Only the latest release is supported.

[advisory]: https://github.com/fatihbaltaci/GoDrop/security/advisories/new

## What GoDrop defends

A single-tenant file host, run by the person who holds its tokens.

| Asset | Protected by |
| --- | --- |
| Stored files | 128-bit identifiers, no listing endpoint, no enumeration |
| Uploads and deletes | Bearer tokens, compared in constant time, stored as SHA-256 digests |
| The host filesystem | Paths derived only from validated identifiers; per-file, per-request and total size limits |
| Browsers fetching a file | Forced downloads for active content, `nosniff`, `Content-Security-Policy: default-src 'none'; sandbox` |
| Availability | Optional rate limits per token and per address, bounded request bodies |

## What it does not defend

Being explicit about this is more useful than a longer list of features.

- **Anyone with the URL can download the file.** That is the design: the URL is
  the capability. Treat it like a password — do not paste it where it will be
  indexed, and re-upload if it leaks. There is no per-file access control, no
  expiry per file and no signed URL.
- **A token is all-or-nothing.** It can upload, delete and read `/stats`. There
  are no scopes and no per-token quotas. Issue one per client with
  `godrop token create` so a leak can be revoked without disturbing the others.
- **Nothing is encrypted at rest.** Files are ordinary files on disk. Use full
  disk encryption if that matters.
- **There is no multi-tenancy.** Every token can delete every file.
- **Slow clients are the reverse proxy's problem.** `GODROP_READ_TIMEOUT` and
  `GODROP_WRITE_TIMEOUT` default to none, because cutting a legitimate 2GB
  upload in half is worse than the connection it holds. Go's own timeouts apply
  to a whole request, so any value large enough for the slowest acceptable
  upload is too large to stop a client that trickles. A proxy can draw the line
  per read instead, which is what the examples in `deploy/` do —
  `client_body_timeout` and `send_timeout` for nginx, a `timeouts` block for
  Caddy. Set the GoDrop variables yourself if you expose it directly.

## Deliberate decisions

These come up often enough to write down.

**Tokens are hashed, not encrypted.** A digest cannot be turned back into a
working token, and — unlike machine-bound encryption — `tokens.json` still
works after being restored onto another host. A plain SHA-256 is right here
rather than bcrypt or argon2: tokens are 128-bit random values, not
human-chosen passwords, so there is nothing to brute force.

**Reloading the token file fails open.** If `tokens.json` becomes unreadable
while the server is running, it keeps using the last good copy rather than
rejecting every request. An operational mistake should not take a working
service down. It is not silent: the server logs it on every distinct failure,
and until it is fixed a revoked token stays valid.

**Logs never contain a whole identifier.** Logs travel further than the files
they describe. Enough of the identifier is kept to match a line to its upload
and find the file on disk:

```bash
ls /var/lib/godrop/2026/08/15/20260815-143022-8f4e2c91*
```

What is dropped is the part that makes the URL unguessable.

**Retention only ever deletes uploads.** `tokens.json`, `.install_id` and
`.telemetry-off` live in the same directory, and deleting them by age would
revoke every token or turn telemetry back on. Cleanup considers only files
whose name is a valid identifier sitting at the exact path that identifier maps
to; symbolic links are never followed.

**`godrop doctor --token` is the worse option.** A token on the command line is
visible in the process list to every local account and lands in the shell
history. Use the environment:

```bash
GODROP_TOKEN=gd_... godrop doctor --url https://files.example.com
```

**CORS is open by default.** `GODROP_CORS_ORIGINS` defaults to `*` so that a
browser front end works without configuration. Downloads need no credentials,
so this grants nothing that fetching the URL directly would not. Restrict it if
you only ever call GoDrop from your own front end.

## Hardening checklist

`godrop doctor` checks most of this and prints the command that fixes each
item.

- [ ] A reverse proxy in front, terminating TLS — `GODROP_BASE_URL` on `https`
- [ ] `GODROP_MAX_TOTAL_SIZE` set, and larger than `GODROP_MAX_FILE_SIZE`
- [ ] `GODROP_RATE_LIMIT` set if the instance is reachable from the internet
- [ ] Data directory `0700`, owned by the service user (the `.deb` and `.rpm`
      packages and `deploy/godrop.service` do this)
- [ ] Not running as root
- [ ] One token per client, so a leak can be revoked in isolation
- [ ] `GODROP_CORS_ORIGINS` restricted if no browser front end needs it
- [ ] Backups of the data directory, `tokens.json` included
