import test from "node:test";
import assert from "node:assert/strict";
import { validateMediaRanges } from "./media-ranges.mjs";

const mediaURL = "https://r1.googlevideo.com/clip?clen=8192";
const matching = (_, options) => {
  const [, start, end] = options.headers.Range.match(/^bytes=(\d+)-(\d+)$/);
  return new Response(new Uint8Array(Number(end) - Number(start) + 1), {
    status: 206,
    headers: { "Content-Range": `bytes ${start}-${end}/8192` },
  });
};

test("validation samples the head, middle and tail with finite byte ranges", async () => {
  const ranges = [];
  const result = await validateMediaRanges(mediaURL, { content_length: 8192 }, (url, options) => {
    assert.equal(url, mediaURL);
    assert.equal(options.redirect, "manual");
    ranges.push(options.headers.Range);
    return matching(url, options);
  });
  assert.equal(result, true);
  assert.deepEqual(ranges, ["bytes=0-1023", "bytes=4096-5119", "bytes=7168-8191"]);
});

test("missing declared length is learned from a safe bounded 206 after redirect", async () => {
  const ranges = [];
  const discovered = await validateMediaRanges(
    "https://r1.googlevideo.com/clip",
    {},
    (url, options) => {
      ranges.push(options.headers.Range);
      if (new URL(url).hostname === "r1.googlevideo.com")
        return new Response(null, {
          status: 302,
          headers: { Location: "https://r2.googlevideo.com/clip" },
        });
      return matching(url, options);
    },
  );
  assert.equal(discovered, true);
  assert.deepEqual(ranges, [
    "bytes=0-1023",
    "bytes=0-1023",
    "bytes=4096-5119",
    "bytes=4096-5119",
    "bytes=7168-8191",
    "bytes=7168-8191",
  ]);
});

test("unknown length rejects missing, inconsistent or oversized Content-Range", async () => {
  const origin = "https://r1.googlevideo.com/clip";
  for (const header of [
    null,
    "bytes 0-1023/0",
    "bytes 1-1023/8192",
    "bytes 0-500/8192",
    "bytes 0-1023/34359738369",
  ]) {
    const response = (_, options) => {
      const headers = header ? { "Content-Range": header } : {};
      return new Response(new Uint8Array(1024), { status: 206, headers });
    };
    assert.equal(await validateMediaRanges(origin, {}, response), false, header ?? "missing");
  }
});

test("a short body with a valid range header rejects the format", async () => {
  assert.equal(
    await validateMediaRanges(mediaURL, { content_length: 8192 }, (_, options) => {
      const [, start, end] = options.headers.Range.match(/^bytes=(\d+)-(\d+)$/);
      const bytes = Number(end) - Number(start) + 1;
      return new Response(new Uint8Array(bytes - 1), {
        status: 206,
        headers: { "Content-Range": `bytes ${start}-${end}/8192` },
      });
    }),
    false,
  );
});

test("a 403 after a valid first chunk rejects the format", async () => {
  assert.equal(
    await validateMediaRanges(mediaURL, { content_length: 8192 }, (url, options) =>
      options.headers.Range === "bytes=4096-5119"
        ? new Response("denied", { status: 403 })
        : matching(url, options),
    ),
    false,
  );
});

test("an aborted validation signal cancels the active range request", async () => {
  const controller = new AbortController();
  let requestSignal;
  const pendingFetch = (_, options) => {
    requestSignal = options.signal;
    return new Promise((_, reject) => {
      requestSignal.addEventListener(
        "abort",
        () => reject(new DOMException("Aborted", "AbortError")),
        { once: true },
      );
    });
  };

  const validation = validateMediaRanges(
    mediaURL,
    { content_length: 8192 },
    pendingFetch,
    controller.signal,
  );
  controller.abort();

  assert.equal(await validation, false);
  assert.equal(requestSignal.aborted, true);
});

test("only bounded HTTPS Googlevideo redirects are followed", async () => {
  const seen = [];
  const follow = (url, options) => {
    seen.push(new URL(url).hostname);
    if (new URL(url).hostname === "r1.googlevideo.com")
      return new Response(null, {
        status: 302,
        headers: { Location: "https://r2.googlevideo.com/clip?clen=8192" },
      });
    return matching(url, options);
  };
  assert.equal(await validateMediaRanges(mediaURL, { content_length: 8192 }, follow), true);
  assert.deepEqual(seen, [
    "r1.googlevideo.com",
    "r2.googlevideo.com",
    "r1.googlevideo.com",
    "r2.googlevideo.com",
    "r1.googlevideo.com",
    "r2.googlevideo.com",
  ]);
  assert.equal(
    await validateMediaRanges(
      mediaURL,
      { content_length: 8192 },
      () => new Response(null, { status: 302, headers: { Location: "https://evil.invalid/clip" } }),
    ),
    false,
  );
  assert.equal(
    await validateMediaRanges(
      "https://r1.googlevideo.com.evil.invalid/clip?clen=8192",
      { content_length: 8192 },
      () => {
        throw new Error("unsafe origin was requested");
      },
    ),
    false,
  );
});
