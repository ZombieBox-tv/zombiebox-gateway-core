import { parentPort, workerData } from "node:worker_threads";
import { Innertube, Platform, Log } from "youtubei.js";
import { resolveVideo } from "./resolve.mjs";
import { browse } from "./browse.mjs";
import { evaluate } from "./interpreter.mjs";

Platform.shim.eval = evaluate;
Log.setLevel(Log.Level.NONE);

async function run() {
  const { operation, query, id, quality, cookie, poToken, visitorData } = workerData;
  const yt = await Innertube.create({
    lang: "en",
    location: "US",
    generate_session_locally: true,
    retrieve_player: operation === "resolve",
    cookie,
    po_token: poToken,
    visitor_data: visitorData,
  });
  if (operation === "browse") return browse(yt, workerData.parent, query, workerData.offset);
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
  return resolveVideo(yt, id, undefined, quality);
}

try {
  parentPort.postMessage({ ok: true, value: await run() });
} catch {
  parentPort.postMessage({ ok: false });
}
