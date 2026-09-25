import test from "node:test";
import assert from "node:assert/strict";
import { resolveFormats } from "./formats.mjs";
const format = (url, video, audio) => ({
  has_video: video,
  has_audio: audio,
  decipher: async () => url,
});
test("combined stream wins over adaptive at the same resolution", async () => {
  const calls = [];
  const result = await resolveFormats(
    {
      chooseFormat(options) {
        calls.push(`${options.type}:${options.quality}`);
        if (options.quality === "1080p") throw new Error("missing");
        return format("combined", true, true);
      },
    },
    {},
  );
  assert.deepEqual(result, { url: "combined", mimeType: "video/mp4" });
  assert.deepEqual(calls, ["video+audio:360p", "video:1080p", "video+audio:720p"]);
});
test("validated 1080p adaptive source beats a lower combined stream", async () => {
  const calls = [];
  const result = await resolveFormats(
    {
      chooseFormat(options) {
        calls.push(`${options.type}:${options.quality}`);
        if (options.type === "video" && options.quality === "1080p")
          return format("high-video", true, false);
        if (options.type === "audio") return format("aac", false, true);
        if (options.type === "video+audio") return format("low-combined", true, true);
        throw new Error("missing");
      },
    },
    {},
  );
  assert.deepEqual(result, { url: "high-video", audioUrl: "aac", mimeType: "video/mp4" });
  assert.deepEqual(calls, ["video+audio:360p", "video:1080p", "audio:bestefficiency"]);
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
    ["1080p", "720p", "480p"],
  );
});
test("expired high source falls back to validated 720p combined", async () => {
  const selected = [];
  const result = await resolveFormats(
    {
      chooseFormat(options) {
        if (options.type === "video" && options.quality === "1080p")
          return format("expired-high", true, false);
        if (options.type === "video+audio" && options.quality === "720p")
          return format("good-720", true, true);
        throw new Error("missing");
      },
    },
    {},
    async (url) => {
      selected.push(url);
      return url === "good-720";
    },
  );
  assert.deepEqual(result, { url: "good-720", mimeType: "video/mp4" });
  assert.deepEqual(selected, ["expired-high", "good-720"]);
});
test("validated 360p baseline survives unavailable higher renditions", async () => {
  const result = await resolveFormats(
    {
      chooseFormat(options) {
        if (options.type === "video+audio" && options.quality === "360p")
          return format("baseline-360", true, true);
        if (options.type === "video+audio") return format(`expired-${options.quality}`, true, true);
        throw new Error("no adaptive source");
      },
    },
    {},
    async (url) => url === "baseline-360",
  );
  assert.deepEqual(result, { url: "baseline-360", mimeType: "video/mp4" });
});
test("combined stream remains playable when separate AAC is missing", async () => {
  const result = await resolveFormats(
    {
      chooseFormat(options) {
        if (options.type === "video" && options.quality === "1080p")
          return format("high-video", true, false);
        if (options.type === "audio") throw new Error("missing AAC");
        if (options.type === "video+audio" && options.quality === "720p")
          return format("combined-720", true, true);
        throw new Error("missing");
      },
    },
    {},
  );
  assert.deepEqual(result, { url: "combined-720", mimeType: "video/mp4" });
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
test("failed combined and low-rate audio try another bounded format", async () => {
  const selected = [];
  const candidates = {
    combined: { ...format("combined", true, true), itag: 18 },
    low: { ...format("low-audio", false, true), itag: 139 },
    high: { ...format("high-audio", false, true), itag: 140 },
    video: { ...format("video", true, false), itag: 134 },
  };
  const result = await resolveFormats(
    {
      chooseFormat(options) {
        if (options.type === "video+audio") return candidates.combined;
        if (options.type === "audio")
          return options.quality === "best" ? candidates.high : candidates.low;
        if (options.type === "video" && options.quality === "1080p") throw new Error("missing");
        if (options.type === "video") return candidates.video;
      },
    },
    {},
    async (url) => {
      selected.push(url);
      return url !== "combined" && url !== "low-audio";
    },
  );
  assert.deepEqual(result, { url: "video", audioUrl: "high-audio", mimeType: "video/mp4" });
  assert.deepEqual(selected, ["combined", "video", "low-audio", "high-audio"]);
});
