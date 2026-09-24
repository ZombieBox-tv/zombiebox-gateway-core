import assert from "node:assert/strict";
import test from "node:test";
import { resolveVideo } from "./resolve.mjs";

test("resolution falls back when a client only exposes undecipherable formats", async () => {
  const clients = [];
  const yt = {
    session: { player: {} },
    async getBasicInfo(_id, options) {
      clients.push(options.client);
      if (options.client === "IOS") throw new Error("not available");
      return {
        chooseFormat() {
          return {
            has_audio: true,
            has_video: true,
            async decipher() {
              return "https://r1.googlevideo.com/videoplayback";
            },
          };
        },
      };
    },
  };
  const result = await resolveVideo(yt, "aqz-KE-bpKQ");
  assert.equal(result.mimeType, "video/mp4");
  assert.deepEqual(clients, ["IOS", "ANDROID"]);
});

test("resolution stops after the first playable client", async () => {
  const clients = [];
  const yt = {
    session: { player: {} },
    async getBasicInfo(_id, options) {
      clients.push(options.client);
      return {
        chooseFormat() {
          return {
            has_audio: true,
            has_video: true,
            async decipher() {
              return "https://r1.googlevideo.com/videoplayback";
            },
          };
        },
      };
    },
  };
  await resolveVideo(yt, "aqz-KE-bpKQ");
  assert.deepEqual(clients, ["IOS"]);
});
