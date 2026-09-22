# URL queues, adaptation and receiver listening

Dev.35 keeps these three lifetimes separate. All ports are injected into the shared
Go implementation; Full and Edge consume the same code.

- `internal/mediaqueue` owns one ephemeral queue of at most sixteen public direct
  file URLs. The download transport pins public DNS answers and isolates redirects
  from private providers. Downloads are bounded to two minutes/256 MiB and use the
  existing temporary-file probe/planner. Only the current receiver's completion
  advances it. Revocation, replacement, cancellation, failure and restart do not
  advance it. Closing the Cast queue screen does not cancel server work.
- `internal/playback.Adaptation` requires two distinct recent bandwidth samples
  before returning a lower quality ceiling for an opted-in Auto session. The Client
  samples at most once per 65 seconds, restarts at the observed position and discards
  stale responses. No live upshift, seamless ABR or inferred hardware acceleration.
- The explicit `universal` media selection requires local casting/handoff consent.
  Listening and playback ownership are independent: Spotify/AirPlay inbox plus
  YouTube TV Code/DIAL may remain armed while one transport owns playback. A prepared
  replacement retires the old stream. Previously active media sources must become
  observably idle before reclaiming the screen; private YouTube epochs reject old
  polls/acks and invalidate old source resolution. Older workers close their lease
  instead. Client YouTube polling remains foreground-scoped.

Optional worker errors preserve a known Cast plan. Discovery is never consent.
The queue's direct-URL import does not scrape webpages, import arbitrary manifests,
resume partial downloads or forward provider credentials. Existing provider HLS/DASH
adapters continue to own those authenticated media paths.

Automated tests use injected workers and synthetic media. They do not establish
real account, OEM, multicast, encoding or playback acceptance.
