import assert from "node:assert/strict";
import test from "node:test";
import { Receiver } from "./receiver.mjs";
import { Bridge } from "./bridge.mjs";

test("handoff keeps the listener and rejects acknowledgements from its former epoch", async () => {
  const receiver = new Receiver({});
  const bridge = new Bridge();
  let notifications = 0;
  receiver.session = {
    id: "a".repeat(32),
    epoch: "",
    state: "READY",
    tvCode: "123 456 789",
    bridge,
    player: {
      async notifyExternalStateChange() {
        notifications++;
      },
    },
  };
  const pending = bridge.command("play", { videoId: "abcdefghijk" });
  const epoch = "b".repeat(32);
  receiver.suspend(epoch);
  assert.equal(await pending, false);
  assert.equal(receiver.active, true);
  assert.equal(receiver.snapshot().tvCode, "123 456 789");
  assert.equal(receiver.snapshot().command, null);
  assert.equal(bridge.state.state, "STOPPED");
  assert.throws(
    () => receiver.acknowledge({ ...bridge.state, success: true, epoch: "" }),
    /stale_epoch/,
  );
  receiver.acknowledge({ ...bridge.state, success: true, epoch });
  assert.equal(notifications, 2);
  bridge.close();
});
