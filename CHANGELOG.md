# Changelog

Every notable change, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the versions
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

`scripts/release.sh` reads this file: it renames `[Unreleased]` to the version
being released, dates it, opens a fresh `[Unreleased]`, and uses the section it
just closed as the release notes and the annotated tag's message. So what you
write here while working is what the release says - add the entry with the
change, not at release time.

## [Unreleased]

## [0.1.0] - 2026-10-04

The first release: a Prometheus exporter for JetBrains YouTrack Server (targeting 2026.2) that reads the YouTrack
REST API and exposes application metrics. JVM metrics are left to `jmx_exporter`.

### Added

- Collectors read on every scrape:
  - `info` - `youtrack_up` and the YouTrack version and build.
  - `telemetry` - start time, processors, JVM memory, database and text index size, asynchronous jobs, threads,
    cache hit ratios, transactions, request rate and users online. YouTrack's formatted values (`"5.0 MB"`, `"93%"`)
    are turned into numbers.
  - `backup` - state of the last backup, its error time, and the number, total size and newest of the backup files.
  - `license` - `youtrack_license_valid`.
- Collectors refreshed in the background, a scrape returns their last result:
  - `projects` (every 5m) - all and unresolved issues per project, with `youtrack_project_info`.
  - `issue_types` (every 2m, disabled by default) - open and closed issues per project and issue type,
    `youtrack_project_issues_by_type{project,type,status}`. It keeps an index of the issues in memory, loads a
    project once and then reads only the issues updated since the previous run - usually three requests per run,
    whatever the number of projects and types. Deleted and moved issues are found by a count check, and every
    project is loaded again every 24h (`full_resync_interval`).
  - `users` (every 15m) - accounts by type (`standard_user`, `agent`, `reporter`, from YouTrack 2026.2) and ban state.
  - `queries` (every 5m) - issue counts of your own YouTrack search queries, with your own labels.
- A failing collector does not break the scrape: `youtrack_scrape_collector_success`, `_duration_seconds` and
  `_last_success_timestamp_seconds` per collector, and a hint about the token's permissions in the log on HTTP
  401/403.
- Gentle on YouTrack: a limit of parallel requests across all collectors (`youtrack.max_concurrent_requests`), and
  count requests that YouTrack has not finished (`count: -1`) repeated with a growing delay.
- API client metrics: `yt_exporter_youtrack_api_requests_total{endpoint,code}` and
  `yt_exporter_youtrack_api_request_duration_seconds{endpoint}`, plus `yt_exporter_build_info`.
- Configuration from an optional YAML file (`--config.file`) overridden by the environment (`YOUTRACK_URL`,
  `YOUTRACK_TOKEN`, `YOUTRACK_TOKEN_FILE`); a custom CA or `insecure_skip_verify` for TLS to YouTrack;
  `--config.check` to validate it and exit.
- The standard exporter toolkit: `--web.listen-address` (default `:9776`), `--web.telemetry-path`, TLS and basic
  auth through `--web.config.file`, a landing page and `/-/healthy`.
- Release archives for Linux and macOS (amd64, arm64) and Windows (amd64) with SHA-256 checksums and third-party
  licenses, and the distroless, nonroot container image `ghcr.io/kinjelom/yt_exporter`.
