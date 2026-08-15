# godrop.sh

The site published at <https://godrop.sh> via Cloudflare Pages.

| Path | What it is |
| --- | --- |
| `index.html` | Landing page |
| `install.sh` | Copied from the repository root at build time |
| `functions/api/check.js` | Reachability probe used by `godrop init` and `godrop doctor` |
| `_headers` | Content types and security headers |

## Deploying

Cloudflare Pages → Create a project → connect this repository, then:

- **Build command:** `cp install.sh site/install.sh`
- **Build output directory:** `site`
- **Root directory:** *(repository root)*

Functions under `site/functions` are deployed automatically, so `/api/check`
works without any extra configuration.

The probe fetches a URL from Cloudflare's edge and returns the status it saw.
It stores nothing, never reads the response body, and refuses private
addresses.
