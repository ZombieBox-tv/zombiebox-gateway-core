const MAX_MEDIA_BYTES = 32 * 1024 * 1024 * 1024;
const RANGE_SAMPLE_BYTES = 1024;
const MAX_REDIRECTS = 2;

function googleVideoURL(value) {
  try {
    const url = new URL(value);
    return (
      url.protocol === "https:" &&
      !url.username &&
      !url.password &&
      !url.port &&
      url.hostname.toLowerCase().endsWith(".googlevideo.com")
    );
  } catch {
    return false;
  }
}

async function readExactSample(response, expectedBytes) {
  if (!response.body) return false;
  const reader = response.body.getReader();
  let received = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) return received === expectedBytes;
      received += value.byteLength;
      if (received > expectedBytes) return false;
    }
  } finally {
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

async function sample(url, start, end, total, fetchImpl, abortSignal) {
  let current = url;
  for (let hop = 0; hop <= MAX_REDIRECTS; hop++) {
    if (!googleVideoURL(current)) return false;
    const timeoutSignal = AbortSignal.timeout(2500);
    const signal = abortSignal ? AbortSignal.any([timeoutSignal, abortSignal]) : timeoutSignal;
    const response = await fetchImpl(current, {
      headers: { Range: `bytes=${start}-${end}`, "Accept-Encoding": "identity" },
      redirect: "manual",
      signal,
    });
    try {
      if (response.status >= 300 && response.status < 400) {
        const location = response.headers.get("location");
        if (!location || hop === MAX_REDIRECTS) return false;
        current = new URL(location, current).toString();
        continue;
      }
      if (response.status !== 206) return 0;
      const match = /^bytes (\d+)-(\d+)\/(\d+)$/.exec(response.headers.get("content-range") ?? "");
      if (!match) return 0;
      const first = Number(match[1]);
      const last = Number(match[2]);
      const discovered = Number(match[3]);
      if (
        !Number.isSafeInteger(discovered) ||
        discovered <= 0 ||
        discovered > MAX_MEDIA_BYTES ||
        first !== start ||
        last < first ||
        last !== Math.min(end, discovered - 1) ||
        last >= discovered ||
        (total && discovered !== total) ||
        !(await readExactSample(response, last - first + 1))
      )
        return 0;
      return discovered;
    } finally {
      if (response.body && !response.body.locked) await response.body.cancel().catch(() => {});
    }
  }
  return false;
}

// A 206 for the first kilobyte alone is insufficient: some formats return 403
// for subsequent bytes and FFmpeg otherwise reports a truncated remux as success.
export async function validateMediaRanges(url, format, fetchImpl = fetch, abortSignal) {
  if (!googleVideoURL(url)) return false;
  const declaredValue = format.content_length;
  const urlValue = new URL(url).searchParams.get("clen");
  const declared = declaredValue == null ? 0 : Number(declaredValue);
  const fromURL = urlValue == null ? 0 : Number(urlValue);
  if (
    !Number.isSafeInteger(declared) ||
    !Number.isSafeInteger(fromURL) ||
    declared < 0 ||
    fromURL < 0 ||
    declared > MAX_MEDIA_BYTES ||
    fromURL > MAX_MEDIA_BYTES ||
    (declared > 0 && fromURL > 0 && fromURL !== declared)
  )
    return false;
  let total = declared || fromURL;
  try {
    // Some valid progressive formats omit both content_length and clen. Learn
    // the bounded size from the first finite 206, then verify middle and tail.
    total = await sample(
      url,
      0,
      total ? Math.min(total - 1, RANGE_SAMPLE_BYTES - 1) : RANGE_SAMPLE_BYTES - 1,
      total,
      fetchImpl,
      abortSignal,
    );
    if (!total) return false;
    const starts = [Math.floor(total / 2), Math.max(0, total - RANGE_SAMPLE_BYTES)];
    for (const start of new Set(starts)) {
      if (start === 0) continue;
      const end = Math.min(total - 1, start + RANGE_SAMPLE_BYTES - 1);
      if (!(await sample(url, start, end, total, fetchImpl, abortSignal))) return false;
    }
    return true;
  } catch {
    return false;
  }
}
