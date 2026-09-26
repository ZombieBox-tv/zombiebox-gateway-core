# Hybrid Playback Architecture & Safeguards

## 1. Overview and Context

### Target Hardware and Player Constraints
- **Platform**: Legacy Google TV (API 13, Marvell Armada 1500 / Berlin video processor, Vizio Co-Star VAP430).
- **Decoders**: Hardware H.264 video decoder (accepts Baseline, Main, and High profiles up to 720p/1080p) and hardware AAC audio decoder. Raw AC3/E-AC3 audio is rejected over HTTP.
- **Player Engine**: `AVAPIMediaPlayer` requires HTTP delivery with a definitive `Content-Length` header upfront and rejects HTTP chunked transfer encoding (`Transfer-Encoding: chunked`).
- **Mode Description**: `HYBRID` mode provides progressive fragmented MP4 delivery by copying compatible video packets (`-c:v copy`) and transcoding audio to AAC (`-c:a aac`) in a single pass. Because exact byte counts and seekable byte-ranges are required by legacy players, the converted stream is spooled to local disk.
- **Physical Verification Baseline**: Exact `Content-Length` and `Range` response behaviors were physically verified with a 20-second H.264+AC3 fixture on the physical Vizio API 13 device. Full movies remain unverified under the synchronous GET flow due to the 25-second preparation limit.

---

## 2. Spool Lifecycle and Safeguards

### Private Root & Instance Namespace
- **Base Directory Validation**: Configured base directories (`Options.HybridSpoolDir`) are never modified with `chmod`. The `zombied` executable defaults to `hybrid-spools` beside its state database; Full explicitly uses `/data/hybrid-spools` on the writable state volume. Direct `server.New` callers without a spool directory retain the `/tmp/zombiebox-spools` fallback. Existing directories are validated for current user ownership and restrictive permissions without mutation.
- **Private Root Creation**: If the configured path is shared or world-writable (e.g. `/tmp`), the manager creates and uses a private subdirectory (`zombiebox-spools-<uid>`) with `0700` permissions.
- **Symlink & Unsafe Path Rejection**: Base paths and instance paths that are symlinks or escape safe directory bounds are rejected.
- **Fail-Closed Guarantee**: If base directory preparation, instance directory creation, or lock acquisition fails, the gateway fails closed with an explicit stream error (`500 spool_storage_unavailable`). Spools are never created in unowned or unlocked directories, and the manager never silently falls back to `os.TempDir()`.
- **Instance Isolation**: Each gateway instance creates a private directory (`instance-<pid>-<random>`) under its validated private root. Spools are never placed in a shared global directory.

### Advisory File Lock Crash Cleanup
- Each active gateway instance holds an exclusive, non-blocking advisory file lock (`syscall.Flock` with `LOCK_EX`) on `.lock` within its private instance directory.
- **Precise Cleanup Guarantee**: An orphaned instance directory is removed if and only if:
  1. The entry is a real directory (not a symlink) owned by the current process UID.
  2. It contains a real regular file `.lock` (not a symlink) owned by the current process UID.
  3. The lock file can be opened safely without following symlinks (`O_NOFOLLOW`).
  4. An exclusive non-blocking `syscall.Flock` can be successfully acquired.
- **Active Instance Preservation**: Active sibling instances maintain their open lock descriptors; `flock` fails with `EWOULDBLOCK`, and active directories are left completely untouched.
- **No Time-Based Heuristic on Missing Locks**: A directory is **never** removed merely because `.lock` is missing or unreadable after 30 seconds. Incomplete or unreadable directories are preserved to prevent race conditions during instance startup and protect external filesystem entries.

### Bounded Aggregate Quota and Dynamic Accounting
- **Per-Spool Limit**: Bounded to 2 GiB (`HybridMaxSpoolBytes`, default `2 << 30`). A writer limit guard (`boundedSpoolWriter`) terminates conversions exceeding this budget with HTTP 507 (`spool_limit_exceeded`).
- **Full storage**: Its 16 MiB `/tmp` tmpfs is too small for this limit. Full's `/data` spool survives container recreation and is reclaimed by session cleanup, least recently used eviction, or locked crash cleanup on startup. The quota remains per gateway instance; operators must still provide adequate free disk space.
- **Aggregate Quota Scope**: Bounded to 4 GiB (`HybridAggregateQuotaBytes`, default `4 << 30`).
  - *Per-Instance Scope*: Total quota is tracked in-memory **per gateway instance**. It is not a shared global host-wide limit across separate gateway instances. Each instance bounds its own reservations and spools independently.
- **Dynamic Accounting**:
  - *Active conversion*: Reserves `maxSpoolBytes` upfront against the aggregate quota to ensure concurrent conversions cannot exceed the instance budget.
  - *Completed spool*: Once conversion completes, the reservation is released and only the exact on-disk byte size is accounted.
  - *Cleaned spool*: All accounted bytes are immediately released.

### LRU Retention, Active Reader Protection, and Retry
- Completed spools are retained on disk to allow subsequent seek and range requests without re-encoding.
- When new spool allocations require disk space, completed spools with zero active readers are evicted in Least Recently Used (LRU) order based on access timestamps.
- **Active Reader Protection**: Each range request calls `acquireReader()` and `releaseReader()`. Spools with active readers are never selected for eviction, preventing in-flight stream interruption.
- **Transient Allocation Retry**: Transient allocation errors (HTTP 429 `media_busy` when `HybridMaxConversions` is reached, or HTTP 507 `spool_quota_exceeded` when active readers prevent LRU eviction) are **not permanently cached** on the playback session. When concurrent conversions finish or active readers release their locks, subsequent client GET requests re-attempt allocation and proceed cleanly.

### Concurrency and Thread Safety
- **Strict Lock Ordering**: Lock acquisition strictly follows `hybridSpoolManager.mu -> hybridSpool.mu`. Spool locks are never held while acquiring the manager lock, preventing lock inversion deadlocks.
- **Safe Lifecycle Transitions**: `onSpoolCompleted` and `cleanupSpool` atomically synchronize `reserved`, `actualBytes`, `size`, `modTime`, `lastActivity`, and `activeConvs`. Concurrent cancellation and conversion cannot resurrect a deleted spool file or leak active conversion slots.

### Lazy Conversion
- Creating a playback plan (`POST /v1/playback`, quality adaptation, or track switching) does not start conversion eagerly.
- Spool conversion begins lazily only when the stream endpoint (`GET /v1/streams/<sessionId>`) is first requested by the client, avoiding CPU and disk waste for unplayed or pre-buffered catalog items.

### Cancellation, Error, and Expiry Cleanup
- **Session Cancellation**: When a playback session is stopped (`DELETE /v1/playback/<sessionId>`), superseded, or the server closes, `session.ctx` is cancelled. Registered `context.AfterFunc` handlers immediately delete on-disk spool files and release quota.
- **Conversion Failure**: If ffmpeg fails, times out, or exceeds writer bounds, the spool is cleaned up immediately and partial temp files are deleted.
- **Session Expiry**: Sessions are bounded by a deadline. Upon expiration, context cancellation triggers automatic cleanup.

---

## 3. Long-Media Limitation & Two-Phase Client Flow

### The 25-Second Startup Timeout
- The synchronous `GET /v1/streams/<sessionId>` handler waits up to `maxHybridStartupWait` (25 seconds) for spool conversion to complete before serving HTTP headers (`Content-Length`, `Accept-Ranges`).
- If conversion does not complete within 25 seconds, the gateway returns HTTP 504 (`conversion_timeout`).
- Conversion continues in the background under `session.ctx`. Subsequent retries by the client can connect to the completed spool once ready.

### Why Long Media Cannot Rely on Synchronous GET
- For short fixtures, clips, and trailers (e.g. the physically verified 20s test fixture), remuxing and audio transcoding finish within 1–3 seconds, allowing instantaneous playback.
- For full-length feature films (90–120 minutes), remuxing and AAC transcoding take between 30 and 120+ seconds even with stream-copied video. A synchronous HTTP GET request will time out.
- Faking `Content-Length` is unsafe and rejected: `AVAPIMediaPlayer` relies on accurate byte boundaries and container atom indexing. Premature EOF or mismatched sizes cause player crashes and fatal playback errors.

### Future Architecture: Two-Phase Client Preparation Flow
Supporting full-length movies on legacy players requiring fixed `Content-Length` requires an explicit two-phase staging workflow in the client:
1. **Staging / Preparation Phase**:
   - Client creates the playback session.
   - Client triggers background preparation (or observes a `PREPARING` playback state via polling/events).
   - Client UI displays a progress/buffering indicator while the gateway spools the movie in the background.
2. **Playback Phase**:
   - Once the gateway signals `READY` (spool conversion finished and exact size known), the client passes the stream URL to `AVAPIMediaPlayer`.
   - The player connects and immediately receives HTTP 200 with the exact `Content-Length`, proceeding without timeout.

*Full movies have not been physically tested or verified under the current synchronous GET flow; runtime verification is limited to short media fixtures.*
