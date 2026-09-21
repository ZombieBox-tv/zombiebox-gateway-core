const videoID = /^[\w-]{11}$/;
const channelID = /^UC[\w-]{22}$/;
const playlistID = /^[\w-]{10,100}$/;
export function validParent(parent) {
  if (!parent) return true;
  const [kind, id, extra] = parent.split(":");
  return (
    !extra &&
    ((kind === "channel" && channelID.test(id)) || (kind === "playlist" && playlistID.test(id)))
  );
}
export function browseItems(feed) {
  const items = [],
    seen = new Set();
  function add(kind, node) {
    if (
      !(kind === "video" ? videoID : kind === "channel" ? channelID : playlistID).test(node.id) ||
      (kind === "video" && node.is_live)
    )
      return;
    const key = `${kind}:${node.id}`;
    if (seen.has(key)) return;
    seen.add(key);
    items.push({
      id: node.id,
      kind,
      title: String(node.title ?? node.author?.name ?? "").slice(0, 500),
      subtitle: String(node.author?.name ?? "").slice(0, 200),
      durationMs: Math.max(0, Math.min(604800000, Number(node.duration?.seconds ?? 0) * 1000)),
    });
  }
  for (const node of (feed.channels ?? []).slice(0, 40)) add("channel", node);
  for (const node of (feed.playlists ?? []).slice(0, 40)) add("playlist", node);
  for (const node of (feed.videos ?? []).slice(0, 200)) add("video", node);
  return items;
}
export async function browse(yt, parent, query, offset) {
  if (!validParent(parent) || !Number.isInteger(offset) || offset < 0 || offset > 360)
    throw Error("Invalid browse request");
  let feed;
  if (parent.startsWith("channel:"))
    feed = await (await yt.getChannel(parent.slice(8))).getVideos();
  else if (parent.startsWith("playlist:")) feed = await yt.getPlaylist(parent.slice(9));
  else feed = query ? await yt.search(query) : await yt.getHomeFeed();
  const items = [],
    seen = new Set();
  for (let pages = 0; pages < 10; pages++) {
    for (const item of browseItems(feed)) {
      const key = `${item.kind}:${item.id}`;
      if (!seen.has(key)) {
        seen.add(key);
        items.push(item);
      }
    }
    if (items.length > offset + 40 || !feed.has_continuation) break;
    feed = await feed.getContinuation();
  }
  return {
    items: items.slice(offset, offset + 40),
    nextOffset: offset < 360 && items.length > offset + 40 ? offset + 40 : -1,
  };
}
