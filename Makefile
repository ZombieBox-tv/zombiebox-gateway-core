
.PHONY: format format-check
format:
	python3 scripts/format.py
format-check:
	python3 scripts/format.py --check

.PHONY: check build references wrappers-check spotify-patch-check
check:
	cd gateway && GOMAXPROCS=2 go vet -p 2 ./... && GOMAXPROCS=2 go test -race -p 2 ./...
build:
	mkdir -p dist
	cd gateway && CGO_ENABLED=0 GOMAXPROCS=2 go build -p 2 -trimpath -o ../dist/zombied ./cmd/zombied
references:
	python3 scripts/sync-upstreams.py
wrappers-check:
	npm ci --prefix wrappers/youtube-receiver --ignore-scripts --no-audit --no-fund
	npm ci --prefix wrappers/youtube --ignore-scripts --no-audit --no-fund
	node --test wrappers/youtube/*.test.mjs wrappers/youtube-receiver/*.test.mjs
spotify-patch-check:
	python3 scripts/test-spotify-patch.py

.PHONY: architecture-check
architecture-check:
	python3 scripts/check-architecture.py
