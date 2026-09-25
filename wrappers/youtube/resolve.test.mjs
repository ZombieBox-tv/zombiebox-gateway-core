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
  assert.deepEqual(result, { url: "IOS-stream", mimeType: "video/mp4", variants: ["360p"] });
  assert.deepEqual(clients, ["ANDROID", "IOS"]);
});

test("selecting 720p resolves H.264 video and AAC audio from IOS when Android lacks HD", async () => {
  const clients = [];
  const yt = {
    session: { player: {} },
    async getBasicInfo(_, { client }) {
      clients.push(client);
      return {
        chooseFormat(options) {
          if (client === "ANDROID") {
            // Android only has 360p combined, no HD
            if (options.quality === "360p" && options.type === "video+audio") {
              return {
                itag: 18,
                has_audio: true,
                has_video: true,
                decipher: async () => "android-360",
              };
            }
            throw new Error("not available on android");
          }
          if (options.type === "video" && options.quality === "720p") {
            return {
              itag: 136,
              has_audio: false,
              has_video: true,
              decipher: async () => "ios-720-video",
            };
          }
          if (options.type === "audio") {
            return {
              itag: 140,
              has_audio: true,
              has_video: false,
              decipher: async () => "ios-aac-audio",
            };
          }
          throw new Error("no format");
        },
      };
    },
  };
  const result = await resolveVideo(yt, "dQw4w9WgXcQ", async () => true, "720p");
  assert.equal(result.url, "ios-720-video");
  assert.equal(result.audioUrl, "ios-aac-audio");
  assert.equal(result.mimeType, "video/mp4");
  assert.deepEqual(result.variants, ["720p", "360p"]);
  assert.deepEqual(clients, ["ANDROID", "IOS"]);
});

test("auto resolve discovers HD variants across clients while keeping Android 360p source", async () => {
  const clients = [];
  const yt = {
    session: { player: {} },
    async getBasicInfo(_, { client }) {
      clients.push(client);
      return {
        chooseFormat(options) {
          if (client === "ANDROID") {
            // Android only has 360p combined, no HD
            if (options.quality === "360p" && options.type === "video+audio") {
              return {
                itag: 18,
                has_audio: true,
                has_video: true,
                decipher: async () => "android-360",
              };
            }
            throw new Error("not available on android");
          }
          if (options.type === "video" && options.quality === "1080p") {
            return {
              itag: 137,
              has_audio: false,
              has_video: true,
              decipher: async () => "ios-1080-video",
            };
          }
          if (options.type === "video" && options.quality === "720p") {
            return {
              itag: 136,
              has_audio: false,
              has_video: true,
              decipher: async () => "ios-720-video",
            };
          }
          if (options.type === "audio") {
            return {
              itag: 140,
              has_audio: true,
              has_video: false,
              decipher: async () => "ios-aac-audio",
            };
          }
          throw new Error("no format");
        },
      };
    },
  };
  const result = await resolveVideo(yt, "dQw4w9WgXcQ", async () => true);
  assert.equal(result.url, "android-360");
  assert.equal(result.audioUrl, undefined);
  assert.equal(result.mimeType, "video/mp4");
  assert.deepEqual(result.variants, ["1080p", "720p", "360p"]);
  assert.deepEqual(clients, ["ANDROID", "IOS"]);
});

test("auto resolve omits HD variants when adaptive video fails range or origin validation", async () => {
  const yt = {
    session: { player: {} },
    async getBasicInfo(_, { client }) {
      return {
        chooseFormat(options) {
          if (client === "ANDROID") {
            if (options.quality === "360p" && options.type === "video+audio") {
              return {
                itag: 18,
                has_audio: true,
                has_video: true,
                decipher: async () => "android-360",
              };
            }
            throw new Error("no android hd");
          }
          if (options.type === "video" && options.quality === "1080p") {
            return {
              itag: 137,
              has_audio: false,
              has_video: true,
              decipher: async () => "ios-1080-invalid",
            };
          }
          if (options.type === "video" && options.quality === "720p") {
            return {
              itag: 136,
              has_audio: false,
              has_video: true,
              decipher: async () => "ios-720-valid",
            };
          }
          if (options.type === "audio") {
            return {
              itag: 140,
              has_audio: true,
              has_video: false,
              decipher: async () => "ios-aac-audio",
            };
          }
          throw new Error("no format");
        },
      };
    },
  };
  const validate = async (url) => !url.includes("invalid");
  const result = await resolveVideo(yt, "dQw4w9WgXcQ", validate);
  assert.equal(result.url, "android-360");
  assert.deepEqual(result.variants, ["720p", "360p"]);
});

test("auto resolve omits HD variants when adaptive audio fails validation", async () => {
  const yt = {
    session: { player: {} },
    async getBasicInfo(_, { client }) {
      return {
        chooseFormat(options) {
          if (client === "ANDROID") {
            if (options.quality === "360p" && options.type === "video+audio") {
              return {
                itag: 18,
                has_audio: true,
                has_video: true,
                decipher: async () => "android-360",
              };
            }
            throw new Error("no android hd");
          }
          if (options.type === "video" && options.quality === "720p") {
            return {
              itag: 136,
              has_audio: false,
              has_video: true,
              decipher: async () => "ios-720-video",
            };
          }
          if (options.type === "audio") {
            return {
              itag: 140,
              has_audio: true,
              has_video: false,
              decipher: async () => "ios-aac-bad-range",
            };
          }
          throw new Error("no format");
        },
      };
    },
  };
  const validate = async (url) => !url.includes("bad-range");
  const result = await resolveVideo(yt, "dQw4w9WgXcQ", validate);
  assert.equal(result.url, "android-360");
  assert.deepEqual(result.variants, ["360p"]);
});

test("selecting HD throws audio_unavailable if audio is missing", async () => {
  const yt = {
    session: { player: {} },
    async getBasicInfo() {
      return {
        chooseFormat(options) {
          if (options.type === "video" && options.quality === "720p") {
            return {
              itag: 136,
              has_audio: false,
              has_video: true,
              decipher: async () => "video-url",
            };
          }
          throw new Error("missing audio");
        },
      };
    },
  };
  await assert.rejects(
    resolveVideo(yt, "dQw4w9WgXcQ", async () => true, "720p"),
    /audio_unavailable/,
  );
});

test("selecting unavailable tier throws video_unavailable", async () => {
  const yt = {
    session: { player: {} },
    async getBasicInfo() {
      return {
        chooseFormat(options) {
          if (options.quality === "360p") {
            return {
              itag: 18,
              has_audio: true,
              has_video: true,
              decipher: async () => "stream-360",
            };
          }
          throw new Error("no HD");
        },
      };
    },
  };
  await assert.rejects(
    resolveVideo(yt, "jNQXAC9IVRw", async () => true, "1080p"),
    /video_unavailable/,
  );
});

test("slow validators with shared deadline: baseline playback continues and inventory is bounded", async () => {
  const yt = {
    session: { player: {} },
    async getBasicInfo(_, { client }) {
      return {
        chooseFormat(options) {
          if (client === "ANDROID") {
            if (options.quality === "360p" && options.type === "video+audio") {
              return {
                itag: 18,
                has_audio: true,
                has_video: true,
                decipher: async () => "android-360",
              };
            }
            throw new Error("no android hd");
          }
          if (options.type === "video" && options.quality === "1080p") {
            return {
              itag: 137,
              has_audio: false,
              has_video: true,
              decipher: async () => "ios-1080-slow",
            };
          }
          if (options.type === "video" && options.quality === "720p") {
            return {
              itag: 136,
              has_audio: false,
              has_video: true,
              decipher: async () => "ios-720-slow",
            };
          }
          if (options.type === "audio") {
            return {
              itag: 140,
              has_audio: true,
              has_video: false,
              decipher: async () => "ios-aac-audio",
            };
          }
          throw new Error("no format");
        },
      };
    },
  };

  // Validator that simulates slow range requests (35ms delay per check)
  const slowValidate = async (url) => {
    await new Promise((r) => setTimeout(r, 35));
    return true;
  };

  // Set a tight deadline of 50ms: baseline (360p) will resolve quickly,
  // but variant checks will exceed deadline and cleanly bound inventory
  const result = await resolveVideo(yt, "dQw4w9WgXcQ", slowValidate, "", { deadlineMs: 50 });
  assert.equal(result.url, "android-360");
  assert.equal(result.mimeType, "video/mp4");
  // Baseline playback continues, and variants inventory is bounded (omits tiers that couldn't complete)
  assert.ok(result.variants === undefined || !result.variants.includes("1080p"));
});

test("validations are deduplicated for the same signed URL", async () => {
  const validatedKeys = [];
  const yt = {
    session: { player: {} },
    async getBasicInfo(_, { client }) {
      return {
        chooseFormat(options) {
          if (client === "ANDROID") {
            if (options.quality === "360p" && options.type === "video+audio") {
              return {
                itag: 18,
                has_audio: true,
                has_video: true,
                decipher: async () => "https://r.googlevideo.com/stream-360",
              };
            }
            throw new Error("no android hd");
          }
          if (options.type === "video" && options.quality === "720p") {
            return {
              itag: 136,
              has_audio: false,
              has_video: true,
              decipher: async () => "https://r.googlevideo.com/stream-720",
            };
          }
          if (options.type === "audio") {
            return {
              itag: 140,
              has_audio: true,
              has_video: false,
              decipher: async () => "https://r.googlevideo.com/stream-audio",
            };
          }
          throw new Error("no format");
        },
      };
    },
  };

  const dedupeValidate = async (url, format) => {
    validatedKeys.push(format?.itag ?? url);
    return true;
  };

  const result = await resolveVideo(yt, "dQw4w9WgXcQ", dedupeValidate);
  assert.equal(result.url, "https://r.googlevideo.com/stream-360");
  // The same URL was checked during resolution and inventory; validate once.
  const count18 = validatedKeys.filter((k) => k === 18).length;
  assert.equal(count18, 1);
});

test("same itag from another client gets its own range validation", async () => {
  const checked = [];
  const yt = {
    session: { player: {} },
    async getBasicInfo(_, { client }) {
      return {
        chooseFormat(options) {
          if (
            options.type === "video+audio" &&
            options.quality === "360p" &&
            client === "ANDROID"
          ) {
            return {
              itag: 18,
              has_audio: true,
              has_video: true,
              decipher: async () => "android-360",
            };
          }
          if (options.type === "video" && options.quality === "720p") {
            return {
              itag: 136,
              has_audio: false,
              has_video: true,
              decipher: async () => `${client}-720`,
            };
          }
          if (options.type === "audio") {
            return {
              itag: 140,
              has_audio: true,
              has_video: false,
              decipher: async () => `${client}-aac`,
            };
          }
          throw new Error("unavailable");
        },
      };
    },
  };
  const result = await resolveVideo(yt, "dQw4w9WgXcQ", async (url) => {
    checked.push(url);
    return !url.startsWith("ANDROID-720");
  });
  assert.equal(result.url, "android-360");
  assert.ok(checked.includes("ANDROID-720"));
  assert.ok(checked.includes("IOS-720"));
  assert.ok(result.variants.includes("720p"));
});
