import { parentPort, workerData } from "node:worker_threads";
import { Innertube, Platform, Log } from "youtubei.js";
import { evaluate } from "./interpreter.mjs";

Platform.shim.eval = evaluate;
Log.setLevel(Log.Level.NONE);

async function run() {
  const { operation, query, id, cookie, poToken, visitorData } = workerData;
  const yt = await Innertube.create({
    lang: "en",
    location: "US",
    generate_session_locally: true,
    retrieve_player: operation === "resolve",
    cookie,
    po_token: poToken,
    visitor_data: visitorData,
  });
  if (operation === "catalog") {
    const feed = query ? await yt.search(query, { type: "video" }) : await yt.getHomeFeed();
    const items = [];
    const seen = new Set();
    for (const video of feed.videos ?? []) {
      if (!/^[\w-]{11}$/.test(video.id) || seen.has(video.id) || video.is_live) continue;
      seen.add(video.id);
      items.push({
        id: video.id,
        title: String(video.title ?? "").slice(0, 500),
        subtitle: String(video.author?.name ?? "").slice(0, 200),
        durationMs: Math.max(0, Math.min(604800000, Number(video.duration?.seconds ?? 0) * 1000)),
      });
      if (items.length === 40) break;
    }
    return { items };
  }
  // A combined progressive format avoids pretending adaptive video-only is playable.
  const info = await yt.getBasicInfo(id);
  const format = info.chooseFormat({ type: "video+audio", format: "mp4", quality: "360p" });
  if (!format.has_audio || !format.has_video) throw new Error("combined_format_unavailable");
  const url = await format.decipher(yt.session.player);
  if (!url) throw new Error("stream_unavailable");
  return { url, mimeType: "video/mp4" };
}

try {
  parentPort.postMessage({ ok: true, value: await run() });
} catch {
  parentPort.postMessage({ ok: false });
}
