import test from "node:test";
import assert from "node:assert/strict";
import { resolveFormats } from "./formats.mjs";
const format = (url, video, audio) => ({
  has_video: video,
  has_audio: audio,
  decipher: async () => url,
});
test("combined stream wins without adaptive requests", async () => {
  const calls = [];
  const result = await resolveFormats(
    {
      chooseFormat(options) {
        calls.push(options.type);
        return format("combined", true, true);
      },
    },
    {},
  );
  assert.deepEqual(result, { url: "combined", mimeType: "video/mp4" });
  assert.deepEqual(calls, ["video+audio"]);
});
test("adaptive selection requires separate audio and bounds video resolution", async () => {
  const calls = [];
  const result = await resolveFormats(
    {
      chooseFormat(options) {
        calls.push(options);
        if (options.type === "audio") return format("audio", false, true);
        if (options.type === "video" && options.quality === "480p")
          return format("video", true, false);
        throw new Error("missing");
      },
    },
    {},
  );
  assert.equal(result.audioUrl, "audio");
  assert.equal(result.url, "video");
  assert.deepEqual(
    calls.filter((x) => x.type === "video").map((x) => x.quality),
    ["360p", "480p"],
  );
});
test("missing audio never yields a video-only plan", async () => {
  await assert.rejects(
    resolveFormats(
      {
        chooseFormat(options) {
          if (options.type === "video+audio") throw new Error("missing");
          return format("video", true, false);
        },
      },
      {},
    ),
    /audio_unavailable/,
  );
});
