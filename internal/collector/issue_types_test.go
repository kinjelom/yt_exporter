package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kinjelom/yt_exporter/internal/config"
)

type fakeIssue struct {
	id, project, issueType string
	resolved               bool
	created, updated       int64
}

// issueStore holds the issues of the fake YouTrack: projects ABC and OLD (archived) have the Type field, SKIP has not.
type issueStore struct {
	mu      sync.Mutex
	clock   int64
	seq     int
	issues  map[string]*fakeIssue
	queries []string // of the issue searches and counts, in order
}

func newIssueStore() *issueStore {
	return &issueStore{clock: 1_700_000_000_000, issues: map[string]*fakeIssue{}}
}

// tick moves the clock by more than the update overlap, so that a run reads only the changes since the previous one.
func (s *issueStore) tick() int64 {
	s.clock += (5 * time.Minute).Milliseconds()
	return s.clock
}

func (s *issueStore) add(project, issueType string, resolved bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	id := "2-" + strconv.Itoa(s.seq)
	now := s.tick()
	s.issues[id] = &fakeIssue{id: id, project: project, issueType: issueType, resolved: resolved, created: now, updated: now}
	return id
}

// change modifies an issue and its update time, a silent change keeps the update time (like a renamed type value).
func (s *issueStore) change(id string, silent bool, f func(*fakeIssue)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s.issues[id])
	if !silent {
		s.issues[id].updated = s.tick()
	}
}

func (s *issueStore) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.issues, id)
}

func (s *issueStore) takeQueries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.queries
	s.queries = nil
	return q
}

var (
	fakeQueryRe   = regexp.MustCompile(`^(project: \{[^}]+\}(?:, \{[^}]+\})*)? ?(sort by: (?:created asc|updated desc))?$`)
	fakeProjectRe = regexp.MustCompile(`\{([^}]+)\}`)
)

// find returns copies of the issues matching the subset of the query language the collector sends.
func (s *issueStore) find(query string) ([]fakeIssue, error) {
	m := fakeQueryRe.FindStringSubmatch(query)
	if m == nil {
		return nil, fmt.Errorf("unsupported query %q", query)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries = append(s.queries, query)
	projects := map[string]bool{}
	for _, p := range fakeProjectRe.FindAllStringSubmatch(m[1], -1) {
		projects[p[1]] = true
	}
	var found []fakeIssue
	for _, is := range s.issues {
		if len(projects) == 0 || projects[is.project] {
			found = append(found, *is)
		}
	}
	slices.SortFunc(found, func(a, b fakeIssue) int {
		if m[2] == "sort by: updated desc" && a.updated != b.updated {
			return int(b.updated - a.updated)
		}
		return int(a.created - b.created)
	})
	return found, nil
}

func (s *issueStore) serveIssues(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if !slices.Equal(q["customFields"], []string{"Type"}) || !strings.Contains(q.Get("fields"), "resolved") {
			t.Errorf("issues request %s", r.URL)
		}
		found, err := s.find(q.Get("query"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":%q}`, err)
			return
		}
		skip, _ := strconv.Atoi(q.Get("$skip"))
		top, _ := strconv.Atoi(q.Get("$top"))
		page := []map[string]any{}
		for _, is := range found[min(skip, len(found)):min(skip+top, len(found))] {
			fields := []map[string]any{}
			if is.project != "SKIP" {
				var value any
				if is.issueType != "" {
					value = map[string]string{"name": is.issueType, "$type": "EnumBundleElement"}
				}
				fields = append(fields, map[string]any{"name": "Type", "value": value, "$type": "SingleEnumIssueCustomField"})
			}
			var resolved any
			if is.resolved {
				resolved = is.created
			}
			page = append(page, map[string]any{"id": is.id, "updated": is.updated, "resolved": resolved,
				"project": map[string]string{"shortName": is.project, "$type": "Project"}, "customFields": fields})
		}
		_ = json.NewEncoder(w).Encode(page)
	}
}

// newIssueTypesExporter returns an exporter with only the issue_types collector, after its first run.
func newIssueTypesExporter(t *testing.T, store *issueStore, include ...string) (*Exporter, *cached) {
	t.Helper()
	srv := fakeYouTrack(t, nil, store)
	cfg := config.Collectors{IssueTypes: config.Default().Collectors.IssueTypes}
	cfg.IssueTypes.Enabled = true
	cfg.IssueTypes.Include = include
	e := newTestExporter(t, srv.URL, cfg)
	return e, e.background[0]
}

// series returns the values of a metric by its label values, joined with "/" in the order of the label names.
func series(t *testing.T, e *Exporter, name string) map[string]float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(e)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			var values []string
			for _, l := range m.GetLabel() {
				values = append(values, l.GetValue())
			}
			got[strings.Join(values, "/")] = m.GetGauge().GetValue() + m.GetCounter().GetValue()
		}
	}
	return got
}

func checkSeries(t *testing.T, e *Exporter, name string, want map[string]float64) {
	t.Helper()
	if got := series(t, e, name); !maps.Equal(got, want) {
		t.Errorf("%s\n got: %v\nwant: %v", name, got, want)
	}
}

// sampleIssues fills a store, 120 old closed bugs make the updates longer than one page.
func sampleIssues() (*issueStore, map[string]string) {
	s := newIssueStore()
	oldest := s.add("ABC", "Bug", true)
	for range 119 {
		s.add("ABC", "Bug", true)
	}
	ids := map[string]string{
		"oldest":     oldest,
		"open bug":   s.add("ABC", "Bug", false),
		"open bug 2": s.add("ABC", "Bug", false),
		"closed bug": s.add("ABC", "Bug", true),
		"feature":    s.add("ABC", "Feature Request", false),
		"untyped":    s.add("ABC", "", false),
		"skip":       s.add("SKIP", "", false),
		"skip 2":     s.add("SKIP", "", false),
		"skip 3":     s.add("SKIP", "", true),
	}
	s.add("OLD", "Bug", false)
	return s, ids
}

func TestIssueTypes(t *testing.T) {
	store, _ := sampleIssues()
	e, _ := newIssueTypesExporter(t, store)

	// OLD is archived, SKIP has no Type field: all its issues are counted without a type
	expected := `
# HELP youtrack_project_issues_by_type Number of open (#Unresolved) or closed (#Resolved) issues in the project by issue type, the type is empty for issues without one.
# TYPE youtrack_project_issues_by_type gauge
youtrack_project_issues_by_type{project="ABC",status="closed",type=""} 0
youtrack_project_issues_by_type{project="ABC",status="closed",type="Bug"} 121
youtrack_project_issues_by_type{project="ABC",status="closed",type="Feature Request"} 0
youtrack_project_issues_by_type{project="ABC",status="open",type=""} 1
youtrack_project_issues_by_type{project="ABC",status="open",type="Bug"} 2
youtrack_project_issues_by_type{project="ABC",status="open",type="Feature Request"} 1
youtrack_project_issues_by_type{project="SKIP",status="closed",type=""} 1
youtrack_project_issues_by_type{project="SKIP",status="open",type=""} 2
# HELP youtrack_issue_types_project_reloads_total Number of loads of all issues of a project by the issue_types collector, by reason: initial, resync (full_resync_interval) or mismatch (deleted or moved issues).
# TYPE youtrack_issue_types_project_reloads_total counter
youtrack_issue_types_project_reloads_total{reason="initial"} 2
youtrack_issue_types_project_reloads_total{reason="mismatch"} 0
youtrack_issue_types_project_reloads_total{reason="resync"} 0
# HELP youtrack_scrape_collector_success Whether the last run of the collector succeeded.
# TYPE youtrack_scrape_collector_success gauge
youtrack_scrape_collector_success{collector="info"} 1
youtrack_scrape_collector_success{collector="issue_types"} 1
`
	if err := testutil.CollectAndCompare(e, strings.NewReader(expected), "youtrack_project_issues_by_type",
		"youtrack_issue_types_project_reloads_total", "youtrack_scrape_collector_success"); err != nil {
		t.Error(err)
	}
	problems, err := testutil.CollectAndLint(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("lint %s: %s", p.Metric, p.Text)
	}
	want := []string{
		"sort by: updated desc",
		"project: {ABC} sort by: created asc",
		"project: {SKIP} sort by: created asc",
		"project: {ABC}, {SKIP}",
	}
	if got := store.takeQueries(); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
		t.Errorf("queries of the first run:\n got: %q\nwant: %q", got, want)
	}

	// the include list narrows the projects and the searches, archived ones stay skipped
	e, _ = newIssueTypesExporter(t, store, "SKIP", "OLD")
	checkSeries(t, e, "youtrack_project_issues_by_type", map[string]float64{"SKIP/closed/": 1, "SKIP/open/": 2})
	for _, q := range store.takeQueries() {
		if !strings.HasPrefix(q, "project: {SKIP}") {
			t.Errorf("query %q is not limited to SKIP", q)
		}
	}
}

func TestIssueTypesIndex(t *testing.T) {
	store, ids := sampleIssues()
	e, c := newIssueTypesExporter(t, store)
	scraper := c.scraper.(*issueTypesScraper)
	store.takeQueries()
	refresh := func() {
		t.Helper()
		c.refresh(context.Background())
		if !c.success {
			t.Fatal("the run failed")
		}
	}
	const metric = "youtrack_project_issues_by_type"

	t.Run("updates", func(t *testing.T) {
		store.change(ids["open bug"], false, func(is *fakeIssue) { is.resolved = true })
		store.change(ids["feature"], false, func(is *fakeIssue) { is.issueType = "Bug" })
		store.change(ids["closed bug"], false, func(is *fakeIssue) { is.project = "SKIP" })
		store.add("ABC", "", false)
		refresh()
		checkSeries(t, e, metric, map[string]float64{
			"ABC/closed/": 0, "ABC/closed/Bug": 121, "ABC/closed/Feature Request": 0,
			"ABC/open/": 2, "ABC/open/Bug": 2, "ABC/open/Feature Request": 0,
			"SKIP/closed/": 2, "SKIP/open/": 2,
		})
		// one page of updates (it reaches the previous run) and one count, no project is loaded
		want := []string{"sort by: updated desc", "project: {ABC}, {SKIP}"}
		if got := store.takeQueries(); !slices.Equal(got, want) {
			t.Errorf("queries:\n got: %q\nwant: %q", got, want)
		}
	})

	t.Run("deleted issue", func(t *testing.T) {
		store.remove(ids["skip"])
		refresh()
		checkSeries(t, e, metric, map[string]float64{
			"ABC/closed/": 0, "ABC/closed/Bug": 121, "ABC/closed/Feature Request": 0,
			"ABC/open/": 2, "ABC/open/Bug": 2, "ABC/open/Feature Request": 0,
			"SKIP/closed/": 2, "SKIP/open/": 1,
		})
		// the count differs twice, a count per project finds SKIP, only SKIP is loaded again
		got := store.takeQueries()
		if !slices.Contains(got, "project: {SKIP} sort by: created asc") || slices.Contains(got, "project: {ABC} sort by: created asc") {
			t.Errorf("queries: %q", got)
		}
		checkSeries(t, e, "youtrack_issue_types_project_reloads_total",
			map[string]float64{"initial": 2, "mismatch": 1, "resync": 0})
	})

	t.Run("full resync", func(t *testing.T) {
		// a change that does not update an issue is not seen until the projects are loaded again (the issue is
		// older than the page of updates, the issues on it are applied as they are now)
		store.change(ids["oldest"], true, func(is *fakeIssue) { is.resolved = false })
		refresh()
		if got := series(t, e, metric)["ABC/open/Bug"]; got != 2 {
			t.Errorf("open bugs before the resync = %v, want 2 (not seen yet)", got)
		}
		scraper.now = func() time.Time { return time.Now().Add(scraper.cfg.FullResyncInterval) }
		refresh()
		if got := series(t, e, metric)["ABC/open/Bug"]; got != 3 {
			t.Errorf("open bugs after the resync = %v, want 3", got)
		}
		checkSeries(t, e, "youtrack_issue_types_project_reloads_total",
			map[string]float64{"initial": 2, "mismatch": 1, "resync": 2})
	})

	t.Run("project no longer counted", func(t *testing.T) {
		scraper.cfg.Include = []string{"ABC"}
		refresh()
		for k := range series(t, e, metric) {
			if !strings.HasPrefix(k, "ABC/") {
				t.Errorf("series %s of a project that is not counted", k)
			}
		}
	})
}
