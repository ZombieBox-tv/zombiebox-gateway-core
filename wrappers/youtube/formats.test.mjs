import test from "node:test";
import assert from "node:assert/strict";
import { resolveFormats, getAvailableVariants } from "./formats.mjs";
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
        return format("combined", true, true);
      },
    },
    {},
  );
  assert.deepEqual(result, { url: "combined", mimeType: "video/mp4" });
  assert.deepEqual(calls, ["video+audio:360p"]);
});

test("auto keeps valid 360p progressive baseline when HD adaptive exists", async () => {
  const calls = [];
  const result = await resolveFormats(
    {
      chooseFormat(options) {
        calls.push(`${options.type}:${options.quality}`);
        if (options.type === "video" && options.quality === "1080p")
          return format("high-video", true, false);
        if (options.type === "audio") return format("aac", false, true);
        if (options.type === "video+audio" && options.quality === "360p")
          return format("baseline-360", true, true);
        throw new Error("missing");
      },
    },
    {},
  );
  assert.deepEqual(result, { url: "baseline-360", mimeType: "video/mp4" });
  assert.deepEqual(calls, ["video+audio:360p"]);
});

test("when baseline 360p is unavailable, auto falls back to validated adaptive HD", async () => {
  const calls = [];
  const result = await resolveFormats(
    {
      chooseFormat(options) {
        calls.push(`${options.type}:${options.quality}`);
        if (options.type === "video+audio" && options.quality === "360p")
          throw new Error("baseline missing");
        if (options.type === "video" && options.quality === "1080p")
          return format("high-video", true, false);
        if (options.type === "audio") return format("aac", false, true);
        throw new Error("missing");
      },
    },
    {},
  );
  assert.deepEqual(result, { url: "high-video", audioUrl: "aac", mimeType: "video/mp4" });
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

test("getAvailableVariants lists only tiers with video and available audio", async () => {
  const infoWithHD = {
    chooseFormat(options) {
      if (options.type === "audio") return format("aac", false, true);
      if (options.type === "video" && (options.quality === "1080p" || options.quality === "720p")) {
        return format("hd-video", true, false);
      }
      if (options.type === "video+audio" && options.quality === "360p") {
        return format("combined-360", true, true);
      }
      throw new Error("missing");
    },
  };
  assert.deepEqual(await getAvailableVariants(infoWithHD), ["1080p", "720p", "360p"]);

  const infoMissingAudio = {
    chooseFormat(options) {
      if (options.type === "audio") throw new Error("no audio");
      if (options.type === "video" && options.quality === "1080p")
        return format("video", true, false);
      if (options.type === "video+audio" && options.quality === "360p")
        return format("combined", true, true);
      throw new Error("missing");
    },
  };
  assert.deepEqual(await getAvailableVariants(infoMissingAudio), ["360p"]);

  const infoSDOnly = {
    chooseFormat(options) {
      if (options.type === "audio") return format("aac", false, true);
      if (options.type === "video+audio" && options.quality === "360p")
        return format("combined", true, true);
      throw new Error("missing");
    },
  };
  assert.deepEqual(await getAvailableVariants(infoSDOnly), ["360p"]);
});

test("getAvailableVariants omits tiers whose video or audio URLs fail validation", async () => {
  const info = {
    chooseFormat(options) {
      if (options.type === "audio") return format("https://r.googlevideo.com/audio", false, true);
      if (options.type === "video" && options.quality === "1080p")
        return format("https://r.googlevideo.com/video-1080-fail", true, false);
      if (options.type === "video" && options.quality === "720p")
        return format("https://r.googlevideo.com/video-720-ok", true, false);
      if (options.type === "video+audio" && options.quality === "360p")
        return format("https://r.googlevideo.com/combined-360", true, true);
      throw new Error("missing");
    },
  };

  const validate = async (url) => !url.includes("fail");
  const result = await getAvailableVariants(info, {}, validate);
  assert.deepEqual(result, ["720p", "360p"]);

  // If audio fails validation, adaptive 720p must also be omitted:
  const validateAudioFail = async (url) => !url.includes("audio") && !url.includes("fail");
  const resultAudioFail = await getAvailableVariants(info, {}, validateAudioFail);
  assert.deepEqual(resultAudioFail, ["360p"]);
});

test("resolveFormats with targetQuality returns requested tier or errors", async () => {
  const info = {
    chooseFormat(options) {
      if (options.type === "audio") return format("aac-audio", false, true);
      if (options.type === "video" && options.quality === "720p")
        return format("720-video", true, false);
      if (options.type === "video+audio" && options.quality === "360p")
        return format("360-combined", true, true);
      throw new Error("missing");
    },
  };

  const res720 = await resolveFormats(info, {}, async () => true, "720p");
  assert.deepEqual(res720, { url: "720-video", audioUrl: "aac-audio", mimeType: "video/mp4" });

  const res360 = await resolveFormats(info, {}, async () => true, "360p");
  assert.deepEqual(res360, { url: "360-combined", mimeType: "video/mp4" });

  await assert.rejects(
    resolveFormats(info, {}, async () => true, "1080p"),
    /video_unavailable/,
  );
});
