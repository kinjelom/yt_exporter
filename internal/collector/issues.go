package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kinjelom/yt_exporter/internal/config"
	"github.com/kinjelom/yt_exporter/internal/youtrack"
)

// projectsScraper counts all and unresolved issues of every project, two count requests per project.
type projectsScraper struct {
	client *youtrack.Client
	cfg    config.Projects
	logger *slog.Logger
}

var (
	projectInfoDesc = newDesc("project_info",
		"YouTrack project, the value is always 1.", "project", "project_name", "archived")
	projectIssuesDesc = newDesc("project_issues",
		"Number of issues in the project.", "project")
	projectUnresolvedDesc = newDesc("project_issues_unresolved",
		"Number of unresolved issues (#Unresolved) in the project.", "project")
)

func (s *projectsScraper) Name() string { return "projects" }

func (s *projectsScraper) Scrape(ctx context.Context, emit func(prometheus.Metric)) error {
	projects, err := s.client.Projects(ctx)
	if err != nil {
		return err
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	count := func(project, query string, desc *prometheus.Desc) {
		n, err := s.client.CountIssues(ctx, query)
		if err != nil {
			mu.Lock()
			errs = append(errs, fmt.Errorf("project %s: %w", project, err))
			mu.Unlock()
			return
		}
		emit(prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(n), project))
	}
	for _, p := range projects {
		if !s.selected(p) {
			continue
		}
		emit(prometheus.MustNewConstMetric(projectInfoDesc, prometheus.GaugeValue, 1,
			p.ShortName, p.Name, strconv.FormatBool(p.Archived)))
		query := fmt.Sprintf("project: {%s}", p.ShortName)
		// the client limits the requests in flight, a goroutine per count only queues them
		wg.Go(func() { count(p.ShortName, query, projectIssuesDesc) })
		wg.Go(func() { count(p.ShortName, query+" #Unresolved", projectUnresolvedDesc) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (s *projectsScraper) selected(p youtrack.Project) bool {
	if p.ShortName == "" || p.Archived && !s.cfg.IncludeArchived {
		return false
	}
	if len(s.cfg.Include) > 0 && !slices.Contains(s.cfg.Include, p.ShortName) {
		return false
	}
	return !slices.Contains(s.cfg.Exclude, p.ShortName)
}

// queriesScraper counts the issues matching the configured search queries.
type queriesScraper struct {
	client     *youtrack.Client
	queries    []config.Query
	labelNames []string
	desc       *prometheus.Desc
}

// newQueriesScraper builds one metric family for all queries: its labels are "name" and the union of the
// configured labels, a query without one of them gets an empty value.
func newQueriesScraper(client *youtrack.Client, queries []config.Query) *queriesScraper {
	set := map[string]bool{}
	for _, q := range queries {
		for l := range q.Labels {
			set[l] = true
		}
	}
	labelNames := make([]string, 0, len(set))
	for l := range set {
		labelNames = append(labelNames, l)
	}
	sort.Strings(labelNames)
	return &queriesScraper{
		client:     client,
		queries:    queries,
		labelNames: labelNames,
		desc: newDesc("query_issues", "Number of issues matching a configured YouTrack search query.",
			append([]string{"name"}, labelNames...)...),
	}
}

func (s *queriesScraper) Name() string { return "queries" }

func (s *queriesScraper) Scrape(ctx context.Context, emit func(prometheus.Metric)) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, q := range s.queries {
		wg.Go(func() {
			n, err := s.client.CountIssues(ctx, strings.TrimSpace(q.Query))
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("query %s: %w", q.Name, err))
				mu.Unlock()
				return
			}
			values := []string{q.Name}
			for _, l := range s.labelNames {
				values = append(values, q.Labels[l])
			}
			emit(prometheus.MustNewConstMetric(s.desc, prometheus.GaugeValue, float64(n), values...))
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}
