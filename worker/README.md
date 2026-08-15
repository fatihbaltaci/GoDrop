# godrop.sh

The site published at <https://godrop.sh>: a landing page, the install script
and one dynamic route. It is a Cloudflare Worker with static assets.

| Path | What it is |
| --- | --- |
| `../site/index.html` | Landing page |
| `../site/install.sh` | Copied from the repository root at build time |
| `../site/_headers` | Content types and security headers |
| `index.js` | Routes `/api/check` here and everything else to the assets |
| `check.js` | The reachability probe used by `godrop init` and `godrop doctor` |
| `../wrangler.jsonc` | Deployment configuration |

Nothing here affects the GoDrop binary.

## Deploying

Cloudflare dashboard: **Compute (Workers) → Create → Import a repository**,
pick this repository, then fill in:

| Field | Value |
| --- | --- |
| Project name | `godrop` |
| Build command | `cp install.sh site/install.sh` |
| Deploy command | `npx wrangler deploy` (the default) |

Leave the rest alone. `wrangler.jsonc` in the repository root supplies the
name, the entry point and the asset directory, so there is nothing to configure
in *Advanced settings*.

After the first deploy, attach the domain: **Workers & Pages → godrop →
Settings → Domains & Routes → Add → Custom domain → `godrop.sh`**.

Check it end to end:

```bash
curl -fsS https://godrop.sh/install.sh | head -3
curl -fsS -X POST -H 'content-type: application/json' \
  -d '{"url":"https://files.example.com/healthz"}' \
  https://godrop.sh/api/check
```

To try it locally first, without deploying anything:

```bash
npx wrangler dev            # http://localhost:8787
npx wrangler deploy --dry-run
```

## Rate limiting the probe

`/api/check` is unauthenticated, because the machine asking "can anyone reach
me?" has nothing to authenticate with yet. It is kept as narrow as an outbound
request can be: a single GET, to a health endpoint only, with no query string,
a 4KB request body limit and an 8 second ceiling. It cannot limit how often it
is called from inside a Worker.

That belongs in front of it. In the Cloudflare dashboard:

**Security → WAF → Rate limiting rules → Create rule**

- **Expression:** `http.request.uri.path eq "/api/check"`
- **Rate:** 10 requests per minute per IP
- **Action:** Block, 60 seconds

Without it, the worst case is wasted Worker invocations and a bill, not a
compromised host.
