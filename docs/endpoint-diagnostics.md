# Explicit endpoint diagnostics

The shared core can inspect one selected local IPv4 endpoint without starting the
server or opening its configuration, SQLite database or credentials:

```sh
zombied --diagnose-address 192.168.1.20 \
  --diagnose-http-port 8090 \
  --diagnose-discovery-port 8098 \
  --diagnose-rtsp-port 8554
```

Use the actual gateway address and configured ports. The address must be a literal
private, link-local or loopback IPv4 address; DNS and public addresses are rejected.
This is an explicit diagnostic, never an automatic network scan. Three bounded
connections run within a shared two-second deadline and honor cancellation:

| Result | Measured exchange | What it does not establish |
|---|---|---|
| `httpHealth` | GET `/health`, status and API version 1 | Account readiness or media playback |
| `discoveryUnicast` | Nonce-matched UDP8098 reply and advertised HTTP port | Broadcast/multicast traversal or Wi-Fi client isolation |
| `rtspOptions` | Matched RTSP CSeq and advertised DESCRIBE/SETUP/PLAY methods | Authentication success, publishing, RTP delivery or decoding |

Each result contains only `state` and `elapsedMs`. States are `ok`, `unavailable`,
`invalid_response`, `authentication_required` and discovery-only `port_mismatch`.
HTTP redirects are not followed. Response sizes and read times are bounded. Reports
omit addresses, raw replies, tokens and discovery nonces. `mediaValidated` and
`accountValidated` remain false regardless of reachability.

Exit 0 means a report was produced, **not** that every endpoint is healthy. Invalid
arguments fail before connecting. Loopback results cannot establish access from a
second LAN device. No successful result updates decoder/backend health or media
capabilities; functional media and device probes remain separate evidence.

## Full and Edge

Full uses this same CLI from a core build that contains dev.40. Run from the network
whose reachability is in question. Container loopback is that container, not the
host or another container. Existing running images do not acquire new flags until
explicitly rebuilt and deployed.

Edge exposes the installed binary through:

```sh
zombiebox doctor --network --address 192.168.1.20
```

Its optional `--http-port`, `--discovery-port` and `--rtsp-port` override the defaults
above. Plain `zombiebox doctor` retains its offline inventory behavior. The doctor
allowlists the child report and never prints raw stderr. An older installed binary,
malformed result or child timeout reports `unavailable_or_unsupported`; upgrade to
a reviewed bundle containing this implementation before expecting these probes.

Host evidence uses actual loopback HTTP/UDP/RTSP fixtures, malformed responses,
authentication rejection and silent cancellable peers. It is not Android/Bionic,
physical LAN, decoder or sender/receiver acceptance.
