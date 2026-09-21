import test from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { once } from "node:events";
import { createWrapper } from "./server.mjs";
import { evaluate } from "./interpreter.mjs";

const token = "synthetic-worker-token-32-characters";
async function fixture(t, options = {}) {
  const server = createWrapper({ token, ...options });
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
  const workers = [];
  const { url } = await fixture(t, {
    timeoutMs: 100,
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
  const cancel = new AbortController();
  const second = fetch(url + "/catalog", { ...options, signal: cancel.signal }).catch(() => {});
  while (workers.length < 2) await new Promise((r) => setTimeout(r, 5));
  cancel.abort();
  await second;
  for (let i = 0; i < 30 && !workers[1].terminated; i++) await new Promise((r) => setTimeout(r, 5));
  assert.equal(workers[1].terminated, true);
});
test("failure response never exposes upstream secrets", async (t) => {
  const { url } = await fixture(t, {
    workerFactory: () => {
      const worker = new FakeWorker();
      setImmediate(() => worker.emit("error", new Error("cookie=private")));
      return worker;
    },
  });
  const result = await fetch(url + "/catalog", { headers: { authorization: `Bearer ${token}` } });
  assert.equal(result.status, 502);
  assert.deepEqual(await result.json(), { error: "provider_unavailable" });
});
