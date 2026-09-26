import { resolveFormats, getAvailableVariants } from "./formats.mjs";
import { validateMediaRanges } from "./media-ranges.mjs";

const TIERS = ["1080p", "720p", "480p", "360p"];
const DEFAULT_VALIDATION_DEADLINE_MS = 13_500;
const DEFAULT_MANUAL_CLIENT_BUDGET_MS = 4_000;
const MAX_VALIDATION_CHECKS = 25;

// The anonymous WEB client can return format shells without decipherable URLs.
// Try clients with playable MP4 origins, while preserving the existing format gate.
export async function resolveVideo(
  yt,
  id,
  validate = validateMediaRanges,
  targetQuality = "",
  options = {},
) {
  const deadlineMs = options.deadlineMs ?? DEFAULT_VALIDATION_DEADLINE_MS;
  const startTime = Date.now();
  const deadlineAt = startTime + deadlineMs;
  const manualQuality = Boolean(targetQuality && targetQuality !== "auto");
  const manualClientBudgetMs = Math.max(
    1,
    options.manualClientBudgetMs ?? DEFAULT_MANUAL_CLIENT_BUDGET_MS,
  );
  let lastError;
  let checked = 0;

  // Signed URLs can differ by client even for the same itag. Cache only the
  // exact URL and declared size used by the range validator.
  const validationCache = new Map();

  const boundValidate = async (url, format, clientDeadlineAt = deadlineAt) => {
    if (!url) return false;

    // Auto keeps its shared validation deadline. Manual selection also gives
    // each client a smaller window so one slow origin cannot starve the rest.
    const validationDeadlineAt = manualQuality
      ? Math.min(deadlineAt, clientDeadlineAt)
      : deadlineAt;
    const remainingMs = validationDeadlineAt - Date.now();
    if (remainingMs <= 0) {
      return false;
    }

    const key = `${url}\n${format?.content_length ?? ""}`;
    if (validationCache.has(key)) {
      return validationCache.get(key);
    }

    if (++checked > MAX_VALIDATION_CHECKS) {
      return false;
    }

    let promise;
    promise = (async () => {
      if (!manualQuality) {
        try {
          return Boolean(await validate(url, format));
        } catch {
          return false;
        }
      }

      // Range validation composes this signal with its existing 2.5s request
      // timeout, so exhausting a client budget cancels its active HTTP probe.
      const controller = new AbortController();
      const timeoutResult = {};
      let timer;
      const validation = Promise.resolve()
        .then(() => validate(url, format, undefined, controller.signal))
        .then(Boolean, () => false);
      const timeout = new Promise((resolve) => {
        timer = setTimeout(() => {
          controller.abort();
          resolve(timeoutResult);
        }, remainingMs);
      });
      try {
        const result = await Promise.race([validation, timeout]);
        if (result === timeoutResult) {
          if (validationCache.get(key) === promise) validationCache.delete(key);
          return false;
        }
        return Boolean(result);
      } finally {
        clearTimeout(timer);
      }
    })();

    validationCache.set(key, promise);

    return promise;
  };

  const discoveredVariants = new Set();
  let resolvedResult = null;
  // Auto keeps Android first for its fast progressive baseline. Explicit HD
  // selection prioritizes iOS/VisionOS, which commonly expose adaptive H.264/AAC.
  const clients =
    manualQuality && targetQuality !== "360p"
      ? ["IOS", "VISIONOS", "ANDROID", "WEB"]
      : ["ANDROID", "IOS", "VISIONOS", "WEB"];

  for (const client of clients) {
    if (manualQuality && Date.now() >= deadlineAt) break;
    if (resolvedResult && Date.now() - startTime >= deadlineMs) {
      break;
    }

    const clientDeadlineAt = manualQuality
      ? Math.min(deadlineAt, Date.now() + manualClientBudgetMs)
      : deadlineAt;

    let info;
    try {
      info = await yt.getBasicInfo(id, { client });
    } catch (error) {
      lastError = error;
      continue;
    }

    if (!resolvedResult) {
      try {
        resolvedResult = await resolveFormats(
          info,
          yt.session.player,
          manualQuality
            ? (url, format) => boundValidate(url, format, clientDeadlineAt)
            : boundValidate,
          targetQuality,
        );
      } catch (error) {
        if (
          !lastError ||
          error.message === "audio_unavailable" ||
          lastError.message !== "audio_unavailable"
        ) {
          lastError = error;
        }
      }
    }

    // A manual selection needs only its exact stream. Rechecking the menu on
    // every client can consume the shared deadline before later clients are
    // tried, even when one of them has a playable rendition.
    if (!manualQuality && Date.now() - startTime < deadlineMs) {
      const remainingTiers = TIERS.filter((t) => !discoveredVariants.has(t));
      if (remainingTiers.length > 0) {
        try {
          const variants = await getAvailableVariants(
            info,
            yt.session.player,
            boundValidate,
            remainingTiers,
          );
          for (const v of variants) {
            discoveredVariants.add(v);
          }
        } catch {}
      }
    }

    if (resolvedResult) {
      if (manualQuality) break;
      // If we already discovered HD variants (both 1080p and 720p), or completed mobile clients, stop.
      const hasHD = discoveredVariants.has("1080p") && discoveredVariants.has("720p");
      if (
        hasHD ||
        client === "VISIONOS" ||
        client === "WEB" ||
        Date.now() - startTime >= deadlineMs
      ) {
        break;
      }
    }
  }

  if (!resolvedResult) {
    throw lastError ?? new Error("video_unavailable");
  }

  if (manualQuality) {
    return { ...resolvedResult, quality: targetQuality };
  }

  const variants = TIERS.filter((t) => discoveredVariants.has(t));
  if (variants.length > 0) {
    return { ...resolvedResult, variants };
  }
  return resolvedResult;
}
