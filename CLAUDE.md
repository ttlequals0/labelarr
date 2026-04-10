## Critical Rules

- NEVER create mock data or simplified components unless explicitly told to
- NEVER replace existing complex components with simplified versions -- fix the actual problem
- ALWAYS work with the existing codebase -- do not create new simplified alternatives
- ALWAYS find and fix the root cause instead of creating workarounds
- ALWAYS track changes in CHANGELOG.md
- ALWAYS refer to CHANGELOG.md when working on tasks
- ALWAYS verify the app builds successfully before declaring done
- Fix all review findings -- never skip items as "not worth the churn"

## Build and Test

Go is not installed locally. Use Docker for all build verification:

```bash
docker build -t labelarr:test .                    # Verify compilation
docker build --platform linux/amd64 -t ttlequals0/labelarr:latest .  # Production build
docker push ttlequals0/labelarr:latest              # Push to Docker Hub
```

Format Go code via Docker (macOS `sed` is aliased to `gsed`):

```bash
docker run --rm -v $(pwd):/app -w /app golang:1.21-alpine gofmt -w <file.go>
```

Deploy to Portainer after pushing the Docker image:

```bash
curl -X POST https://portainer.ttlequals0.com/api/stacks/webhooks/01e4c5be-188c-4ce7-9213-d42b31a78851
```

## Architecture

Go 1.21 service. No external Go dependencies (stdlib only). Runs as a Docker container.

```
cmd/labelarr/main.go          Entry point, client init, processing loop
internal/config/config.go      Env var loading, Config struct, validation
internal/media/processor.go    Core processing logic (batch iteration, keyword sync, TMDb ID extraction)
internal/plex/client.go        Plex API client (libraries, items, label/genre updates)
internal/tmdb/client.go        TMDb API client (keyword fetching, connection testing)
internal/radarr/client.go      Radarr API client (movie lookup, cached library fetch)
internal/sonarr/client.go      Sonarr API client (series lookup, cached library fetch)
internal/webhook/server.go     Plex webhook HTTP server (debounce, graceful shutdown)
internal/export/export.go      Label-based file path export (txt/json)
internal/storage/storage.go    JSON persistence for processed items
internal/utils/normalize.go    Keyword normalization (90+ test cases)
```

## Coding Conventions

- No emoji in Go source. Log tags use bracketed format: `[OK]`, `[ERROR]`, `[SKIP]`, `[WARN]`, `[INFO]`, `[LOOKUP]`, etc.
- `[ERROR]` is reserved for actual failures (API errors, decode errors). Expected lookup misses use `[SKIP]`.
- No WHAT comments (comments that describe what the next line does). Only add comments explaining WHY.
- ASCII only in all output -- no em dashes, smart quotes, or unicode arrows.
- Use `fmt.Printf` for all logging (no log package).

## Key Patterns

- `NewProcessor` accepts a `Clients` struct, not individual client args
- Radarr/Sonarr clients cache `GetAllMovies()`/`GetAllSeries()` results. Call `ClearCache()` to refresh.
- `Processor.ClearCaches()` resets keyword cache + Radarr/Sonarr caches. Called at the start of each processing cycle.
- `Processor.ProcessAllItems()` has a per-library mutex guard -- safe to call from both timer and webhook goroutines.
- Keyword cache (`keywordCache`) is protected by `sync.RWMutex`.
- Batch iteration uses `makeBatches()` / `logStart()` / `pauseAfterBatch()` helpers.

## Gotchas

- macOS `sed` is aliased to `gsed` (GNU sed). Use `gsed -i` not `sed -i ''`.
- `strings.Title` is deprecated in Go 1.18+. Use manual capitalization instead.
- Plex webhooks require Plex Pass. The webhook payload uses `LibrarySectionType` ("movie"/"show") not `Metadata.Type` ("movie"/"episode") for media type resolution.
- `DATA_DIR` defaults to empty (ephemeral mode). Storage is only created when `DATA_DIR` is explicitly set.
- Docker image: `ttlequals0/labelarr:latest` on Docker Hub.
- Upstream repo: `nullable-eth/labelarr`. Our fork adds batch processing, keyword prefix, webhooks.
