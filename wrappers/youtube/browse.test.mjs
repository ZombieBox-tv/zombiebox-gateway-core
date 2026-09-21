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
