# Phone companion feature

`internal/companion` owns invitation consumption, consent, durable grants and
bounded remote command lifetime. Its consumer-owned persistence interface, clock,
random source and event publisher are constructor-injected. `internal/server`
adapts HTTP, device authentication, QR rendering and existing Cast ownership.
Full and Edge consume this same Go code; there is no second receiver implementation.

SQLite buckets `companion-invitations`, `companion-requests` and `companion-grants`
use the existing transactional record store. Invitations/requests expire in two
minutes; approved grants remain until revoked. Only credential hashes persist
for grants; ephemeral QR invitation secrets expire. Request and grant listings
strip hashes. Phone tokens cannot access ordinary device/provider/admin endpoints.

The event `companion.changed` wakes target listeners. Current Android command
consumption uses bounded foreground polling once/second, at-most-once delivery,
two-second expiry, local window/consent checks and explicit execution receipts.
Commands/heartbeats are volatile and never replay after a server restart. Key
holds and exactly-once effects are not promised. Extended EPG/system/OEM controls,
fully event-driven remote delivery and physical latency acceptance remain open.

PNG QR encoding uses `github.com/skip2/go-qrcode` pinned in go.mod and the reference
lock. Preserve [its MIT license](licenses/go-qrcode-LICENSE) in binary distributions.
The thin Client only displays the bounded PNG; ZXing belongs to the Cast APK.

The protocol repository documents endpoints, scopes, proof construction and wire
fixtures in `protocol/companion.md`. HTTP remains a trusted-LAN transport; proof
before rediscovery prevents accidental credential disclosure to a different host,
not traffic inspection or active relay attacks. Keep remote exposure behind the
existing deployment boundary.
