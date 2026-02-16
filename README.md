# sonos-exporter

A Prometheus exporter (written in Go) that auto-discovers Sonos speakers on your network and exports key playback/health metrics using the official Prometheus Go client library.

## What it collects

- `sonos_speaker_up` - 1 if the speaker responded to core metric collection calls.
- `sonos_speaker_volume_percent` - current master volume (0-100).
- `sonos_speaker_mute` - mute state (`1` muted, `0` unmuted).
- `sonos_speaker_bass` - current bass EQ level.
- `sonos_speaker_treble` - current treble EQ level.
- `sonos_speaker_loudness` - loudness state (`1` enabled, `0` disabled).
- `sonos_speaker_is_playing` - 1 when transport state is `PLAYING` or `TRANSITIONING`.
- `sonos_speaker_play_mode{mode=...}` - labeled current play mode (`REPEAT`, `SHUFFLE`, etc.).
- `sonos_speaker_track_position_seconds` - current playback position when available.
- `sonos_speaker_track_duration_seconds` - current track duration when available.
- `sonos_speaker_now_playing_info` - labeled metadata for current track (`title`, `artist`, `album`, `uri`), value is always `1`.
- `sonos_speaker_sub_level` - current subwoofer level (`SubGain`) when the device exposes it.
- `sonos_speaker_last_seen_timestamp_seconds` - UNIX timestamp of the latest successful discovery.
- `sonos_speaker_discovery_age_seconds` - seconds since the speaker was last discovered.
- `sonos_exporter_discovered_speakers` - total speakers currently in exporter cache.
- `sonos_speaker_uptime_seconds` - speaker uptime from `/status/zp` when available, otherwise observed uptime since first discovery (`source` label indicates which).
- `sonos_speaker_info` - static info metric (value always `1`) with labels for model/software version.

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
- debug speaker cache: `http://localhost:9798/debug/speakers`

Flags:

- `-web.listen-address` (default `:9798`)
- `-web.telemetry-path` (default `/metrics`)
- `-sonos.discovery-interval` (default `60s`)
- `-sonos.discovery-timeout` (default `3s`)
- `-sonos.speaker-stale-after` (default `10m`, set `0` to keep speakers indefinitely even if offline)
- `-sonos.static-targets` (comma-separated list of speaker IPs/hostnames, bypasses SSDP - **recommended for Docker**)
- `-otel.exporter.otlp.endpoint` (optional OTLP gRPC endpoint for logs/traces, e.g. `otel-collector:4317`)
- `-otel.exporter.otlp.insecure` (default `true`, use plaintext OTLP gRPC)


## OpenTelemetry

When `-otel.exporter.otlp.endpoint` is set, the exporter sends:

- traces over OTLP gRPC
- logs over OTLP gRPC

Telemetry includes spans around discovery and speaker metric collection calls, and structured logs are emitted through OpenTelemetry log pipelines.

When track metadata is available, the exporter also logs `sonos now playing` events whenever a speaker changes tracks.

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
- Runtime collection uses Sonos APIs on port `1400`: RenderingControl (`GetVolume`, `GetMute`, `GetBass`, `GetTreble`, `GetLoudness`, `GetEQ`) and AVTransport (`GetTransportInfo`, `GetTransportSettings`, `GetPositionInfo`) plus `/status/zp`.
- Exporter refreshes discovered speakers every 60 seconds by default (configurable via `-sonos.discovery-interval`).
- Make sure UDP multicast and TCP access to Sonos speakers are allowed from where the exporter runs.
- For Docker deployments (for example QNAP), either use `network_mode: host` or set `-sonos.static-targets` with your speaker IPs.
- **SSDP multicast does not work with Docker bridge networking**. If you see `sonos_exporter_discovered_speakers 0`, use `-sonos.static-targets`.

## Docker

Option 1 — Host networking (SSDP works natively):

```bash
docker run --network=host ghcr.io/<owner>/sonos-exporter:latest
```

Option 2 — Bridge networking with static targets (no multicast needed):

```bash
docker run -p 9798:9798 ghcr.io/<owner>/sonos-exporter:latest \
  -sonos.static-targets=192.168.1.10,192.168.1.11,192.168.1.12
```

You can find your Sonos speaker IPs in the Sonos app under Settings > System > About My System.

## CI/CD

GitHub Actions workflows run as follows:

- `.github/workflows/ci.yml` for lint/test/vuln checks
- `.github/workflows/conventional-commits.yml` for enforcing conventional commit messages on PRs and `main`
- `.github/workflows/docker.yml` for Docker build/publish (only on push to `main`), including automatic next SemVer calculation from conventional commits since the latest `vX.Y.Z` tag
- `.github/workflows/dockerfile-lint.yml` for Dockerfile lint (runs only when `Dockerfile` changes)
- `.github/workflows/conventional-commits.yml` for Conventional Commits validation on pull requests
- `go test` coverage profile generation (`cover.out`) + `go-test-coverage` status check
- `golangci-lint` (pinned CLI run in CI)
- `golang-vulncheck` action

Published image:

- `ghcr.io/<owner>/sonos-exporter:latest`
- `ghcr.io/<owner>/sonos-exporter:sha-<commit>`
- `ghcr.io/<owner>/sonos-exporter:<semver>` (automatically computed from conventional commits since the latest `vX.Y.Z` tag, or from `0.0.0` when no tag exists)
