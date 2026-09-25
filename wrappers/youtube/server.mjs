import { validParent } from "./browse.mjs";
import { createServer } from "node:http";
import { timingSafeEqual } from "node:crypto";
import { readFileSync } from "node:fs";
import { Worker } from "node:worker_threads";
import { pathToFileURL } from "node:url";

const equal = (a, b) => {
  const left = Buffer.from(a),
    right = Buffer.from(b);
  return left.length === right.length && timingSafeEqual(left, right);
};

// Resolving YouTube.js player scripts exceeded the former 96 MiB worker heap
// during physical evaluation. Keep one worker at a time and bound its lifetime.
export const WORKER_LIMITS = Object.freeze({ maxOldGenerationSizeMb: 192, stackSizeMb: 4 });

export function createWrapper({
  token,
  cookie = "",
  poToken = "",
  visitorData = "",
  timeoutMs = 7000,
  resolveTimeoutMs = 15000,
  workerFactory = (data) =>
    new Worker(new URL("./worker.mjs", import.meta.url), {
      workerData: data,
      execArgv: [],
      resourceLimits: WORKER_LIMITS,
    }),
}) {
  if (!token || token.length < 32) throw new Error("worker_token_required");
  let active = null;
  const server = createServer((req, res) => {
    const reply = (status, data) => {
      if (!res.destroyed && !res.writableEnded) {
        res.writeHead(status, { "content-type": "application/json", "cache-control": "no-store" });
        res.end(JSON.stringify(data));
      }
    };
    if (req.method !== "GET") return reply(405, { error: "method_not_allowed" });
    const url = new URL(req.url, "http://wrapper");
    if (url.pathname === "/health") return reply(200, { status: "ok", busy: active !== null });
    if (!equal(req.headers.authorization ?? "", `Bearer ${token}`))
      return reply(401, { error: "unauthorized" });
    const id = url.pathname.startsWith("/resolve/") ? url.pathname.slice(9) : "";
    if (url.pathname !== "/catalog" && url.pathname !== "/browse" && !/^[\w-]{11}$/.test(id))
      return reply(404, { error: "not_found" });
    const query = url.searchParams.get("q") ?? "";
    if (query.length > 200) return reply(400, { error: "invalid_query" });
    const parent = url.searchParams.get("parent") ?? "";
    const offset = Number(url.searchParams.get("offset") ?? 0);
    if (!validParent(parent) || !Number.isInteger(offset) || offset < 0 || offset > 360)
      return reply(400, { error: "invalid_browse" });
    const quality = url.searchParams.get("quality") ?? "";
    if (quality && !["1080p", "720p", "480p", "360p"].includes(quality))
      return reply(400, { error: "invalid_quality" });
    if (active) return reply(503, { error: "busy" });
    let worker;
    try {
      worker = workerFactory({
        operation: id ? "resolve" : url.pathname === "/browse" ? "browse" : "catalog",
        parent,
        offset,
        id,
        quality,
        query,
        cookie,
        poToken,
        visitorData,
      });
    } catch {
      return reply(503, { error: "worker_unavailable" });
    }
    active = worker;
    let done = false;
    const finish = (status, value) => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      reply(status, value);
      // Keep the capacity reserved until worker resources have actually exited.
      Promise.resolve(worker.terminate())
        .catch(() => {})
        .finally(() => {
          if (active === worker) active = null;
        });
    };
    // A video requires player retrieval and format deciphering; catalog requests
    // retain their shorter deadline. Both stay below the gateway's 20s private
    // HTTP client deadline, even when private configuration overrides defaults.
    const deadlineMs = Math.min(18000, Math.max(100, id ? resolveTimeoutMs : timeoutMs));
    const timer = setTimeout(() => finish(504, { error: "provider_timeout" }), deadlineMs);
    worker.once("message", (message) =>
      finish(
        message.ok ? 200 : 502,
        message.ok ? message.value : { error: "provider_unavailable" },
      ),
    );
    worker.once("error", () => finish(502, { error: "provider_unavailable" }));
    worker.once("exit", () => finish(502, { error: "provider_unavailable" }));
    res.once("close", () => {
      if (!res.writableEnded) finish(499, { error: "cancelled" });
    });
  });
  server.headersTimeout = 3000;
  server.requestTimeout = 10000;
  server.keepAliveTimeout = 2000;
  server.maxHeadersCount = 30;
  server.on("close", () => {
    active?.terminate();
  });
  return server;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  let server;
  try {
    const config = JSON.parse(
      readFileSync(process.env.ZOMBIE_YOUTUBE_CONFIG ?? ".local/youtube.json", "utf8"),
    );
    server = createWrapper(config);
    server.listen(Number(process.env.PORT ?? 8091), process.env.HOST ?? "127.0.0.1", () =>
      console.log("YouTube wrapper ready"),
    );
  } catch {
    console.error("YouTube wrapper configuration unavailable or invalid");
    process.exit(1);
  }
  for (const signal of ["SIGINT", "SIGTERM"])
    process.on(signal, () => {
      server.closeAllConnections();
      server.close();
    });
}
