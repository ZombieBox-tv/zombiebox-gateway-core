# Credential-free LAN discovery

`zombied -discovery-listen 0.0.0.0:8098 -discovery-http-port 8090` enables the
bounded IPv4 UDP responder beside the normal gateway. It is opt-in in the core
binary. The HTTP listen address must also be reachable from the trusted LAN.

`-discovery-only` runs just the responder, without opening SQLite, loading provider
credentials or starting HTTP/media workers. Full uses this mode in a host-network
sidecar; Edge can enable discovery in its native core process. If integrated
listener startup fails, manual pairing and core HTTP remain available.

Protocol: `ZOMBIE_DISCOVER_V1 <32 hex nonce>\n` →
`ZOMBIE_GATEWAY_V1 <same nonce> <HTTP port>\n`. The response advertises neither
a gateway identity nor a pairing grant. One fixed buffer/goroutine, local IPv4
sources, a global 20 replies/s budget and bounded cancellation protect the optional
listener. Tests use loopback only and do not establish Wi-Fi/OEM reachability.
