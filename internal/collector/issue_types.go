package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kinjelom/yt_exporter/internal/config"
	"github.com/kinjelom/yt_exporter/internal/youtrack"
)

const (
	// issueTypesRunTimeout limits a run of the issue_types collector, it is longer than the interval because loading
	// all issues of the projects (at the start, on a full resync) can take longer.
	issueTypesRunTimeout = 30 * time.Minute
	loadPageSize         = 500
	updatePageSize       = 100
	// updateOverlap is how far before the newest change seen the updates are read again, so that a change committed
	// after a newer one is not missed. Reading an issue again does not change the index.
	updateOverlap = 2 * time.Minute
)

// Reasons for loading all issues of a project.
const (
	reloadInitial  = "initial"  // the project is counted for the first time
	reloadResync   = "resync"   // full_resync_interval passed since the last load
	reloadMismatch = "mismatch" // the number of issues in YouTrack differs from the index
)

var (
	projectIssuesByTypeDesc = newDesc("project_issues_by_type",
		"Number of open (#Unresolved) or closed (#Resolved) issues in the project by issue type, "+
			"the type is empty for issues without one.", "project", "type", "status")
	projectReloadsDesc = newDesc("issue_types_project_reloads_total",
		"Number of loads of all issues of a project by the issue_types collector, by reason: initial, "+
			"resync (full_resync_interval) or mismatch (deleted or moved issues).", "reason")
	reloadReasons = []string{reloadInitial, reloadResync, reloadMismatch}
)

// issueTypesScraper counts the open and closed issues of every project that is not archived by issue type. It keeps
// an index of the issues in memory: all issues of a project are loaded once, then a run reads only the issues updated
// since the previous run, newest first. Deleted issues and issues moved to a project that is not counted do not show
// up in the updates, a count request per run compares the number of issues with the index and the projects that
// differ are loaded again. Every project is also loaded again every full_resync_interval, this corrects the changes
// that do not update the issues, e.g. a renamed type value.
//
// Scrape is not called concurrently, the state is kept between runs.
type issueTypesScraper struct {
	client *youtrack.Client
	cfg    config.IssueTypes
	logger *slog.Logger
	now    func() time.Time

	issues    map[string]indexedIssue    // by issue ID, nil until the first update point is known
	projects  map[string]*indexedProject // loaded projects by short name
	watermark int64                      // newest "updated" seen (Unix ms), the updates are read from here
	reloads   map[string]int
	missing   string // included projects not found, logged when it changes
}

type indexedIssue struct {
	project, issueType string
	resolved           bool
}

type indexedProject struct {
	types  []string // values of the type field, exported also without issues
	loaded time.Time
}

func newIssueTypesScraper(client *youtrack.Client, cfg config.IssueTypes, logger *slog.Logger) *issueTypesScraper {
	return &issueTypesScraper{
		client:   client,
		cfg:      cfg,
		logger:   logger,
		now:      time.Now,
		projects: map[string]*indexedProject{},
		reloads:  map[string]int{},
	}
}

func (s *issueTypesScraper) Name() string { return "issue_types" }

func (s *issueTypesScraper) Scrape(ctx context.Context, emit func(prometheus.Metric)) error {
	all, err := s.client.Projects(ctx)
	if err != nil {
		return err
	}
	selected := s.selectProjects(all)
	for name := range s.projects {
		if _, ok := selected[name]; !ok {
			s.dropProject(name) // archived, deleted, renamed or no longer included
		}
	}
	if len(selected) > 0 {
		if s.issues == nil {
			// the first updates are read from the newest change before the projects are loaded
			if s.watermark, err = s.newest(ctx, selected); err != nil {
				return err
			}
			s.issues = map[string]indexedIssue{}
		} else if err := s.update(ctx, selected); err != nil {
			return err
		}
	}

	stale := map[string]string{}
	for name := range selected {
		switch p, ok := s.projects[name]; {
		case !ok:
			stale[name] = reloadInitial
		case s.now().Sub(p.loaded) >= s.cfg.FullResyncInterval:
			stale[name] = reloadResync
		}
	}
	errs := s.reload(ctx, selected, stale)
	if err := s.verify(ctx, selected); err != nil {
		errs = append(errs, err)
	}
	s.collect(emit)
	return errors.Join(errs...)
}

// selectProjects returns the counted projects by short name: the projects that are not archived, only the included
// ones when the include list is set.
func (s *issueTypesScraper) selectProjects(all []youtrack.Project) map[string]youtrack.Project {
	selected := map[string]youtrack.Project{}
	for _, p := range all {
		if p.ShortName != "" && !p.Archived && (len(s.cfg.Include) == 0 || slices.Contains(s.cfg.Include, p.ShortName)) {
			selected[p.ShortName] = p
		}
	}
	var missing []string
	for _, name := range s.cfg.Include {
		if _, ok := selected[name]; !ok {
			missing = append(missing, name)
		}
	}
	if m := strings.Join(missing, ","); m != s.missing {
		s.missing = m
		if m != "" {
			s.logger.Warn("included projects not found or archived", "collector", s.Name(), "projects", missing)
		}
	}
	return selected
}

// scope returns the search query of the counted projects for the updates, "" when all projects are counted: the
// issues of projects that are not loaded are skipped by the index.
func (s *issueTypesScraper) scope(selected map[string]youtrack.Project) string {
	if len(s.cfg.Include) == 0 {
		return ""
	}
	return projectsQuery(slices.Sorted(maps.Keys(selected)))
}

func projectsQuery(names []string) string {
	return "project: {" + strings.Join(names, "}, {") + "}"
}

// newest returns the update time of the most recently updated issue, 0 when there are no issues.
func (s *issueTypesScraper) newest(ctx context.Context, selected map[string]youtrack.Project) (int64, error) {
	var newest int64
	query := strings.TrimSpace(s.scope(selected) + " sort by: updated desc")
	err := s.client.IssuePages(ctx, query, 1, []string{s.cfg.TypeField}, func(page []youtrack.Issue) bool {
		newest = page[0].Updated
		return false
	})
	return newest, err
}

// update applies the issues updated since the watermark, it reads them newest first until it gets past it.
func (s *issueTypesScraper) update(ctx context.Context, selected map[string]youtrack.Project) error {
	since := s.watermark - updateOverlap.Milliseconds()
	newest := s.watermark
	query := strings.TrimSpace(s.scope(selected) + " sort by: updated desc")
	err := s.client.IssuePages(ctx, query, updatePageSize, []string{s.cfg.TypeField}, func(page []youtrack.Issue) bool {
		for _, is := range page {
			newest = max(newest, is.Updated)
			s.apply(is)
		}
		return page[len(page)-1].Updated >= since
	})
	if err != nil {
		return err // the applied issues are read again in the next run
	}
	s.watermark = newest
	return nil
}

// apply stores the current state of an issue. An issue of a project that is not loaded is removed: it was moved out
// of the counted projects, or to a project that is loaded later and reads it.
func (s *issueTypesScraper) apply(is youtrack.Issue) {
	if is.Project == nil || s.projects[is.Project.ShortName] == nil {
		delete(s.issues, is.ID)
		return
	}
	issueType := ""
	for _, f := range is.CustomFields {
		if strings.EqualFold(f.Name, s.cfg.TypeField) {
			issueType = f.ValueName()
			break
		}
	}
	s.issues[is.ID] = indexedIssue{project: is.Project.ShortName, issueType: issueType, resolved: is.Resolved != nil}
}

// reload loads all issues of the stale projects (short name -> reason) and replaces their entries in the index. A
// project that fails keeps its previous entries, a project that was not loaded yet is tried again in the next run.
func (s *issueTypesScraper) reload(ctx context.Context, selected map[string]youtrack.Project, stale map[string]string) []error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for name, reason := range stale {
		// the client limits the requests in flight, a goroutine per project only queues them
		wg.Go(func() {
			types, issues, err := s.readProject(ctx, selected[name])
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("project %s: %w", name, err))
				return
			}
			s.dropProject(name)
			s.projects[name] = &indexedProject{types: types, loaded: s.now()}
			for _, is := range issues {
				s.apply(is)
			}
			s.reloads[reason]++
		})
	}
	wg.Wait()
	return errs
}

// readProject reads the values of the project's type field and all its issues.
func (s *issueTypesScraper) readProject(ctx context.Context, p youtrack.Project) ([]string, []youtrack.Issue, error) {
	fields, err := s.client.ProjectCustomFields(ctx, p.ID)
	if err != nil {
		return nil, nil, err
	}
	var issues []youtrack.Issue
	// oldest first: the issues created while reading are appended at the end and do not shift the pages
	query := projectsQuery([]string{p.ShortName}) + " sort by: created asc"
	err = s.client.IssuePages(ctx, query, loadPageSize, []string{s.cfg.TypeField}, func(page []youtrack.Issue) bool {
		issues = append(issues, page...)
		return true
	})
	if err != nil {
		return nil, nil, err
	}
	return s.typeValues(p, fields), issues, nil
}

// typeValues returns the values of the project's type field, none when the project has no type field with values:
// all its issues are counted without a type.
func (s *issueTypesScraper) typeValues(p youtrack.Project, fields []youtrack.ProjectCustomField) []string {
	for _, f := range fields {
		if !strings.EqualFold(f.Field.Name, s.cfg.TypeField) || f.Bundle == nil || len(f.Bundle.Values) == 0 {
			continue
		}
		types := make([]string, 0, len(f.Bundle.Values))
		for _, v := range f.Bundle.Values {
			types = append(types, v.Name)
		}
		return types
	}
	s.logger.Debug("project has no issue type field with values, all issues are counted without a type",
		"collector", s.Name(), "project", p.ShortName, "field", s.cfg.TypeField)
	return nil
}

func (s *issueTypesScraper) dropProject(name string) {
	maps.DeleteFunc(s.issues, func(_ string, is indexedIssue) bool { return is.project == name })
	delete(s.projects, name)
}

// verify compares the number of issues of the loaded projects in YouTrack with the index: deleted issues and issues
// moved to a project that is not counted do not show up in the updates. When they differ, one count per project
// finds the projects to load again.
func (s *issueTypesScraper) verify(ctx context.Context, selected map[string]youtrack.Project) error {
	if len(s.projects) == 0 {
		return nil
	}
	query := projectsQuery(slices.Sorted(maps.Keys(s.projects)))
	for attempt := 1; ; attempt++ {
		n, err := s.client.CountIssues(ctx, query)
		if err != nil {
			return err
		}
		if n == int64(len(s.issues)) {
			return nil
		}
		if attempt == 2 {
			break
		}
		// an issue created after the update is already counted, read the updates once more
		if err := s.update(ctx, selected); err != nil {
			return err
		}
	}

	indexed := map[string]int64{}
	for _, is := range s.issues {
		indexed[is.project]++
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		errs  []error
		stale = map[string]string{}
	)
	for name := range s.projects {
		wg.Go(func() {
			n, err := s.client.CountIssues(ctx, projectsQuery([]string{name}))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				errs = append(errs, fmt.Errorf("project %s: %w", name, err))
			case n != indexed[name]:
				stale[name] = reloadMismatch
			}
		})
	}
	wg.Wait()
	if len(stale) > 0 {
		s.logger.Info("number of issues differs from the index (deleted or moved issues), loading the projects again",
			"collector", s.Name(), "projects", slices.Sorted(maps.Keys(stale)))
	}
	return errors.Join(append(errs, s.reload(ctx, selected, stale)...)...)
}

func (s *issueTypesScraper) collect(emit func(prometheus.Metric)) {
	type key struct {
		project, issueType string
		resolved           bool
	}
	counts := map[key]int{}
	// every type of a loaded project is exported, also without issues
	for name, p := range s.projects {
		for _, t := range append([]string{""}, p.types...) {
			counts[key{name, t, false}], counts[key{name, t, true}] = 0, 0
		}
	}
	for _, is := range s.issues {
		counts[key{is.project, is.issueType, is.resolved}]++
	}
	for k, n := range counts {
		status := "open"
		if k.resolved {
			status = "closed"
		}
		emit(prometheus.MustNewConstMetric(projectIssuesByTypeDesc, prometheus.GaugeValue, float64(n),
			k.project, k.issueType, status))
	}
	for _, reason := range reloadReasons {
		emit(prometheus.MustNewConstMetric(projectReloadsDesc, prometheus.CounterValue, float64(s.reloads[reason]), reason))
	}
}
