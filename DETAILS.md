# yt_exporter details

How the collectors work, the permissions of the token, the configuration, the metrics with example queries and
alerts, and the deployment. The overview and the quick start are in the [README](README.md).

- [How it works](#how-it-works)
- [Issues per project and type](#issues-per-project-and-type)
- [Token and permissions](#token-and-permissions)
- [Configuration](#configuration)
- [Metrics](#metrics)
- [Example alerts](#example-alerts)
- [Deployment](#deployment)
- [Development](#development)

## How it works

Collectors read on every scrape, enabled by default (`info` always):

- `info` — `GET /api/config`
- `telemetry` — `GET /api/admin/telemetry`
- `backup` — `GET /api/admin/databaseBackup/settings/backupStatus` and `.../backups`
- `license` — `GET /api/admin/globalSettings/license`

Heavier collectors, refreshed in the background every `interval` (the default in brackets):

- `projects` (5m) — `GET /api/admin/projects` + `POST /api/issuesGetter/count`
- `issue_types` (2m, **disabled** by default) — `GET /api/admin/projects`, `GET /api/issues` + `POST
  /api/issuesGetter/count`
- `users` (15m) — `GET /api/users` (the `userType` field since 2026.2)
- `queries` (5m, enabled when `items` are defined) — `POST /api/issuesGetter/count`

Counting issues takes many requests (2 per project for `projects`; `issue_types` loads all issues once and then reads
only the changed ones, see below), so these collectors run in the background and a scrape returns their last result.
YouTrack answers `count: -1` until it has finished counting, the client then repeats the request with a growing
delay. The number of parallel requests to YouTrack is limited by `youtrack.max_concurrent_requests`.

A failing collector does not break the scrape: it shows up as `youtrack_scrape_collector_success{collector="..."} 0`
and in the log (on HTTP 401/403 with a hint about the token's permissions).

## Issues per project and type

The `issue_types` collector counts the open (unresolved) and closed (resolved) issues of every project that is not
archived, or only of the projects listed in `include` (archived ones are skipped also when listed). The issue types
are the values of the project's `type_field` custom field (`Type` by default, a single-value field); every type is
exported, also types without issues (value 0). Issues without a type are counted with `type=""`, so the sum over all
types equals the number of issues in the project. A project without the type field has only the `type=""` series.

Instead of counting every project and type on each run, the collector keeps an index of the issues in memory (ID →
project, type, resolved; about 12 MB per 100 000 issues):

1. **Load** — when a project is counted for the first time (also after a restart of the exporter), all its issues
   are read with only the needed fields: `GET /api/issues?query=project: {X} sort by: created asc` with
   `fields=id,updated,resolved,project(shortName),customFields(name,value(name))&customFields=<type_field>`, 500
   issues per request, plus the values of the type field from `.../projects/{id}/customFields`.
2. **Updates** — every `interval` the issues updated since the previous run are read newest first
   (`sort by: updated desc`, 100 per page) until the previous run is reached, usually one request. A changed state,
   type or project of an issue is applied from its current values.
3. **Check** — deleted issues and issues moved to a project that is not counted do not show up in the updates. One
   count request per run compares the number of issues of the counted projects with the index; when it differs
   (also after reading the updates once more), one count per project finds the projects that are loaded again.
4. **Full resync** — every `full_resync_interval` (24h) every project is loaded again. This corrects the changes
   that do not update the issues: a renamed type value, a state whose *resolved* flag was changed, a change of the
   token's permissions.

A run normally takes three requests (projects, one page of updates, one count) regardless of the number of projects
and types; a load takes `issues / 500` requests per project. A run that loads projects may take longer than the
interval, it is limited to 30 minutes (or the interval when that is longer); a project that fails to load is tried
again in the next run. `youtrack_issue_types_project_reloads_total{reason}` shows how often projects were loaded.

## Token and permissions

The exporter needs a [permanent token](https://www.jetbrains.com/help/youtrack/server/manage-permanent-token.html)
of a dedicated technical account. Permissions per collector:

- `telemetry`, `backup` — **Low-level Admin Read**,
- `license` — read access to the global settings,
- `projects`, `issue_types`, `queries` — **Read Issue** in all counted projects (only what the account can see is
  counted),
- `users` — read access to users (**Read User**).

A collector the account has no permissions for can be disabled (`enabled: false`).

## Configuration

YAML file (`--config.file`, optional) — a full example with comments: [`config.example.yml`](config.example.yml).
Environment variables override the file:

| Variable                  | Meaning                                            |
|---------------------------|----------------------------------------------------|
| `YOUTRACK_URL`            | YouTrack URL, e.g. `http://127.0.0.1:8080`         |
| `YOUTRACK_TOKEN`          | permanent token (takes precedence over a file)     |
| `YOUTRACK_TOKEN_FILE`     | file holding the token                             |
| `YT_EXPORTER_CONFIG_FILE` | path of the configuration file (= `--config.file`) |

Flags:
- `--web.listen-address` (default `:9776`),
- `--web.telemetry-path` (`/metrics`),
- `--web.config.file`
  ([TLS / basic auth](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md)),
- `--log.level`,
- `--log.format`,
- `--config.check` (validate the configuration and exit).

## Metrics

**Telemetry** (`telemetry`) — YouTrack returns most values as formatted text (`"5.0 MB"`, `"93%"`), the exporter turns
them into numbers (sizes as multiples of 1024). A value that cannot be parsed is skipped (logged at `debug` level).

- `youtrack_up` — 1 if the API answered `/api/config`
- `youtrack_build_info{version,build}` — YouTrack version
- `youtrack_start_time_seconds` — YouTrack start (uptime: `time() - youtrack_start_time_seconds`)
- `youtrack_available_processors` — processors available to the JVM
- `youtrack_memory_bytes{type="available|allocated|used"}` — JVM memory as reported by YouTrack
- `youtrack_database_size_bytes`, `youtrack_database_full_size_bytes`, `youtrack_text_index_size_bytes` — database
  size without BLOBs, full size, text index size
- `youtrack_pending_async_jobs` — queue of asynchronous jobs
- `youtrack_database_background_threads`, `youtrack_report_calculator_threads`,
  `youtrack_notification_analyzer_threads` — threads
- `youtrack_database_queries_cache_entries` — entries of the queries cache
- `youtrack_database_queries_cache_hit_ratio`, `youtrack_blob_strings_cache_hit_ratio` — hit ratio 0–1
- `youtrack_transactions_total` (counter) — transactions since start
- `youtrack_transactions_per_second`, `youtrack_requests_per_second` — rates computed by YouTrack
- `youtrack_online_users` — users online

**Backup, license:**

- `youtrack_backup_in_progress`, `youtrack_backup_cancelled`, `youtrack_backup_error` — state of the last backup (0/1)
- `youtrack_backup_error_timestamp_seconds` — time of the last backup error
- `youtrack_backup_files`, `youtrack_backup_files_size_bytes` — number and total size of backup files
- `youtrack_backup_last_timestamp_seconds`, `youtrack_backup_last_size_bytes` — newest backup file
- `youtrack_license_valid` — 1 if the license reports no error

**Issues, users:**

- `youtrack_project_info{project,project_name,archived}` — project (value 1)
- `youtrack_project_issues{project}`, `youtrack_project_issues_unresolved{project}` — all / `#Unresolved` issues
- `youtrack_project_issues_by_type{project,type,status="open|closed"}` — `#Unresolved` / `#Resolved` issues per issue
  type, `type=""` = no type (`issue_types` collector)
- `youtrack_issue_types_project_reloads_total{reason="initial|resync|mismatch"}` — loads of all issues of a project by
  `issue_types` (counter)
- `youtrack_query_issues{name,<labels>}` — results of the queries from `collectors.queries.items`
- `youtrack_users{type,banned}` — accounts (guest excluded); `type`: `standard_user`, `agent`, `reporter` (2026.2+),
  `unknown`

**Exporter state:** `youtrack_scrape_collector_success|duration_seconds{collector}`,
`youtrack_scrape_collector_last_success_timestamp_seconds{collector}` (background collectors),
`yt_exporter_youtrack_api_requests_total{endpoint,code}`, `yt_exporter_youtrack_api_request_duration_seconds{endpoint}`,
`yt_exporter_build_info` and the standard `go_*` / `process_*` metrics.

**Example queries** for `issue_types`:

```promql
# open issues per type across all projects
sum by (type) (youtrack_project_issues_by_type{status="open"})
# net change of the closed issues per project and type over a day
delta(youtrack_project_issues_by_type{status="closed"}[1d])
# share of closed bugs per project
youtrack_project_issues_by_type{type="Bug",status="closed"}
  / ignoring (status) sum without (status) (youtrack_project_issues_by_type{type="Bug"})
```

## Example alerts

```yaml
groups:
  - name: youtrack
    rules:
      - alert: YouTrackDown
        expr: youtrack_up == 0
        for: 5m
      - alert: YouTrackBackupFailed
        expr: youtrack_backup_error == 1
      - alert: YouTrackBackupTooOld
        expr: time() - youtrack_backup_last_timestamp_seconds > 2 * 86400
      - alert: YouTrackAsyncJobsPiling
        expr: youtrack_pending_async_jobs > 100
        for: 15m
      - alert: YouTrackLicenseError
        expr: youtrack_license_valid == 0
      - alert: YouTrackExporterStale
        expr: time() - youtrack_scrape_collector_last_success_timestamp_seconds > 3 * 900
```

## Deployment

The image (distroless, nonroot) is configured through the environment or a mounted `--config.file`:

```bash
docker run -p 9776:9776 -e YOUTRACK_URL=https://youtrack.example.com -e YOUTRACK_TOKEN=perm:... \
  ghcr.io/kinjelom/yt_exporter:latest
```

Next to YouTrack, the simplest way is a second container in the YouTrack pod (Kubernetes, podman `kube play`, e.g. the
BOSH [pods release](https://github.com/kinjelom/pods-boshrelease)) — the containers of a pod share the network, so
`YOUTRACK_URL=http://127.0.0.1:8080`:

```yaml
- name: yt-exporter
  image: ghcr.io/kinjelom/yt_exporter:<version>
  ports:
    - { containerPort: 9776, hostPort: 9776 }
  env:
    - { name: YOUTRACK_URL, value: "http://127.0.0.1:8080" }
    - name: YOUTRACK_TOKEN
      valueFrom: { secretKeyRef: { name: yt-exporter, key: token } }
```

The host has to reach ghcr.io, or a registry mirroring the image. With the BOSH pods release the
`prometheus_exporter_port` tag of a job holds one port: when it already points to another exporter of the pod (e.g.
a `jmx_exporter` javaagent in the YouTrack JVM), add the scrape of port 9776 to the Prometheus configuration
separately.

## Development

```bash
make test        # gofmt, vet, unit tests with the race detector
make             # tests + bin/yt_exporter
```

```
cmd/yt_exporter/      main: flags, HTTP, metrics registry
internal/config/      YAML file, environment variables, validation
internal/youtrack/    REST API client (pagination, count retries, parsing "5.0 MB" / "93%")
internal/collector/   Prometheus collectors (on scrape and in the background)
```

Building and releasing: [RELEASING.md](RELEASING.md).
