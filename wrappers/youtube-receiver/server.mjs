import http from "node:http";
import { readFile } from "node:fs/promises";
import { timingSafeEqual } from "node:crypto";
import { Receiver } from "./receiver.mjs";

const config = JSON.parse(
  await readFile(process.env.ZOMBIE_YOUTUBE_RECEIVER_CONFIG || "/config/receiver.json", "utf8"),
);
if (typeof config.token !== "string" || config.token.length < 32)
  throw Error("Private configuration required");
// Upstream uses fetch internally without a caller-supplied transport. Bound it
// inside this dedicated process; failures never affect the gateway process.
const upstreamFetch = globalThis.fetch;
globalThis.fetch = (input, init = {}) =>
  upstreamFetch(input, {
    ...init,
    signal: init.signal
      ? AbortSignal.any([init.signal, AbortSignal.timeout(30000)])
      : AbortSignal.timeout(30000),
  });
const receiver = new Receiver(config);
let changing = false;
async function body(req) {
  let raw = "";
  for await (const chunk of req) {
    raw += chunk;
    if (Buffer.byteLength(raw) > 8192) throw Error("request_too_large");
  }
  return JSON.parse(raw || "{}");
}
function json(res, status, data) {
  res.writeHead(status, {
    "Content-Type": "application/json",
    "Cache-Control": "no-store",
    "X-Content-Type-Options": "nosniff",
  });
  res.end(JSON.stringify(data));
}
const server = http.createServer(async (req, res) => {
  const supplied = Buffer.from(req.headers.authorization || ""),
    expected = Buffer.from("Bearer " + config.token);
  if (supplied.length !== expected.length || !timingSafeEqual(supplied, expected)) {
    json(res, 401, { error: "unauthorized" });
    return;
  }
  if (req.method === "GET" && req.url === "/health") {
    json(res, 200, { available: true, active: receiver.active });
    return;
  }
  if (changing) {
    json(res, 409, { error: "receiver_busy" });
    return;
  }
  try {
    if (req.method === "POST" && req.url === "/receiver") {
      if (receiver.active) {
        json(res, 409, { error: "receiver_busy" });
        return;
      }
      changing = true;
      try {
        const value = await body(req);
        await receiver.start(value.receiverId);
        json(res, 201, receiver.snapshot());
      } finally {
        changing = false;
      }
      return;
    }
    const match = req.url.match(/^\/receiver\/([a-f0-9]{32})(\/state|\/suspend)?$/);
    if (!match || !receiver.active || receiver.id !== match[1]) {
      json(res, 404, { error: "receiver_not_found" });
      return;
    }
    receiver.touch();
    if (req.method === "GET" && !match[2]) {
      json(res, 200, receiver.snapshot());
      return;
    }
    if (req.method === "POST" && match[2] === "/suspend") {
      receiver.suspend((await body(req)).epoch);
      json(res, 200, { suspended: true });
      return;
    }
    if (req.method === "POST" && match[2] === "/state") {
      const state = await body(req);
      receiver.acknowledge(state);
      json(res, 200, { accepted: true });
      return;
    }
    if (req.method === "DELETE" && !match[2]) {
      changing = true;
      try {
        await receiver.stop();
        json(res, 200, { closed: true });
      } finally {
        changing = false;
      }
      return;
    }
    json(res, 405, { error: "method_not_allowed" });
  } catch {
    json(res, 502, { error: "receiver_unavailable" });
  }
});
server.requestTimeout = 25000;
server.headersTimeout = 5000;
server.maxConnections = 8;
const reaper = setInterval(() => {
  if (receiver.expired && !changing) {
    changing = true;
    void receiver.stop().finally(() => {
      changing = false;
    });
  }
}, 5000);
server.listen(config.port || 8095, config.listen || "0.0.0.0");
for (const signal of ["SIGTERM", "SIGINT"])
  process.on(signal, () => {
    clearInterval(reaper);
    server.close();
    void receiver.stop().finally(() => process.exit(0));
  });
