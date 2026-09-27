import test from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { once } from "node:events";
import { readFileSync } from "node:fs";
import { createWrapper, WORKER_LIMITS } from "./server.mjs";
import { evaluate } from "./interpreter.mjs";

const token = "synthetic-worker-token-32-characters";
async function fixture(t, options = {}) {
  const server = createWrapper({ token, diagnosticLogger: () => {}, ...options });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  t.after(() => {
    server.closeAllConnections();
    server.close();
  });
  return { server, url: `http://127.0.0.1:${server.address().port}` };
}
class FakeWorker extends EventEmitter {
  terminated = false;
  async terminate() {
    this.terminated = true;
  }
}
test("image parent heap does not undercut the bounded worker heap", () => {
  const dockerfile = readFileSync(new URL("./Dockerfile", import.meta.url), "utf8");
  const parentHeap = Number(dockerfile.match(/--max-old-space-size=(\d+)/)?.[1]);
  assert.ok(parentHeap >= WORKER_LIMITS.maxOldGenerationSizeMb);
});
test("player resolution has a bounded worker heap and longer deadline than browse", async (t) => {
  assert.deepEqual(WORKER_LIMITS, { maxOldGenerationSizeMb: 192, stackSizeMb: 4 });
  const { url } = await fixture(t, {
    timeoutMs: 100,
    resolveTimeoutMs: 500,
    workerFactory: () => {
      const worker = new FakeWorker();
      setTimeout(() => worker.emit("message", { ok: true, value: { mimeType: "video/mp4" } }), 220);
      return worker;
    },
  });
  const headers = { authorization: `Bearer ${token}` };
  const catalog = await fetch(url + "/catalog", { headers });
  assert.equal(catalog.status, 504);
  const resolved = await fetch(url + "/resolve/dQw4w9WgXcQ", { headers });
  assert.equal(resolved.status, 200);
  assert.deepEqual(await resolved.json(), { mimeType: "video/mp4" });
  const badQuality = await fetch(url + "/resolve/dQw4w9WgXcQ?quality=9999p", { headers });
  assert.equal(badQuality.status, 400);
  assert.deepEqual(await badQuality.json(), { error: "invalid_quality" });
});

test("quality parameter is passed to worker factory for HD resolution", async (t) => {
  let passedQuality = "";
  const { url } = await fixture(t, {
    workerFactory: (data) => {
      passedQuality = data.quality;
      const worker = new FakeWorker();
      setTimeout(
        () =>
          worker.emit("message", {
            ok: true,
            value: {
              url: "https://r.googlevideo.com/v",
              audioUrl: "https://r.googlevideo.com/a",
              mimeType: "video/mp4",
              variants: ["720p"],
            },
          }),
        20,
      );
      return worker;
    },
  });
  const headers = { authorization: `Bearer ${token}` };
  const resp = await fetch(url + "/resolve/dQw4w9WgXcQ?quality=720p", { headers });
  assert.equal(resp.status, 200);
  assert.equal(passedQuality, "720p");
  assert.deepEqual(await resp.json(), {
    url: "https://r.googlevideo.com/v",
    audioUrl: "https://r.googlevideo.com/a",
    mimeType: "video/mp4",
    variants: ["720p"],
  });
});

test("terminal resolve logs accept opaque IDs and redact private worker data", async (t) => {
  const lines = [];
  const privateValue = {
    url: "https://signed.example/video?sig=private-video-secret",
    audioUrl: "https://signed.example/audio?sig=private-audio-secret",
    mimeType: "video/mp4",
  };
  const { url } = await fixture(t, {
    diagnosticLogger: (line) => lines.push(line),
    workerFactory: () => {
      const worker = new FakeWorker();
      setImmediate(() =>
        worker.emit("message", {
          ok: true,
          value: privateValue,
          diagnostic: {
            reason: "selected",
            clients: [
              { client: "IOS", outcome: "selected", videoId: "private-video-id" },
              { client: "WEB", outcome: "secret-exception" },
            ],
            title: "private-title",
            exception: "cookie=private-cookie",
          },
        }),
      );
      return worker;
    },
  });

  const response = await fetch(url + "/resolve/dQw4w9WgXcQ?quality=720p", {
    headers: {
      authorization: `Bearer ${token}`,
      "x-zombie-diagnostic-id": "0123456789abcdef",
    },
  });
  assert.equal(response.status, 200);
  assert.deepEqual(await response.json(), privateValue);
  assert.equal(lines.length, 1);
  const log = JSON.parse(lines[0]);
  assert.deepEqual(log, {
    event: "youtube_wrapper_terminal",
    diagnostic_id: "0123456789abcdef",
    operation: "resolve",
    quality: "720p",
    elapsed_ms: log.elapsed_ms,
    worker_lifecycle: "completed",
    final_reason: "selected",
    client_outcomes: [
      { client: "IOS", outcome: "selected" },
      { client: "VISIONOS", outcome: "not_reached" },
      { client: "ANDROID", outcome: "not_reached" },
      { client: "WEB", outcome: "not_reached" },
    ],
    stream_mode: "split",
    video_present: true,
    audio_present: true,
  });
  assert.ok(log.elapsed_ms >= 0 && log.elapsed_ms <= 30_000);
  for (const secret of [
    "private-video-secret",
    "private-audio-secret",
    "private-video-id",
    "private-title",
    "private-cookie",
    "dQw4w9WgXcQ",
  ]) {
    assert.equal(lines[0].includes(secret), false);
  }

  const catalog = await fetch(url + "/catalog", {
    headers: {
      authorization: `Bearer ${token}`,
      "x-zombie-diagnostic-id": "0123456789ABCDEF",
    },
  });
  assert.equal(catalog.status, 200);
  assert.equal(lines.length, 2);
  const generated = JSON.parse(lines[1]);
  assert.match(generated.diagnostic_id, /^[0-9a-f]{16}$/);
  assert.notEqual(generated.diagnostic_id, "0123456789ABCDEF");
  assert.equal(generated.operation, "catalog");
  assert.equal(generated.quality, "auto");
  assert.equal(generated.worker_lifecycle, "completed");
  assert.equal(generated.final_reason, "completed");

  const browse = await fetch(url + "/browse?offset=361", {
    headers: { authorization: `Bearer ${token}` },
  });
  assert.equal(browse.status, 400);
  assert.equal(lines.length, 3);
  const browseLog = JSON.parse(lines[2]);
  assert.equal(browseLog.operation, "browse");
  assert.equal(browseLog.worker_lifecycle, "failed");
  assert.equal(browseLog.final_reason, "invalid_browse");
});

test("busy rejection logs once while the active worker keeps its slot", async (t) => {
  const lines = [];
  let activeWorker;
  const { url } = await fixture(t, {
    diagnosticLogger: (line) => lines.push(line),
    workerFactory: () => {
      activeWorker = new FakeWorker();
      return activeWorker;
    },
  });
  const options = { headers: { authorization: `Bearer ${token}` } };
  const first = fetch(url + "/catalog", options);
  while (!activeWorker) await new Promise((resolve) => setTimeout(resolve, 5));
  const busy = await fetch(url + "/resolve/dQw4w9WgXcQ?quality=1080p", options);
  assert.equal(busy.status, 503);
  assert.deepEqual(await busy.json(), { error: "busy" });
  assert.equal(lines.length, 1);
  assert.deepEqual(JSON.parse(lines[0]), {
    event: "youtube_wrapper_terminal",
    diagnostic_id: JSON.parse(lines[0]).diagnostic_id,
    operation: "resolve",
    quality: "1080p",
    elapsed_ms: JSON.parse(lines[0]).elapsed_ms,
    worker_lifecycle: "busy",
    final_reason: "busy",
    client_outcomes: [
      { client: "IOS", outcome: "not_reached" },
      { client: "VISIONOS", outcome: "not_reached" },
      { client: "ANDROID", outcome: "not_reached" },
      { client: "WEB", outcome: "not_reached" },
    ],
  });
  activeWorker.emit("message", { ok: true, value: { items: [] } });
  const completed = await first;
  assert.equal(completed.status, 200);
  assert.equal(lines.length, 2);
  assert.equal(JSON.parse(lines[1]).worker_lifecycle, "completed");
});
test("interpreter evaluates without host access and stops infinite code", async () => {
  assert.deepEqual(await evaluate({ output: 'return {sig:"abc",n:"def"}' }), {
    sig: "abc",
    n: "def",
  });
  assert.equal(
    await evaluate({ output: 'return typeof process + ":" + typeof fetch + ":" + typeof require' }),
    "undefined:undefined:undefined",
  );
  await assert.rejects(evaluate({ output: "while(true){}" }));
});
test("unauthorized calls do not start workers; public health stays local", async (t) => {
  let starts = 0;
  const { url } = await fixture(t, {
    workerFactory: () => {
      starts++;
      return new FakeWorker();
    },
  });
  assert.equal((await fetch(url + "/health")).status, 200);
  assert.equal((await fetch(url + "/catalog")).status, 401);
  assert.equal(starts, 0);
});
test("concurrency, timeout and disconnect terminate workers", async (t) => {
  const lines = [];
  const workers = [];
  const { url } = await fixture(t, {
    timeoutMs: 100,
    diagnosticLogger: (line) => lines.push(line),
    workerFactory: () => {
      const w = new FakeWorker();
      workers.push(w);
      return w;
    },
  });
  const options = { headers: { authorization: `Bearer ${token}` } };
  const first = fetch(url + "/catalog", options);
  while (!workers.length) await new Promise((r) => setTimeout(r, 5));
  assert.equal((await fetch(url + "/catalog", options)).status, 503);
  assert.equal((await first).status, 504);
  assert.equal(workers[0].terminated, true);
  assert.equal(lines.length, 2);
  assert.equal(JSON.parse(lines[0]).worker_lifecycle, "busy");
  assert.equal(JSON.parse(lines[1]).worker_lifecycle, "timed_out");
  const cancel = new AbortController();
  const second = fetch(url + "/catalog", { ...options, signal: cancel.signal }).catch(() => {});
  while (workers.length < 2) await new Promise((r) => setTimeout(r, 5));
  cancel.abort();
  await second;
  for (let i = 0; i < 30 && !workers[1].terminated; i++) await new Promise((r) => setTimeout(r, 5));
  assert.equal(workers[1].terminated, true);
  assert.equal(lines.length, 3);
  assert.equal(JSON.parse(lines[2]).worker_lifecycle, "cancelled");
});

test("failed resolve logs known categories while preserving its public error", async (t) => {
  const lines = [];
  const { url } = await fixture(t, {
    diagnosticLogger: (line) => lines.push(line),
    workerFactory: () => {
      const worker = new FakeWorker();
      setImmediate(() =>
        worker.emit("message", {
          ok: false,
          diagnostic: {
            reason: "audio_unavailable",
            clients: [
              { client: "IOS", outcome: "audio_unavailable", url: "private-url" },
              { client: "VISIONOS", outcome: "format_unavailable", title: "private-title" },
            ],
            exception: "cookie=private-cookie",
          },
        }),
      );
      return worker;
    },
  });
  const response = await fetch(url + "/resolve/dQw4w9WgXcQ?quality=480p", {
    headers: { authorization: `Bearer ${token}` },
  });
  assert.equal(response.status, 502);
  assert.deepEqual(await response.json(), { error: "provider_unavailable" });
  assert.equal(lines.length, 1);
  const log = JSON.parse(lines[0]);
  assert.equal(log.worker_lifecycle, "failed");
  assert.equal(log.final_reason, "audio_unavailable");
  assert.deepEqual(log.client_outcomes, [
    { client: "IOS", outcome: "audio_unavailable" },
    { client: "VISIONOS", outcome: "format_unavailable" },
    { client: "ANDROID", outcome: "not_reached" },
    { client: "WEB", outcome: "not_reached" },
  ]);
  for (const secret of ["private-url", "private-title", "private-cookie", "dQw4w9WgXcQ"])
    assert.equal(lines[0].includes(secret), false);
});
test("failure response never exposes upstream secrets", async (t) => {
  const lines = [];
  const { url } = await fixture(t, {
    diagnosticLogger: (line) => lines.push(line),
    workerFactory: () => {
      const worker = new FakeWorker();
      setImmediate(() => worker.emit("error", new Error("cookie=private")));
      return worker;
    },
  });
  const result = await fetch(url + "/catalog", { headers: { authorization: `Bearer ${token}` } });
  assert.equal(result.status, 502);
  assert.deepEqual(await result.json(), { error: "provider_unavailable" });
  assert.equal(lines.length, 1);
  assert.equal(lines[0].includes("cookie=private"), false);
  assert.equal(JSON.parse(lines[0]).worker_lifecycle, "failed");
  assert.equal(JSON.parse(lines[0]).final_reason, "worker_failed");
});
