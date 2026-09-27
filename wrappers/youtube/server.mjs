import { validParent } from "./browse.mjs";
import { createServer } from "node:http";
import { randomBytes, timingSafeEqual } from "node:crypto";
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
const RESOLVE_CLIENTS = Object.freeze(["IOS", "VISIONOS", "ANDROID", "WEB"]);
const CLIENT_OUTCOMES = new Set([
  "basic_info_failed",
  "format_unavailable",
  "audio_unavailable",
  "client_budget_exhausted",
  "selected",
  "not_reached",
]);
const RESOLVE_REASONS = new Set([
  "selected",
  "basic_info_failed",
  "format_unavailable",
  "audio_unavailable",
  "client_budget_exhausted",
]);
const WORKER_LIFECYCLES = new Set([
  "busy",
  "started",
  "completed",
  "failed",
  "timed_out",
  "cancelled",
]);
const FINAL_REASONS = new Set([
  ...RESOLVE_REASONS,
  "busy",
  "method_not_allowed",
  "unauthorized",
  "not_found",
  "invalid_query",
  "invalid_browse",
  "invalid_quality",
  "worker_unavailable",
  "provider_timeout",
  "cancelled",
  "worker_failed",
  "provider_failed",
  "completed",
]);
const MAX_DIAGNOSTIC_ELAPSED_MS = 30_000;

function requestOperation(pathname) {
  if (pathname === "/catalog") return "catalog";
  if (pathname === "/browse") return "browse";
  if (pathname === "/resolve" || pathname.startsWith("/resolve/")) return "resolve";
  return "";
}

function normalizeResolveDiagnostic(diagnostic, success) {
  const clients = RESOLVE_CLIENTS.map((client) => ({ client, outcome: "not_reached" }));
  if (Array.isArray(diagnostic?.clients)) {
    for (const entry of diagnostic.clients.slice(0, RESOLVE_CLIENTS.length)) {
      if (!entry || typeof entry !== "object") continue;
      const index = RESOLVE_CLIENTS.indexOf(entry.client);
      if (index < 0 || !CLIENT_OUTCOMES.has(entry.outcome)) continue;
      clients[index].outcome = entry.outcome;
    }
  }
  const reason = success
    ? "selected"
    : RESOLVE_REASONS.has(diagnostic?.reason) && diagnostic.reason !== "selected"
      ? diagnostic.reason
      : "provider_failed";
  return { reason, clients };
}

function generatedDiagnosticId(header) {
  return typeof header === "string" && /^[0-9a-f]{16}$/.test(header)
    ? header
    : randomBytes(8).toString("hex");
}

export function createWrapper({
  token,
  cookie = "",
  poToken = "",
  visitorData = "",
  timeoutMs = 7000,
  resolveTimeoutMs = 15000,
  diagnosticLogger = (line) => console.log(line),
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
    let operation = requestOperation(typeof req.url === "string" ? req.url.split("?", 1)[0] : "");
    let loggedQuality = "auto";
    let diagnosticId = "";
    const requestStartedAt = Date.now();
    let logged = false;
    const reply = (status, data) => {
      if (!res.destroyed && !res.writableEnded) {
        res.writeHead(status, { "content-type": "application/json", "cache-control": "no-store" });
        res.end(JSON.stringify(data));
      }
    };
    const logTerminal = ({
      lifecycle = "failed",
      reason,
      success = false,
      value,
      resolveDiagnostic,
    }) => {
      if (!operation || logged) return;
      logged = true;
      const record = {
        event: "youtube_wrapper_terminal",
        diagnostic_id: diagnosticId,
        operation,
        quality: loggedQuality,
        elapsed_ms: Math.min(MAX_DIAGNOSTIC_ELAPSED_MS, Math.max(0, Date.now() - requestStartedAt)),
        worker_lifecycle: WORKER_LIFECYCLES.has(lifecycle) ? lifecycle : "failed",
        final_reason: FINAL_REASONS.has(reason) ? reason : "provider_failed",
      };
      if (operation === "resolve") {
        const normalized = normalizeResolveDiagnostic(resolveDiagnostic, success);
        record.final_reason = success
          ? normalized.reason
          : FINAL_REASONS.has(reason)
            ? reason
            : normalized.reason;
        record.client_outcomes = normalized.clients;
        if (success) {
          const videoPresent = typeof value?.url === "string" && value.url.length > 0;
          const split = typeof value?.audioUrl === "string" && value.audioUrl.length > 0;
          record.stream_mode = split ? "split" : videoPresent ? "multiplex" : "unavailable";
          record.video_present = videoPresent;
          record.audio_present = split || videoPresent;
        }
      }
      try {
        diagnosticLogger(JSON.stringify(record));
      } catch {
        // Diagnostics must not change request behavior if the logger fails.
      }
    };
    const terminalReply = (status, data, reason, lifecycle = "failed") => {
      logTerminal({ reason, lifecycle });
      return reply(status, data);
    };
    if (req.method !== "GET") {
      diagnosticId = operation ? generatedDiagnosticId(req.headers["x-zombie-diagnostic-id"]) : "";
      return terminalReply(405, { error: "method_not_allowed" }, "method_not_allowed");
    }
    const url = new URL(req.url, "http://wrapper");
    operation = requestOperation(url.pathname);
    const requestedQuality = url.searchParams.get("quality") ?? "";
    loggedQuality = ["", "auto", "1080p", "720p", "480p", "360p"].includes(requestedQuality)
      ? requestedQuality || "auto"
      : "auto";
    diagnosticId = operation ? generatedDiagnosticId(req.headers["x-zombie-diagnostic-id"]) : "";
    if (url.pathname === "/health") return reply(200, { status: "ok", busy: active !== null });
    if (!equal(req.headers.authorization ?? "", `Bearer ${token}`))
      return terminalReply(401, { error: "unauthorized" }, "unauthorized");
    const id = url.pathname.startsWith("/resolve/") ? url.pathname.slice(9) : "";
    if (url.pathname !== "/catalog" && url.pathname !== "/browse" && !/^[\w-]{11}$/.test(id))
      return terminalReply(404, { error: "not_found" }, "not_found");
    const query = url.searchParams.get("q") ?? "";
    if (query.length > 200) return terminalReply(400, { error: "invalid_query" }, "invalid_query");
    const parent = url.searchParams.get("parent") ?? "";
    const offset = Number(url.searchParams.get("offset") ?? 0);
    if (!validParent(parent) || !Number.isInteger(offset) || offset < 0 || offset > 360)
      return terminalReply(400, { error: "invalid_browse" }, "invalid_browse");
    const quality = url.searchParams.get("quality") ?? "";
    if (quality && !["1080p", "720p", "480p", "360p"].includes(quality))
      return terminalReply(400, { error: "invalid_quality" }, "invalid_quality");
    if (active) return terminalReply(503, { error: "busy" }, "busy", "busy");
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
      return terminalReply(503, { error: "worker_unavailable" }, "worker_unavailable");
    }
    active = worker;
    let done = false;
    const finish = (
      status,
      value,
      { lifecycle = "failed", reason = "provider_failed", resolveDiagnostic, success = false } = {},
    ) => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      logTerminal({ lifecycle, reason, success, value, resolveDiagnostic });
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
    const timer = setTimeout(
      () =>
        finish(
          504,
          { error: "provider_timeout" },
          { lifecycle: "timed_out", reason: "provider_timeout" },
        ),
      deadlineMs,
    );
    worker.once("message", (message) => {
      const success = Boolean(message?.ok);
      const normalized =
        operation === "resolve" ? normalizeResolveDiagnostic(message?.diagnostic, success) : null;
      finish(success ? 200 : 502, success ? message.value : { error: "provider_unavailable" }, {
        lifecycle: success ? "completed" : "failed",
        reason: success
          ? operation === "resolve"
            ? normalized.reason
            : "completed"
          : (normalized?.reason ?? "provider_failed"),
        resolveDiagnostic: message?.diagnostic,
        success,
      });
    });
    worker.once("error", () =>
      finish(
        502,
        { error: "provider_unavailable" },
        { lifecycle: "failed", reason: "worker_failed" },
      ),
    );
    worker.once("exit", () =>
      finish(
        502,
        { error: "provider_unavailable" },
        { lifecycle: "failed", reason: "worker_failed" },
      ),
    );
    res.once("close", () => {
      if (!res.writableEnded)
        finish(499, { error: "cancelled" }, { lifecycle: "cancelled", reason: "cancelled" });
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
