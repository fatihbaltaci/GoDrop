// Reachability probe for godrop.sh.
//
// A self-hosted server cannot tell whether the outside world can reach it: a
// request to its own public address may loop back through NAT, and a cloud
// provider's firewall is invisible from inside the instance. This endpoint
// fetches the given URL from Cloudflare's edge and reports what it saw.
//
// It receives a URL and returns a status. It does not store anything, does not
// read the response body, and never follows a redirect.
//
// It is unauthenticated, so it is deliberately kept as narrow as an outbound
// request primitive can be: one GET, to a health endpoint only, no query
// string, a bounded request body and an eight-second ceiling. Volume is the
// one thing it cannot limit by itself — that belongs to a Cloudflare rate
// limiting rule on /api/check, which site/README.md describes.

const TIMEOUT_MS = 8000;
const MAX_URL_LENGTH = 2048;
const MAX_BODY_BYTES = 4096;

// The only thing this endpoint exists to fetch is a health endpoint. Anything
// else would make it a general "fetch this URL for me" service on the open
// internet — a free scanner and a status oracle for anyone who finds it.
const ALLOWED_PATHS = [/^\/?$/, /\/healthz$/];

// Hosts that would make this a proxy into private infrastructure. Cloudflare's
// fetch cannot reach RFC1918 space from the edge, but rejecting them outright
// gives a clearer error than a timeout.
//
// This is a courtesy check, not the boundary: a hostname resolving to a private
// address is not caught here, and must not be relied on to be.
const BLOCKED_HOSTS = [
  /^localhost$/i,
  /\.localhost$/i,
  /^127\./,
  /^0\./,
  /^10\./,
  /^192\.168\./,
  /^172\.(1[6-9]|2\d|3[01])\./,
  /^169\.254\./,
  /^\[?::1\]?$/,
  /^\[?f[cd][0-9a-f]{2}:/i,
  /^\[?fe80:/i,
  /\.internal$/i,
  /\.local$/i,
];

// readBounded returns the body as text, or null when it goes past limit bytes.
// The size is checked while reading rather than after: a declared
// content-length can lie, and a chunked body declares nothing at all.
async function readBounded(request, limit) {
  const declared = Number(request.headers.get("content-length"));
  if (Number.isFinite(declared) && declared > limit) return null;
  if (!request.body) return "";

  const reader = request.body.getReader();
  const decoder = new TextDecoder();
  let text = "";
  let size = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > limit) {
      await reader.cancel();
      return null;
    }
    text += decoder.decode(value, { stream: true });
  }
  return text + decoder.decode();
}

const json = (body, status = 200) =>
  new Response(JSON.stringify(body), {
    status,
    headers: {
      "content-type": "application/json; charset=utf-8",
      "cache-control": "no-store",
      "access-control-allow-origin": "*",
    },
  });

export async function onRequestOptions() {
  return new Response(null, {
    status: 204,
    headers: {
      "access-control-allow-origin": "*",
      "access-control-allow-methods": "POST, OPTIONS",
      "access-control-allow-headers": "content-type",
      "access-control-max-age": "86400",
    },
  });
}

export async function onRequestPost({ request }) {
  const body = await readBounded(request, MAX_BODY_BYTES);
  if (body === null) {
    return json({ ok: false, error: "body too large" }, 413);
  }

  let payload;
  try {
    payload = JSON.parse(body);
  } catch {
    return json({ ok: false, error: "expected a JSON body with a url field" }, 400);
  }

  const raw = typeof payload?.url === "string" ? payload.url.trim() : "";
  if (!raw || raw.length > MAX_URL_LENGTH) {
    return json({ ok: false, error: "missing or oversized url" }, 400);
  }

  let target;
  try {
    target = new URL(raw);
  } catch {
    return json({ ok: false, error: "url is not valid" }, 400);
  }
  if (target.protocol !== "http:" && target.protocol !== "https:") {
    return json({ ok: false, error: "only http and https are supported" }, 400);
  }
  if (BLOCKED_HOSTS.some((pattern) => pattern.test(target.hostname))) {
    return json({
      ok: false,
      error:
        "that address is only reachable from your own network, so it cannot be checked from the internet",
    }, 400);
  }
  if (!ALLOWED_PATHS.some((pattern) => pattern.test(target.pathname))) {
    return json({
      ok: false,
      error: "only a health endpoint can be checked, e.g. https://files.example.com/healthz",
    }, 400);
  }
  // A query string or a fragment adds nothing to a reachability check and only
  // widens what this endpoint can be pointed at.
  target.search = "";
  target.hash = "";

  const started = Date.now();
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), TIMEOUT_MS);

  try {
    const response = await fetch(target.toString(), {
      method: "GET",
      redirect: "manual",
      signal: controller.signal,
      headers: { "user-agent": "godrop.sh reachability check (https://godrop.sh)" },
    });
    clearTimeout(timer);

    // The body is never read: only whether the request arrived matters.
    const status = response.status;
    const colo = request.cf?.colo ?? "";
    const reachable = status > 0 && status < 500;

    return json({
      ok: reachable,
      status,
      location: colo,
      duration_ms: Date.now() - started,
      error: reachable ? "" : `the server answered ${status}`,
    });
  } catch (err) {
    clearTimeout(timer);
    const aborted = err?.name === "AbortError";
    return json({
      ok: false,
      status: 0,
      location: request.cf?.colo ?? "",
      duration_ms: Date.now() - started,
      error: aborted
        ? `no answer within ${TIMEOUT_MS / 1000}s — the port is probably closed or filtered`
        : `could not connect: ${err?.message ?? "unknown error"}`,
    });
  }
}

// Anything other than POST gets a usable hint rather than a bare 405.
export async function onRequest({ request }) {
  if (request.method === "POST") return onRequestPost({ request });
  if (request.method === "OPTIONS") return onRequestOptions();
  return json({
    ok: false,
    error: 'POST {"url":"https://files.example.com/healthz"} to check reachability',
  }, 405);
}
