---
name: godrop
description: Upload a file to a GoDrop server and get a public URL back. Use when you need to share a file, screenshot, log, build artifact or report as a link, including when a user asks for "a link to this file". Also covers deleting an uploaded file and checking a GoDrop instance's health.
---

# GoDrop

GoDrop is a small self-hosted file host. You upload a file with a token and get
back a URL that nobody can guess. Anyone with the URL can download it; no token
is needed to read.

## Setup

Two values are all you need:

```bash
export GODROP_URL=https://files.example.com
export GODROP_TOKEN=gd_...
```

If only the URL is known, `curl $GODROP_URL/llms.txt` returns the API and the
instance's actual limits.

## Upload a file

```bash
curl -sS -X POST \
  -H "Authorization: Bearer $GODROP_TOKEN" \
  -F "file=@report.pdf" \
  "$GODROP_URL/upload"
```

Response (`201`):

```json
{
  "files": [
    {
      "url": "https://files.example.com/f/20260815-143022-8f4e…/report.pdf",
      "name": "report.pdf",
      "size_bytes": 12345
    }
  ]
}
```

Read `.files[0].url` for the single-file case:

```bash
url=$(curl -sS -X POST -H "Authorization: Bearer $GODROP_TOKEN" \
        -F "file=@report.pdf" "$GODROP_URL/upload" | jq -r '.files[0].url')
echo "$url"
```

The response has one shape whatever was sent, so there is no branching to do.
The same URL is in the `Location` header, for when there is no JSON parser.

## Put a screenshot in a pull request or an issue

GitHub has no supported API for attaching an image, so upload it here and paste
the URL: it needs no token to open, and GitHub renders it inline for everyone
on the thread.

```bash
url=$(curl -sS -X POST -H "Authorization: Bearer $GODROP_TOKEN" \
        -H "X-Expires-In: 30d" -F "file=@screenshots/checkout.png" \
        "$GODROP_URL/upload" | jq -r '.files[0].url')

gh pr comment 42 --body "Checkout, after the fix:

![checkout]($url)"
```

The same URL works in an issue, a review comment, a commit message or a chat
message. Add `X-Expires-In` when the picture is only interesting while the pull
request is open.

## Upload several files

```bash
curl -sS -X POST -H "Authorization: Bearer $GODROP_TOKEN" \
  -F "file=@chart.png" -F "file=@data.csv" \
  "$GODROP_URL/upload" | jq -r '.files[].url'
```

The request is all-or-nothing: if one file is rejected, none are stored. At
most 20 files per request by default.

## Upload something that should not stick around

```bash
curl -sS -X POST -H "Authorization: Bearer $GODROP_TOKEN" \
  -H "X-Expires-In: 7d" -F "file=@invoice.pdf" \
  "$GODROP_URL/upload" | jq -r '.files[0].expires_at'
```

Durations are `30m`, `12h`, `7d`, `30d`. `?expires=7d` does the same thing.
The response carries `expires_at` when an expiry was asked for, and the file
answers `404` from the moment it passes. A server with a retention period caps
anything longer than it.

## Upload without multipart

Useful when piping generated content:

```bash
go test ./... 2>&1 | curl -sS -X PUT --data-binary @- \
  -H "Authorization: Bearer $GODROP_TOKEN" \
  "$GODROP_URL/upload/test-output.txt" | jq -r '.files[0].url' 
```

The extension in the path determines the stored type, so name it sensibly.

## Download

No authentication:

```bash
curl -sS -O "$url"
curl -sS -o out.pdf "$url"
```

Add `?dl=1` to force a download instead of inline rendering.

## Delete

```bash
curl -sS -X DELETE -H "Authorization: Bearer $GODROP_TOKEN" "$url"
```

`204` on success, `404` if it was already gone. **Keep the URL from the upload
response**: there is no listing endpoint, and a file whose URL is lost cannot
be found again.

## Check an instance

```bash
curl -sS "$GODROP_URL/healthz"                                  # is it up
curl -sS "$GODROP_URL/readyz"                                   # can it write
curl -sS -H "Authorization: Bearer $GODROP_TOKEN" "$GODROP_URL/stats" | jq
```

## Status codes

| Code | Meaning | What to do |
| --- | --- | --- |
| `201` | Uploaded | Read `.url` |
| `204` | Deleted | Nothing |
| `400` | Malformed body, or too many files | Send fewer files; check the multipart body |
| `401` | Missing or wrong token | Check `GODROP_TOKEN`; do not retry blindly |
| `404` | Unknown id, or the name's extension does not match | Use the URL exactly as returned |
| `413` | File larger than the limit | Split or compress it; `GET /` shows the limit |
| `415` | Not `multipart/form-data` | Use `-F`, or `PUT /upload/{name}` for a raw body |
| `429` | Rate limited | Wait for `Retry-After` seconds, then retry once |
| `507` | Server storage is full | Tell the user; deleting old files is their call |

Errors are always `{"error": "..."}`.

## Things worth knowing

- **Identifiers are unguessable and unlisted.** Store the returned URL if the
  file will be needed later.
- **The stored extension decides the Content-Type.** Give files a sensible name.
- **HTML and SVG uploads are always served as downloads**, never rendered, so do
  not expect an uploaded page to open in a browser.
- **Downloads support Range and ETag**, so resuming and conditional requests
  work normally.
- **Do not put the token in a URL**: it belongs in the header. Never paste it
  into a file you are about to upload.

## For a client that has no shell

An assistant that cannot run `curl` reaches the same instance over the Model
Context Protocol. Either as a URL:

```bash
claude mcp add --transport http godrop "$GODROP_URL/mcp" \
  --header "Authorization: Bearer $GODROP_TOKEN"
```

Or as a command, which needs no URL and no token in the client's configuration
because it reads both from the installation on that machine:

```json
{"mcpServers": {"godrop": {"command": "godrop", "args": ["mcp"]}}}
```

The tools are `upload_file`, `delete_file` and `storage_stats`, plus
`upload_local_file` when it is run as a command: that one takes a path and
streams the file, so it has no size limit of its own. Prefer the commands above
whenever a shell is available.

## Running your own

```bash
curl -fsSL https://godrop.sh/install.sh | sh    # installs, then guides setup
godrop token create --name agent --json         # a token for this agent
godrop doctor --json                            # diagnose an existing install
```

Every `godrop` command accepts `--json` and prints nothing else in that mode.
