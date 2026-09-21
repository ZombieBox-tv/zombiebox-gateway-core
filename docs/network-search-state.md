# Measured LAN playback, federated search and state maintenance

## Measured adaptation

The paired `GET /v1/network/sample` endpoint supplies exactly 1 MiB without
compression or caching. Two downloads can run globally, each has a four-second
write deadline, and each device can start at most one per minute. A private
single-use sample ID binds `POST /v1/device/network` to that device for 15 seconds.
The client streams into an 8-KiB buffer for at most three seconds of sampling;
at least 32 KiB must arrive. Connection/report deadlines are independently bounded.

Evidence expires after five minutes and is not persisted across gateway restarts.
The planner reserves 30% headroom against measured goodput and compares ffprobe's
source bitrate. It selects the existing STANDARD/LOW conversion profiles only for
video, in Auto, and when known output probes do not reject conversion. Explicit
modes and `networkAdaptation: false` bypass the cap. Audio-only and unknown metadata
retain the existing planner. Network errors never become decoder FAIL evidence.
The client samples before new Auto playback and recovery, subject to cooldown.

This measures the gateway-to-client link, not upstream Internet bandwidth. It is
not seamless ABR, a promise that the minimum LOW profile fits every link, or a
hardware-acceleration measurement. Unknown live-TS metadata retains the explicit
compatible-retry path.

## Search

`GET /v1/search?q=...` searches six catalog providers through injected adapters.
Two searches may run globally, each with two provider workers and a six-second
context deadline. Results distinguish READY, DISABLED and UNAVAILABLE. Preview
budgets are eight items/provider for memory classes at most 64 MiB, otherwise 20.
Descriptions are bounded; EPG programmes remain in the guide/catalog flow.

Plex root search uses hubs and lazily resolves omitted media parts, based on the
[Plex API](https://developer.plex.tv/pms/) and the locked plexgo reference. Jellyfin
and YouTube use their existing upstream search. Stremio consults at most four
search-capable addon catalogs, deduplicates the first 40 results from each, and
retains scoped catalog search/paging. Catalogs without search support are not
reported as successful content searches. IPTV/local search filters their catalogs.
The existing scoped locators preserve playback/navigation without exposing tokens.
Plex root results are capped at 400; these bounded search windows are not a claim
to enumerate every remote catalog result.

## Database copy and rollback staging

```sh
zombied -state /private/gateway.db -state-check
zombied -state /private/gateway.db -state-copy /private/pre-update.db
zombied -state /private/pre-update.db -state-copy /private/restored.db
```

These maintenance commands exit without starting HTTP, media workers or migrations.
They open an existing regular database read-only, check the schema/integrity/record
JSON and use SQLite's [consistent snapshot](https://sqlite.org/lang_vacuum.html#vacuuminto).
The output has mode 0600 and is published atomically without replacing any existing
path or symlink. WAL-committed records are included. Newer unsupported schemas and
invalid databases are rejected. The command deadline is 30 seconds.

For rollback, stop the gateway, retain the current database and its journals, and
point the selected older compatible gateway at the new restored path. Never replace
a live database or restore an old database over active WAL/SHM files. Keep matching
private provider config and worker state backups separately; this copies SQLite
only. Backups contain credentials and must never enter review bundles/source Git.
Driver behavior is checked with both Go SQLite implementations on the host; this
does not certify Termux/Bionic execution or an arbitrary future schema downgrade.
