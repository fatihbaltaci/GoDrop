// The godrop.sh Worker: a static site plus one dynamic route.
//
// Everything under site/ is served straight from Cloudflare's asset storage
// without the Worker running at all. Only /api/check reaches this code, and
// what it does is described in check.js.

import { handleCheck, handleOptions, handleOther } from "./check.js";

export default {
  async fetch(request, env) {
    const { pathname } = new URL(request.url);
    if (pathname === "/api/check") {
      if (request.method === "POST") return handleCheck(request);
      if (request.method === "OPTIONS") return handleOptions();
      return handleOther();
    }
    return env.ASSETS.fetch(request);
  },
};
