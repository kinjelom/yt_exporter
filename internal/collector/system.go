package collector

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kinjelom/yt_exporter/internal/youtrack"
)

func newDesc(name, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc(namespace+"_"+name, help, labels, nil)
}

// infoScraper reads /api/config, it also decides youtrack_up.
type infoScraper struct {
	client *youtrack.Client
}

var buildInfoDesc = newDesc("build_info", "YouTrack version and build, the value is always 1.", "version", "build")

func (s *infoScraper) Name() string { return "info" }

func (s *infoScraper) Scrape(ctx context.Context, emit func(prometheus.Metric)) error {
	cfg, err := s.client.AppConfig(ctx)
	if err != nil {
		return err
	}
	emit(prometheus.MustNewConstMetric(buildInfoDesc, prometheus.GaugeValue, 1, cfg.Version, cfg.Build))
	return nil
}

// telemetryScraper reads /api/admin/telemetry, the "Server metrics" page of the administration.
type telemetryScraper struct {
	client *youtrack.Client
	logger *slog.Logger
}

type telemetryMetric struct {
	desc   *prometheus.Desc
	kind   prometheus.ValueType
	parse  func(string) (float64, error)
	value  func(*youtrack.Telemetry) youtrack.Value
	labels []string
}

var memoryDesc = newDesc("memory_bytes",
	"Memory of the YouTrack JVM: available (maximum heap), allocated or used.", "type")

var telemetryMetrics = []telemetryMetric{
	{
		desc:  newDesc("available_processors", "Number of processors available to the YouTrack JVM."),
		kind:  prometheus.GaugeValue,
		parse: youtrack.ParseNumber,
		value: func(t *youtrack.Telemetry) youtrack.Value { return t.AvailableProcessors },
	},
	{
		desc: memoryDesc, kind: prometheus.GaugeValue, parse: youtrack.ParseBytes, labels: []string{"available"},
		value: func(t *youtrack.Telemetry) youtrack.Value { return t.AvailableMemory },
	},
	{
		desc: memoryDesc, kind: prometheus.GaugeValue, parse: youtrack.ParseBytes, labels: []string{"allocated"},
		value: func(t *youtrack.Telemetry) youtrack.Value { return t.AllocatedMemory },
	},
	{
		desc: memoryDesc, kind: prometheus.GaugeValue, parse: youtrack.ParseBytes, labels: []string{"used"},
		value: func(t *youtrack.Telemetry) youtrack.Value { return t.UsedMemory },
	},
	{
		desc:  newDesc("start_time_seconds", "Unix time when YouTrack started."),
		kind:  prometheus.GaugeValue,
		parse: func(s string) (float64, error) { v, err := youtrack.ParseNumber(s); return v / 1000, err },
		value: func(t *youtrack.Telemetry) youtrack.Value { return t.StartedTime },
	},
	{
		desc:  newDesc("database_size_bytes", "Size of the YouTrack database without BLOBs."),
		kind:  prometheus.GaugeValue,
		parse: youtrack.ParseBytes,
		value: func(t *youtrack.Telemetry) youtrack.Value { return t.DatabaseSize },
	},
	{
		desc:  newDesc("database_full_size_bytes", "Full size of the YouTrack database, BLOBs included."),
		kind:  prometheus.GaugeValue,
		parse: youtrack.ParseBytes,
		value: func(t *youtrack.Telemetry) youtrack.Value { return t.FullDatabaseSize },
	},
	{
		desc:  newDesc("text_index_size_bytes", "Size of the full-text search index."),
		kind:  prometheus.GaugeValue,
		parse: youtrack.ParseBytes,
		value: func(t *youtrack.Telemetry) youtrack.Value { return t.TextIndexSize },
	},
	{
		desc:  newDesc("database_background_threads", "Number of database background threads."),
		kind:  prometheus.GaugeValue,
		parse: youtrack.ParseNumber,
		value: func(t *youtrack.Telemetry) youtrack.Value { return t.DatabaseBackgroundThreads },
	},
	{
		desc:  newDesc("pending_async_jobs", "Number of queued asynchronous jobs."),
		kind:  prometheus.GaugeValue,
		parse: youtrack.ParseNumber,
		value: func(t *youtrack.Telemetry) youtrack.Value { return t.PendingAsyncJobs },
	},
	{
		desc:  newDesc("database_queries_cache_entries", "Number of cached results in the database queries cache."),
		kind:  prometheus.GaugeValue,
		parse: youtrack.ParseNumber,
		value: func(t *youtrack.Telemetry) youtrack.Value { return t.CachedResultsCountInDBQueriesCache },
	},
	{
		desc:  newDesc("database_queries_cache_hit_ratio", "Hit ratio (0-1) of the database queries cache."),
		kind:  prometheus.GaugeValue,
		parse: youtrack.ParseRatio,
		value: func(t *youtrack.Telemetry) youtrack.Value { return t.DatabaseQueriesCacheHitRate },
	},
	{
		desc:  newDesc("blob_strings_cache_hit_ratio", "Hit ratio (0-1) of the BLOB strings cache."),
		kind:  prometheus.GaugeValue,
		parse: youtrack.ParseRatio,
		value: func(t *youtrack.Telemetry) youtrack.Value { return t.BlobStringsCacheHitRate },
	},
	{
		desc:  newDesc("transactions_total", "Number of database transactions since YouTrack started."),
		kind:  prometheus.CounterValue,
		parse: youtrack.ParseNumber,
		value: func(t *youtrack.Telemetry) youtrack.Value { return t.TotalTransactions },
	},
	{
		desc:  newDesc("transactions_per_second", "Database transactions per second, as computed by YouTrack."),
		kind:  prometheus.GaugeValue,
		parse: youtrack.ParseNumber,
		value: func(t *youtrack.Telemetry) youtrack.Value { return t.TransactionsPerSecond },
	},
	{
		desc:  newDesc("requests_per_second", "HTTP requests per second, as computed by YouTrack."),
		kind:  prometheus.GaugeValue,
		parse: youtrack.ParseNumber,
		value: func(t *youtrack.Telemetry) youtrack.Value { return t.RequestsPerSecond },
	},
	{
		desc:  newDesc("online_users", "Number of users online."),
		kind:  prometheus.GaugeValue,
		parse: youtrack.ParseNumber,
		value: func(t *youtrack.Telemetry) youtrack.Value {
			if t.OnlineUsers == nil {
				return ""
			}
			return t.OnlineUsers.Users
		},
	},
	{
		desc:  newDesc("report_calculator_threads", "Number of report calculator threads."),
		kind:  prometheus.GaugeValue,
		parse: youtrack.ParseNumber,
		value: func(t *youtrack.Telemetry) youtrack.Value { return t.ReportCalculatorThreads },
	},
	{
		desc:  newDesc("notification_analyzer_threads", "Number of notification analyzer threads."),
		kind:  prometheus.GaugeValue,
		parse: youtrack.ParseNumber,
		value: func(t *youtrack.Telemetry) youtrack.Value { return t.NotificationAnalyzerThreads },
	},
}

func (s *telemetryScraper) Name() string { return "telemetry" }

// Scrape skips the attributes YouTrack does not return or that cannot be parsed, a missing value is not an error.
func (s *telemetryScraper) Scrape(ctx context.Context, emit func(prometheus.Metric)) error {
	t, err := s.client.Telemetry(ctx)
	if err != nil {
		return err
	}
	for _, m := range telemetryMetrics {
		raw := m.value(&t)
		if raw == "" {
			continue
		}
		v, err := m.parse(string(raw))
		if err != nil {
			s.logger.Debug("telemetry value skipped", "metric", m.desc.String(), "value", raw, "err", err)
			continue
		}
		emit(prometheus.MustNewConstMetric(m.desc, m.kind, v, m.labels...))
	}
	return nil
}

// backupScraper reads the database backup status and the list of backup files.
type backupScraper struct {
	client *youtrack.Client
}

var (
	backupInProgressDesc = newDesc("backup_in_progress", "Whether a database backup is running.")
	backupCancelledDesc  = newDesc("backup_cancelled", "Whether the last database backup was cancelled.")
	backupErrorDesc      = newDesc("backup_error", "Whether the last database backup failed.")
	backupErrorTimeDesc  = newDesc("backup_error_timestamp_seconds", "Unix time of the last database backup error.")
	backupFilesDesc      = newDesc("backup_files", "Number of database backup files.")
	backupFilesSizeDesc  = newDesc("backup_files_size_bytes", "Total size of the database backup files.")
	backupLastTimeDesc   = newDesc("backup_last_timestamp_seconds", "Unix time when the newest database backup file was created.")
	backupLastSizeDesc   = newDesc("backup_last_size_bytes", "Size of the newest database backup file.")
)

func (s *backupScraper) Name() string { return "backup" }

func (s *backupScraper) Scrape(ctx context.Context, emit func(prometheus.Metric)) error {
	var (
		wg               sync.WaitGroup
		status           youtrack.BackupStatus
		files            []youtrack.BackupFile
		statusErr, fsErr error
	)
	wg.Go(func() { status, statusErr = s.client.BackupStatus(ctx) })
	wg.Go(func() { files, fsErr = s.client.Backups(ctx) })
	wg.Wait()

	gauge := func(d *prometheus.Desc, v float64) { emit(prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v)) }
	if statusErr == nil {
		gauge(backupInProgressDesc, boolValue(status.BackupInProgress))
		gauge(backupCancelledDesc, boolValue(status.BackupCancelled))
		gauge(backupErrorDesc, boolValue(status.BackupError != nil))
		if status.BackupError != nil && status.BackupError.Date > 0 {
			gauge(backupErrorTimeDesc, float64(status.BackupError.Date)/1000)
		}
	}
	if fsErr == nil {
		var total int64
		var newest *youtrack.BackupFile
		for i, f := range files {
			total += f.Size
			if newest == nil || f.CreationDate > newest.CreationDate {
				newest = &files[i]
			}
		}
		gauge(backupFilesDesc, float64(len(files)))
		gauge(backupFilesSizeDesc, float64(total))
		if newest != nil {
			gauge(backupLastTimeDesc, float64(newest.CreationDate)/1000)
			gauge(backupLastSizeDesc, float64(newest.Size))
		}
	}
	return errors.Join(statusErr, fsErr)
}

// licenseScraper reports whether YouTrack shows a license error (expired license, user limit exceeded, ...).
type licenseScraper struct {
	client *youtrack.Client
}

var licenseValidDesc = newDesc("license_valid", "Whether the YouTrack license has no error.")

func (s *licenseScraper) Name() string { return "license" }

func (s *licenseScraper) Scrape(ctx context.Context, emit func(prometheus.Metric)) error {
	l, err := s.client.License(ctx)
	if err != nil {
		return err
	}
	emit(prometheus.MustNewConstMetric(licenseValidDesc, prometheus.GaugeValue, boolValue(l.Error == "")))
	return nil
}
