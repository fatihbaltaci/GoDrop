// Reachability probe for godrop.sh.
//
// A self-hosted server cannot tell whether the outside world can reach it: a
// request to its own public address may loop back through NAT, and a cloud
// provider's firewall is invisible from inside the instance. This endpoint
// fetches the given URL from Cloudflare's edge and reports what it saw.
//
// It receives a URL and returns a status. It does not store anything, does not
// read the response body, and never follows a redirect to a different host.

const TIMEOUT_MS = 8000;
const MAX_URL_LENGTH = 2048;

// Hosts that would make this a proxy into private infrastructure. Cloudflare's
// fetch cannot reach RFC1918 space from the edge, but rejecting them outright
// gives a clearer error than a timeout.
const BLOCKED_HOSTS = [
  /^localhost$/i,
  /^127\./,
  /^0\./,
  /^10\./,
  /^192\.168\./,
  /^172\.(1[6-9]|2\d|3[01])\./,
  /^169\.254\./,
  /^\[?::1\]?$/,
  /^\[?f[cd][0-9a-f]{2}:/i,
  /\.internal$/i,
  /\.local$/i,
];

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
  let payload;
  try {
    payload = await request.json();
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
