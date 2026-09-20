# Gateway core

One Go module for Full and Edge. From the repository root, run `make gateway-run` or `make full-up`; both use the ignored `.local/gateway/` runtime. Do not run both against the same SQLite file.

Implemented: health, paired devices, preferences/capability reports, semantic Home/modules, long polling, write-only provider administration, catalog pages, direct-play sessions, HLS/HTTP relay and resume progress. SQLite is the only database. Optional integrations fail independently.

See [services and credentials](../docs/development/services-and-credentials.md) for configuration, limits and actual adapter coverage. `make check` runs race-enabled tests and validates real response samples against protocol schemas. `CGO_ENABLED=1 go test -tags sqlite_cgo ./...` checks the alternate SQLite driver on the host; Edge itself requires native Termux validation.
