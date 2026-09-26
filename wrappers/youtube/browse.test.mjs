import test from "node:test";
import assert from "node:assert/strict";
import { browse, validParent } from "./browse.mjs";

test("channel and playlist parents are finite identifiers", () => {
  assert.equal(validParent("channel:UC" + "a".repeat(22)), true);
  assert.equal(validParent("playlist:PL" + "a".repeat(20)), true);
  for (const value of [
    "http://internal/",
    "channel:../../secret",
    "playlist:short",
    "playlist:abcdefghijk:extra",
  ])
    assert.equal(validParent(value), false);
});
test("large upstream pages retain items beyond the first forty", async () => {
  const videos = Array.from({ length: 100 }, (_, i) => ({
    id: String(i).padStart(11, "0"),
    title: `Video ${i}`,
  }));
  const yt = { getPlaylist: async () => ({ videos, has_continuation: false }) };
  const second = await browse(yt, "playlist:PLabcdefghijk", "", 40);
  assert.equal(second.items.length, 40);
  assert.equal(second.items[0].title, "Video 40");
  assert.equal(second.nextOffset, 80);
  const last = await browse(yt, "playlist:PLabcdefghijk", "", 80);
  assert.equal(last.items.length, 20);
  assert.equal(last.nextOffset, -1);
});

test("empty search pages the provider Home feed with stable offsets and cross-page dedupe", async () => {
  let homeRequests = 0;
  const page = (index) => {
    const ranges = [
      Array.from({ length: 40 }, (_, i) => i),
      Array.from({ length: 40 }, (_, i) => i + 35),
      Array.from({ length: 40 }, (_, i) => i + 75),
    ];
    return {
      channels: index === 0 ? [{ id: "UC" + "c".repeat(22), title: "Channel result" }] : [],
      playlists: index === 0 ? [{ id: "PL" + "p".repeat(20), title: "Playlist result" }] : [],
      videos: ranges[index].map((id) => ({
        id: String(id).padStart(11, "0"),
        title: `Video ${id}`,
      })),
      has_continuation: index < ranges.length - 1,
      getContinuation: async () => page(index + 1),
    };
  };
  const yt = {
    getHomeFeed: async () => {
      homeRequests++;
      return page(0);
    },
    search: async () => {
      throw new Error("empty query must not call search");
    },
  };

  const first = await browse(yt, "", "", 0);
  assert.equal(first.items.length, 40);
  assert.ok(first.items.every((item) => item.kind === "video"));
  assert.equal(first.items[0].title, "Video 0");
  assert.equal(first.nextOffset, 40);

  const second = await browse(yt, "", "", first.nextOffset);
  assert.equal(second.items.length, 40);
  assert.equal(second.items[0].title, "Video 40");
  assert.equal(second.items.at(-1).title, "Video 79");
  assert.equal(second.nextOffset, 80);
  assert.equal(new Set(second.items.map((item) => item.id)).size, 40);
  assert.equal(homeRequests, 2);
});

test("an empty provider Home feed stays empty without a search or trending fallback", async () => {
  let homeRequests = 0;
  const yt = {
    getHomeFeed: async () => {
      homeRequests++;
      return {
        videos: [],
        has_continuation: false,
        getContinuation: async () => {
          throw new Error("an exhausted Home feed must not request another page");
        },
      };
    },
    search: async () => {
      throw new Error("empty Home must not silently become a search");
    },
    getTrending: async () => {
      throw new Error("empty Home must not silently become trending");
    },
  };

  const result = await browse(yt, "", "", 0);

  assert.deepEqual(result, { items: [], nextOffset: -1 });
  assert.equal(homeRequests, 1);
});
