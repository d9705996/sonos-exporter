# sonos-exporter

A Prometheus exporter (written in Go) that auto-discovers Sonos speakers on your network and exports key playback/health metrics.

## What it collects

- `sonos_speaker_up` - 1 if the speaker responded to metric collection calls.
- `sonos_speaker_volume_percent` - current master volume (0-100).
- `sonos_speaker_is_playing` - 1 when transport state is `PLAYING` or `TRANSITIONING`.
- `sonos_speaker_uptime_seconds` - speaker uptime from `/status/zp` when available, otherwise observed uptime since first discovery (`source` label indicates which).
- `sonos_speaker_info` - static info metric (value always `1`) with labels for model/version.

All metrics include identifying labels:

- `uuid`
- `name`
- `model`
- `host`

## Run locally

```bash
go run .
```

Default endpoint is:

- metrics: `http://localhost:9798/metrics`
- UI root: `http://localhost:9798/`

Flags:

- `-web.listen-address` (default `:9798`)
- `-web.telemetry-path` (default `/metrics`)

## Prometheus scrape config

```yaml
scrape_configs:
  - job_name: sonos
    static_configs:
      - targets:
          - sonos-exporter:9798
```

## Notes

- Discovery uses SSDP `M-SEARCH` for `urn:schemas-upnp-org:device:ZonePlayer:1`.
- Exporter refreshes discovered speakers every 60 seconds.
- Make sure UDP multicast and TCP access to Sonos speakers are allowed from where the exporter runs.

## CI/CD

GitHub Actions workflows run as follows:

- `.github/workflows/ci.yml` for lint/test/vuln checks
- `.github/workflows/docker.yml` for Docker build/publish (only on push to `main`)
- `go test` coverage profile generation (`cover.out`) + `go-test-coverage` status check
- `golangci-lint` (pinned CLI run in CI)
- `golang-vulncheck` action
- Dockerfile linting via `hadolint` runs only when `Dockerfile` changes

Published image:

- `ghcr.io/<owner>/sonos-exporter:latest`
- `ghcr.io/<owner>/sonos-exporter:sha-<commit>`
- `ghcr.io/<owner>/sonos-exporter:<semver>` (when a matching `vX.Y.Z` tag exists in the repository)
