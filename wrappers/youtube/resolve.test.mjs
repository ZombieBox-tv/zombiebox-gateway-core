import test from "node:test";
import assert from "node:assert/strict";
import { resolveVideo } from "./resolve.mjs";

test("resolution falls back when the first client has no format data", async () => {
  const clients = [];
  const yt = {
    session: { player: {} },
    async getBasicInfo(_, { client }) {
      clients.push(client);
      if (client === "ANDROID") throw new Error("not available");
      return {
        chooseFormat() {
          return {
            itag: 18,
            has_audio: true,
            has_video: true,
            decipher: async () => "IOS-stream",
          };
        },
      };
    },
  };
  const result = await resolveVideo(yt, "aqz-KE-bpKQ", async () => true);
  assert.equal(result.mimeType, "video/mp4");
  assert.deepEqual(clients, ["ANDROID", "IOS"]);
});

test("resolution stops after the first validated playable client", async () => {
  const clients = [];
  const yt = {
    session: { player: {} },
    async getBasicInfo(_, { client }) {
      clients.push(client);
      return {
        chooseFormat() {
          return {
            itag: 18,
            has_audio: true,
            has_video: true,
            decipher: async () => "ANDROID-stream",
          };
        },
      };
    },
  };
  await resolveVideo(yt, "aqz-KE-bpKQ", async () => true);
  assert.deepEqual(clients, ["ANDROID"]);
});

test("invalid Android media falls back to another client before returning", async () => {
  const clients = [];
  const yt = {
    session: { player: {} },
    async getBasicInfo(_, { client }) {
      clients.push(client);
      return {
        chooseFormat(options) {
          if (options.type !== "video+audio" || options.quality !== "360p")
            throw new Error("no format");
          return {
            itag: client === "ANDROID" ? 18 : 22,
            has_audio: true,
            has_video: true,
            decipher: async () => `${client}-stream`,
          };
        },
      };
    },
  };
  const result = await resolveVideo(yt, "dQw4w9WgXcQ", (url) => url !== "ANDROID-stream");
  assert.deepEqual(result, { url: "IOS-stream", mimeType: "video/mp4" });
  assert.deepEqual(clients, ["ANDROID", "IOS"]);
});
