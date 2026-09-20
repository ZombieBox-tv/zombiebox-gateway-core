# Gateway core

One Go module for Full and Edge. `go run ./cmd/zombied` serves `/health` on `127.0.0.1:8090`; `-listen` explicitly selects a LAN interface. `go test -race ./...` checks the bootstrap surface. No external Go dependencies or persistence yet.

Next: registration/pairing, module registry, Home and long polling backed by `../protocol/`. Add adapters under modules after the client spike permits progress; never expose upstream DTOs.
