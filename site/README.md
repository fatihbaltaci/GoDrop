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
It stores nothing and never reads the response body.

## Rate limiting the probe

`/api/check` is unauthenticated, because the machine asking "can anyone reach
me?" has nothing to authenticate with yet. It is kept as narrow as an outbound
request can be — a single GET, to a health endpoint only, with no query string,
a 4KB request body limit and an 8 second ceiling — but it cannot limit how
often it is called from inside a Function.

That belongs in front of it. In the Cloudflare dashboard:

**Security → WAF → Rate limiting rules → Create rule**

- **Expression:** `http.request.uri.path eq "/api/check"`
- **Rate:** 10 requests per minute per IP
- **Action:** Block, 60 seconds

Without it, the worst case is wasted Function invocations and a bill, not a
compromised host.
