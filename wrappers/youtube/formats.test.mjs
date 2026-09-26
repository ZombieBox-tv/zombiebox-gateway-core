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

test("auto prefers a validated combined 720p after the 360p check fails", async () => {
  const selected = [];
  const result = await resolveFormats(
    {
      chooseFormat(options) {
        if (options.type === "video+audio" && options.quality === "360p")
          return format("baseline-360", true, true);
        if (options.type === "video+audio" && options.quality === "720p")
          return format("progressive-720", true, true);
        if (options.type === "video" && options.quality === "1080p")
          return format("adaptive-1080", true, false);
        if (options.type === "audio") return format("aac", false, true);
        throw new Error("missing");
      },
    },
    {},
    async (url) => {
      selected.push(url);
      return url !== "baseline-360";
    },
  );

  assert.deepEqual(result, { url: "progressive-720", mimeType: "video/mp4" });
  assert.deepEqual(selected, ["baseline-360", "progressive-720"]);
});

test("auto tries lower adaptive tiers before 1080p and validates paired audio", async () => {
  const selected = [];
  const result = await resolveFormats(
    {
      chooseFormat(options) {
        if (options.type === "video+audio")
          return format(`progressive-${options.quality}`, true, true);
        if (options.type === "video" && options.quality === "720p")
          return format("adaptive-720", true, false);
        if (options.type === "video" && options.quality === "1080p")
          return format("adaptive-1080", true, false);
        if (options.type === "audio") return format("aac", false, true);
        throw new Error("missing");
      },
    },
    {},
    async (url) => {
      selected.push(url);
      return url === "adaptive-720" || url === "aac";
    },
  );

  assert.deepEqual(result, { url: "adaptive-720", audioUrl: "aac", mimeType: "video/mp4" });
  assert.deepEqual(selected, [
    "progressive-360p",
    "progressive-720p",
    "progressive-480p",
    "adaptive-720",
    "aac",
  ]);
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
    ["720p", "480p"],
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
  assert.deepEqual(selected, ["good-720"]);
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
      if (options.type === "video" && options.quality === "1080p")
        return format("1080-video", true, false);
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

  const res1080 = await resolveFormats(info, {}, async () => true, "1080p");
  assert.deepEqual(res1080, {
    url: "1080-video",
    audioUrl: "aac-audio",
    mimeType: "video/mp4",
  });
});

test("tier checks overlap within the bound of at most two simultaneous checks", async () => {
  let activeChecks = 0;
  let maxActiveChecks = 0;
  const started = [];

  let releaseFirstBatch;
  const firstBatchWait = new Promise((resolve) => {
    releaseFirstBatch = resolve;
  });

  const info = {
    chooseFormat(options) {
      if (options.type === "video+audio") {
        return {
          itag:
            options.quality === "1080p"
              ? 37
              : options.quality === "720p"
                ? 22
                : options.quality === "480p"
                  ? 18
                  : 36,
          has_video: true,
          has_audio: true,
          decipher: async () => `https://r.googlevideo.com/${options.quality}`,
        };
      }
      throw new Error("no format");
    },
  };

  const validate = async (url) => {
    activeChecks++;
    maxActiveChecks = Math.max(maxActiveChecks, activeChecks);
    started.push(url);

    if (activeChecks === 2 && started.length === 2) {
      releaseFirstBatch();
    } else if (started.length <= 2) {
      await firstBatchWait;
    }

    activeChecks--;
    return true;
  };

  const variants = await getAvailableVariants(info, {}, validate);

  assert.equal(maxActiveChecks, 2, "tier checks overlap and reach exactly 2 simultaneous checks");
  assert.deepEqual(variants, ["1080p", "720p", "480p", "360p"]);
});

test("output remains ordered [1080p, 720p, 480p, 360p] even when lower tiers complete first", async () => {
  let release1080;
  const p1080 = new Promise((resolve) => {
    release1080 = resolve;
  });
  const completionOrder = [];

  const info = {
    chooseFormat(options) {
      return {
        itag:
          options.quality === "1080p"
            ? 37
            : options.quality === "720p"
              ? 22
              : options.quality === "480p"
                ? 18
                : 36,
        has_video: true,
        has_audio: true,
        decipher: async () => options.quality,
      };
    },
  };

  const validate = async (url) => {
    if (url === "1080p") {
      await p1080;
    }
    completionOrder.push(url);
    if (completionOrder.length === 3) {
      release1080();
    }
    return true;
  };

  const variants = await getAvailableVariants(info, {}, validate);

  assert.deepEqual(completionOrder, ["720p", "480p", "360p", "1080p"]);
  assert.deepEqual(variants, ["1080p", "720p", "480p", "360p"]);
});

test("a bad high tier cannot suppress a valid lower tier", async () => {
  const info = {
    chooseFormat(options) {
      if (options.type === "audio") {
        return {
          itag: 140,
          has_audio: true,
          has_video: false,
          decipher: async () => "https://r.googlevideo.com/aac",
        };
      }
      if (options.quality === "1080p") {
        return {
          itag: 137,
          has_video: true,
          has_audio: false,
          decipher: async () => {
            throw new Error("upstream_decipher_failure");
          },
        };
      }
      if (options.quality === "720p") {
        return {
          itag: 136,
          has_video: true,
          has_audio: false,
          decipher: async () => "https://r.googlevideo.com/720p",
        };
      }
      if (options.quality === "480p") {
        throw new Error("corrupted_format_manifest");
      }
      if (options.quality === "360p") {
        return {
          itag: 18,
          has_video: true,
          has_audio: true,
          decipher: async () => "https://r.googlevideo.com/360p",
        };
      }
      throw new Error("missing");
    },
  };

  const variants = await getAvailableVariants(info, {}, async () => true);
  assert.deepEqual(variants, ["720p", "360p"]);
});

test("shared validated AAC work is performed only once across adaptive tiers", async () => {
  let audioChecks = 0;
  const info = {
    chooseFormat(options) {
      if (options.type === "audio") {
        return {
          itag: 140,
          has_audio: true,
          has_video: false,
          decipher: async () => "https://r.googlevideo.com/aac-shared",
        };
      }
      if (options.type === "video") {
        return {
          itag: options.quality === "1080p" ? 137 : options.quality === "720p" ? 136 : 135,
          has_video: true,
          has_audio: false,
          decipher: async () => `https://r.googlevideo.com/video-${options.quality}`,
        };
      }
      throw new Error("no combined");
    },
  };

  const validate = async (url) => {
    if (url.includes("aac")) {
      audioChecks++;
    }
    return true;
  };

  const variants = await getAvailableVariants(info, {}, validate);
  assert.equal(audioChecks, 1, "AAC audio was only validated once across all adaptive tiers");
  assert.deepEqual(variants, ["1080p", "720p", "480p", "360p"]);
});

test("getAvailableVariants rejects high framerate >30fps HD tiers", async () => {
  const info = {
    chooseFormat(options) {
      if (options.type === "audio") return format("https://r.googlevideo.com/aac", false, true);
      if (options.type === "video" && options.quality === "1080p") {
        const f = format("https://r.googlevideo.com/1080p60", true, false);
        f.fps = 60;
        f.quality_label = "1080p60";
        return f;
      }
      if (options.type === "video" && options.quality === "720p") {
        const f = format("https://r.googlevideo.com/720p50", true, false);
        f.fps = 50;
        f.quality_label = "720p50";
        return f;
      }
      if (options.type === "video" && options.quality === "480p") {
        const f = format("https://r.googlevideo.com/480p", true, false);
        f.fps = 30;
        f.quality_label = "480p";
        return f;
      }
      if (options.type === "video+audio" && options.quality === "360p") {
        const f = format("https://r.googlevideo.com/360p", true, true);
        f.fps = 30;
        f.quality_label = "360p";
        return f;
      }
      throw new Error("missing format");
    },
  };
  const variants = await getAvailableVariants(info, {}, async () => true);
  assert.deepEqual(variants, ["480p", "360p"]);
});

test("resolveFormats with targetQuality rejects high framerate >30fps tier", async () => {
  const info = {
    chooseFormat(options) {
      if (options.type === "audio") return format("https://r.googlevideo.com/aac", false, true);
      if (options.type === "video" && options.quality === "1080p") {
        const f = format("https://r.googlevideo.com/1080p60", true, false);
        f.fps = 60;
        f.quality_label = "1080p60";
        return f;
      }
      throw new Error("missing format");
    },
  };
  await assert.rejects(
    resolveFormats(info, {}, async () => true, "1080p"),
    /video_unavailable/,
  );
});

test("getAvailableVariants honors custom tiersToCheck subset", async () => {
  const checked = [];
  const info = {
    chooseFormat(options) {
      checked.push(`${options.type}:${options.quality}`);
      if (options.type === "audio") return format("aac", false, true);
      if (options.type === "video" && options.quality === "720p")
        return format("720p-video", true, false);
      throw new Error("missing");
    },
  };
  const variants = await getAvailableVariants(info, {}, async () => true, ["720p"]);
  assert.deepEqual(variants, ["720p"]);
  assert.ok(checked.some((c) => c.includes("720p")));
  assert.ok(!checked.some((c) => c.includes("1080p")));
  assert.ok(!checked.some((c) => c.includes("360p")));
});

test("getAvailableVariants skips candidate labels early when audio validation fails", async () => {
  let videoChecks = 0;
  const info = {
    chooseFormat(options) {
      if (options.type === "audio") return format("bad-audio", false, true);
      if (options.type === "video") return format(`video-${options.quality}`, true, false);
      throw new Error("no combined");
    },
  };
  const validate = async (url) => {
    if (url.includes("video")) videoChecks++;
    return false; // audio fails
  };
  const variants = await getAvailableVariants(info, {}, validate);
  assert.deepEqual(variants, []);
  assert.equal(videoChecks, 0, "no video validation performed when audio is invalid");
});
