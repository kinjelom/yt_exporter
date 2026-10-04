// Package collector turns YouTrack REST API data into Prometheus metrics.
//
// Cheap endpoints (version, telemetry, backups, license) are read on every scrape. Issue and user counts can take
// many requests, they are refreshed in the background every interval and scrapes return the last result.
package collector

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kinjelom/yt_exporter/internal/config"
	"github.com/kinjelom/yt_exporter/internal/youtrack"
)

const namespace = "youtrack"

// scraper reads one area of YouTrack. emit is safe for concurrent use.
type scraper interface {
	Name() string
	Scrape(ctx context.Context, emit func(prometheus.Metric)) error
}

var (
	upDesc = prometheus.NewDesc(namespace+"_up",
		"Whether the YouTrack REST API answered the version request of this scrape.", nil, nil)
	collectorSuccessDesc = prometheus.NewDesc(namespace+"_scrape_collector_success",
		"Whether the last run of the collector succeeded.", []string{"collector"}, nil)
	collectorDurationDesc = prometheus.NewDesc(namespace+"_scrape_collector_duration_seconds",
		"Duration of the last run of the collector.", []string{"collector"}, nil)
	collectorLastSuccessDesc = prometheus.NewDesc(namespace+"_scrape_collector_last_success_timestamp_seconds",
		"Unix time of the last successful run of a background collector.", []string{"collector"}, nil)
)

// Exporter is a prometheus.Collector for one YouTrack instance.
type Exporter struct {
	logger     *slog.Logger
	info       scraper
	onScrape   []scraper
	background []*cached
}

func New(client *youtrack.Client, cfg config.Collectors, logger *slog.Logger) *Exporter {
	e := &Exporter{logger: logger, info: &infoScraper{client: client}}
	e.onScrape = append(e.onScrape, e.info)
	if cfg.Telemetry.Enabled {
		e.onScrape = append(e.onScrape, &telemetryScraper{client: client, logger: logger})
	}
	if cfg.Backup.Enabled {
		e.onScrape = append(e.onScrape, &backupScraper{client: client})
	}
	if cfg.License.Enabled {
		e.onScrape = append(e.onScrape, &licenseScraper{client: client})
	}
	if cfg.Projects.Enabled {
		e.background = append(e.background, newCached(&projectsScraper{client: client, cfg: cfg.Projects, logger: logger},
			cfg.Projects.Interval, logger))
	}
	if cfg.IssueTypes.Enabled {
		c := newCached(newIssueTypesScraper(client, cfg.IssueTypes, logger), cfg.IssueTypes.Interval, logger)
		// loading all issues at the start or a full resync may take longer than the interval
		c.timeout = max(cfg.IssueTypes.Interval, issueTypesRunTimeout)
		e.background = append(e.background, c)
	}
	if cfg.Users.Enabled {
		e.background = append(e.background, newCached(&usersScraper{client: client}, cfg.Users.Interval, logger))
	}
	if len(cfg.Queries.Items) > 0 {
		e.background = append(e.background, newCached(newQueriesScraper(client, cfg.Queries.Items),
			cfg.Queries.Interval, logger))
	}
	return e
}

// Start runs the background collectors until ctx is done.
func (e *Exporter) Start(ctx context.Context) {
	for _, c := range e.background {
		go c.run(ctx)
	}
}

// Describe sends no descriptors: the exporter is an unchecked collector, the set of metrics depends on YouTrack.
func (e *Exporter) Describe(chan<- *prometheus.Desc) {}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	emit := func(m prometheus.Metric) { ch <- m }
	var wg sync.WaitGroup
	for _, s := range e.onScrape {
		wg.Go(func() {
			start := time.Now()
			err := s.Scrape(context.Background(), emit)
			ch <- prometheus.MustNewConstMetric(collectorDurationDesc, prometheus.GaugeValue,
				time.Since(start).Seconds(), s.Name())
			ch <- prometheus.MustNewConstMetric(collectorSuccessDesc, prometheus.GaugeValue, boolValue(err == nil), s.Name())
			if s == e.info {
				ch <- prometheus.MustNewConstMetric(upDesc, prometheus.GaugeValue, boolValue(err == nil))
			}
			if err != nil {
				logError(e.logger, s.Name(), err)
			}
		})
	}
	for _, c := range e.background {
		c.collect(ch)
	}
	wg.Wait()
}

func logError(logger *slog.Logger, collector string, err error) {
	if youtrack.IsPermissionError(err) {
		logger.Warn("collector failed, check the permissions of the token's user or disable the collector",
			"collector", collector, "err", err)
		return
	}
	logger.Warn("collector failed", "collector", collector, "err", err)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// cached runs a scraper in the background and keeps its last result.
type cached struct {
	scraper  scraper
	interval time.Duration
	timeout  time.Duration // of one run
	logger   *slog.Logger

	mu          sync.RWMutex
	ran         bool
	metrics     []prometheus.Metric
	success     bool
	duration    time.Duration
	lastSuccess time.Time
}

func newCached(s scraper, interval time.Duration, logger *slog.Logger) *cached {
	return &cached{scraper: s, interval: interval, timeout: interval, logger: logger}
}

func (c *cached) run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		c.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// refresh replaces the kept metrics with the result of one run. A run that failed without any metric keeps the
// previous result, a partial one (e.g. one project not readable) replaces it; the success metric tells them apart.
func (c *cached) refresh(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()

	var (
		mu      sync.Mutex
		metrics []prometheus.Metric
	)
	start := time.Now()
	err := c.scraper.Scrape(ctx, func(m prometheus.Metric) {
		mu.Lock()
		metrics = append(metrics, m)
		mu.Unlock()
	})
	duration := time.Since(start)
	if parent.Err() != nil {
		return
	}
	if err != nil {
		logError(c.logger, c.scraper.Name(), err)
	} else {
		c.logger.Debug("collector refreshed", "collector", c.scraper.Name(), "metrics", len(metrics), "duration", duration)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.ran, c.success, c.duration = true, err == nil, duration
	if err == nil {
		c.lastSuccess = time.Now()
	}
	if err == nil || len(metrics) > 0 {
		c.metrics = metrics
	}
}

func (c *cached) collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.ran {
		return
	}
	name := c.scraper.Name()
	for _, m := range c.metrics {
		ch <- m
	}
	ch <- prometheus.MustNewConstMetric(collectorSuccessDesc, prometheus.GaugeValue, boolValue(c.success), name)
	ch <- prometheus.MustNewConstMetric(collectorDurationDesc, prometheus.GaugeValue, c.duration.Seconds(), name)
	if !c.lastSuccess.IsZero() {
		ch <- prometheus.MustNewConstMetric(collectorLastSuccessDesc, prometheus.GaugeValue,
			float64(c.lastSuccess.UnixNano())/1e9, name)
	}
}
