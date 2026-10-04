
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

.PHONY: soloist-check soloist-synthetic-check
soloist-check:
	cd gateway && GOMAXPROCS=2 go vet -p 2 ./... && GOMAXPROCS=2 go test -race -p 2 ./internal/worker ./internal/providers ./internal/server ./internal/playback ./internal/media -run 'Soloist|Spotify|PCM|Remote' -count=1
	CGO_ENABLED=0 GOMAXPROCS=2 go -C gateway build -p 2 ./cmd/zombie-worker ./cmd/zombie-soloist ./cmd/zombied
soloist-synthetic-check:
	docker build -f wrappers/soloist-runtime/Dockerfile -t zombie-box-tv/soloist-runtime:0.1.0-zb006.2-qa .
	docker run --rm --network none --user 65531:65531 --read-only --cap-drop ALL --security-opt no-new-privileges:true --pids-limit 32 --memory 256m --cpus 1 --tmpfs /tmp:size=33554432,uid=65531,gid=65531,mode=0700 zombie-box-tv/soloist-runtime:0.1.0-zb006.2-qa -synthetic-check
