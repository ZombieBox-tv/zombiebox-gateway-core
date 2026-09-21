import { test } from "node:test";
import assert from "node:assert/strict";
import { Bridge } from "./bridge.mjs";
const state = {
  success: true,
  state: "PLAYING",
  positionMs: 1000,
  durationMs: 10000,
  volume: 75,
  muted: false,
};
test("only matching acknowledgement completes a command; retries are idempotent", async () => {
  const bridge = new Bridge();
  const result = bridge.command("play", { videoId: "abcdefghijk" });
  const id = bridge.pending.command.id;
  assert.throws(() => bridge.acknowledge({ ...state, commandId: "foreign" }));
  bridge.acknowledge({ ...state, commandId: id });
  assert.equal(await result, true);
  bridge.acknowledge({ ...state, commandId: id });
  bridge.close();
});
test("stop preempts pending play and close rejects pending command", async () => {
  const bridge = new Bridge();
  const play = bridge.command("play");
  const stop = bridge.command("stop");
  assert.equal(await play, false);
  bridge.close();
  assert.equal(await stop, false);
});
test("unresponsive receiver times out and releases capacity", async () => {
  const bridge = new Bridge(10);
  assert.equal(await bridge.command("pause"), false);
  assert.equal(bridge.pending, null);
  bridge.close();
});
