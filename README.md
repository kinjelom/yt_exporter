# yt_exporter

Prometheus exporter for JetBrains YouTrack (Server, targeting **2026.2**). It reads the YouTrack REST API and exposes
application metrics: server telemetry, backup status, license, issue counts per project, per project and issue type
and of custom queries, and user accounts. JVM metrics (heap, GC, threads) are left to `jmx_exporter`.

## Quick start

Binaries for Linux, macOS and Windows are on the [releases page](https://github.com/kinjelom/yt_exporter/releases), the
image is `ghcr.io/kinjelom/yt_exporter`. Or build it (Go 1.26+):

```bash
make                                  # gofmt, vet, tests + bin/yt_exporter
YOUTRACK_URL=https://youtrack.example.com YOUTRACK_TOKEN=perm:... ./bin/yt_exporter
curl -s localhost:9776/metrics | grep ^youtrack_
```

The token belongs to a technical account with read permissions, see
[Token and permissions](DETAILS.md#token-and-permissions); the rest of the settings are in
[`config.example.yml`](config.example.yml).

## Collectors

- **`info`, `telemetry`, `backup`, `license`** — read on every scrape: version, server metrics (memory, database,
  jobs, transactions, users online), backup state and files, license errors.
- **`projects`** (every 5m) — all and unresolved issues per project.
- **`issue_types`** (every 2m, disabled by default) — open and closed issues per project and issue type, from an index
  of the issues kept in memory.
- **`users`** (every 15m) — user accounts by type and ban state.
- **`queries`** (every 5m) — issue counts of your own YouTrack search queries.

A failing collector does not break the scrape: `youtrack_scrape_collector_success{collector}` shows which one failed.

## More

- [How it works](DETAILS.md#how-it-works)
- [Issues per project and type](DETAILS.md#issues-per-project-and-type)
- [Token and permissions](DETAILS.md#token-and-permissions)
- [Configuration](DETAILS.md#configuration)
- [Metrics](DETAILS.md#metrics)
- [Example alerts](DETAILS.md#example-alerts)
- [Deployment](DETAILS.md#deployment)
- [Development](DETAILS.md#development)
- [Building and releasing](RELEASING.md)

## License

[MIT](LICENSE)
