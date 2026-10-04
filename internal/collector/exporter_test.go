package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/promslog"

	"github.com/kinjelom/yt_exporter/internal/config"
	"github.com/kinjelom/yt_exporter/internal/youtrack"
)

// fakeYouTrack answers like YouTrack 2026.2, issue counts come from the counts map (query -> count) or else from the
// issues of the store, which also answers the issue searches (store may be nil).
func fakeYouTrack(t *testing.T, counts map[string]int, store *issueStore) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	reply := func(path, body string) {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("$skip") > "0" {
				_, _ = w.Write([]byte("[]"))
				return
			}
			_, _ = w.Write([]byte(body))
		})
	}
	reply("/api/config", `{"version":"2026.2","build":"2026.2.12345","$type":"ApplicationConfig"}`)
	reply("/api/admin/telemetry", `{
		"availableProcessors": 8,
		"availableMemory": "5.0 GB",
		"allocatedMemory": "2.5 GB",
		"usedMemory": "1.25 GB",
		"uptime": "644 hours, 19 minutes, 10 seconds and 121 milliseconds",
		"startedTime": 1610613083628,
		"databaseBackgroundThreads": 2,
		"pendingAsyncJobs": 3,
		"cachedResultsCountInDBQueriesCache": 1500,
		"databaseQueriesCacheHitRate": "93.5%",
		"blobStringsCacheHitRate": "40%",
		"totalTransactions": 123456,
		"transactionsPerSecond": "0.25",
		"requestsPerSecond": "1.5",
		"databaseSize": "5.0 MB",
		"fullDatabaseSize": "10.0 MB",
		"textIndexSize": "0.5 MB",
		"onlineUsers": {"users": 12, "$type": "OnlineUsers"},
		"reportCalculatorThreads": 1,
		"notificationAnalyzerThreads": "n/a",
		"$type": "Telemetry"}`)
	reply("/api/admin/databaseBackup/settings/backupStatus",
		`{"backupInProgress":false,"backupCancelled":false,"backupError":{"date":1700000000000,"errorMessage":"disk full"}}`)
	reply("/api/admin/databaseBackup/backups",
		`[{"name":"a.tar.gz","size":100,"creationDate":1690000000000},{"name":"b.tar.gz","size":200,"creationDate":1695000000000}]`)
	mux.HandleFunc("GET /api/admin/globalSettings/license", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"Forbidden"}`))
	})
	reply("/api/admin/projects", `[
		{"id":"0-1","shortName":"ABC","name":"Alpha Beta","archived":false},
		{"id":"0-2","shortName":"OLD","name":"Old one","archived":true},
		{"id":"0-3","shortName":"SKIP","name":"Skipped","archived":false}]`)
	reply("/api/admin/projects/0-1/customFields", `[
		{"field":{"name":"Priority"},"bundle":{"values":[{"name":"Normal"},{"name":"Critical"}]}},
		{"field":{"name":"Type"},"bundle":{"values":[{"name":"Bug"},{"name":"Feature Request"}]}},
		{"field":{"name":"Assignee"},"bundle":{}}]`)
	reply("/api/admin/projects/0-3/customFields", `[{"field":{"name":"State"},"bundle":{"values":[{"name":"Open"}]}}]`)
	reply("/api/users", `[
		{"id":"1","login":"guest","guest":true,"banned":false},
		{"id":"2","login":"a","banned":false,"userType":{"id":"STANDARD_USER"}},
		{"id":"3","login":"b","banned":true,"userType":{"id":"STANDARD_USER"}},
		{"id":"4","login":"c","banned":false,"userType":{"id":"AGENT"}},
		{"id":"5","login":"d","banned":false}]`)
	mux.HandleFunc("POST /api/issuesGetter/count", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Query string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		n, ok := counts[body.Query]
		if !ok && store != nil {
			issues, err := store.find(body.Query)
			n, ok = len(issues), err == nil
		}
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":"bad query %q"}`, body.Query)
			return
		}
		fmt.Fprintf(w, `{"count":%d}`, n)
	})
	if store != nil {
		mux.HandleFunc("GET /api/issues", store.serveIssues(t))
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestExporter(t *testing.T, url string, cfg config.Collectors) *Exporter {
	t.Helper()
	client, err := youtrack.New(youtrack.Options{URL: url, Token: "perm:test", Timeout: 5 * time.Second, MaxConcurrentRequests: 4})
	if err != nil {
		t.Fatal(err)
	}
	e := New(client, cfg, promslog.NewNopLogger())
	for _, c := range e.background {
		c.refresh(context.Background())
	}
	return e
}

func TestExporter(t *testing.T) {
	srv := fakeYouTrack(t, map[string]int{
		"project: {ABC}":                 120,
		"project: {ABC} #Unresolved":     17,
		"#Unresolved Priority: Critical": 3,
		"":                               500,
	}, nil)
	cfg := config.Default().Collectors
	cfg.Projects.Exclude = []string{"SKIP"}
	cfg.Queries.Items = []config.Query{
		{Name: "critical", Query: "#Unresolved Priority: Critical", Labels: map[string]string{"team": "ops"}},
		{Name: "all", Query: ""},
	}
	e := newTestExporter(t, srv.URL, cfg)

	expected := `
# HELP youtrack_up Whether the YouTrack REST API answered the version request of this scrape.
# TYPE youtrack_up gauge
youtrack_up 1
# HELP youtrack_build_info YouTrack version and build, the value is always 1.
# TYPE youtrack_build_info gauge
youtrack_build_info{build="2026.2.12345",version="2026.2"} 1
# HELP youtrack_memory_bytes Memory of the YouTrack JVM: available (maximum heap), allocated or used.
# TYPE youtrack_memory_bytes gauge
youtrack_memory_bytes{type="allocated"} 2.68435456e+09
youtrack_memory_bytes{type="available"} 5.36870912e+09
youtrack_memory_bytes{type="used"} 1.34217728e+09
# HELP youtrack_start_time_seconds Unix time when YouTrack started.
# TYPE youtrack_start_time_seconds gauge
youtrack_start_time_seconds 1.610613083628e+09
# HELP youtrack_database_size_bytes Size of the YouTrack database without BLOBs.
# TYPE youtrack_database_size_bytes gauge
youtrack_database_size_bytes 5.24288e+06
# HELP youtrack_database_queries_cache_hit_ratio Hit ratio (0-1) of the database queries cache.
# TYPE youtrack_database_queries_cache_hit_ratio gauge
youtrack_database_queries_cache_hit_ratio 0.935
# HELP youtrack_transactions_total Number of database transactions since YouTrack started.
# TYPE youtrack_transactions_total counter
youtrack_transactions_total 123456
# HELP youtrack_online_users Number of users online.
# TYPE youtrack_online_users gauge
youtrack_online_users 12
# HELP youtrack_backup_error Whether the last database backup failed.
# TYPE youtrack_backup_error gauge
youtrack_backup_error 1
# HELP youtrack_backup_files Number of database backup files.
# TYPE youtrack_backup_files gauge
youtrack_backup_files 2
# HELP youtrack_backup_last_timestamp_seconds Unix time when the newest database backup file was created.
# TYPE youtrack_backup_last_timestamp_seconds gauge
youtrack_backup_last_timestamp_seconds 1.695e+09
# HELP youtrack_backup_last_size_bytes Size of the newest database backup file.
# TYPE youtrack_backup_last_size_bytes gauge
youtrack_backup_last_size_bytes 200
# HELP youtrack_project_info YouTrack project, the value is always 1.
# TYPE youtrack_project_info gauge
youtrack_project_info{archived="false",project="ABC",project_name="Alpha Beta"} 1
# HELP youtrack_project_issues Number of issues in the project.
# TYPE youtrack_project_issues gauge
youtrack_project_issues{project="ABC"} 120
# HELP youtrack_project_issues_unresolved Number of unresolved issues (#Unresolved) in the project.
# TYPE youtrack_project_issues_unresolved gauge
youtrack_project_issues_unresolved{project="ABC"} 17
# HELP youtrack_query_issues Number of issues matching a configured YouTrack search query.
# TYPE youtrack_query_issues gauge
youtrack_query_issues{name="all",team=""} 500
youtrack_query_issues{name="critical",team="ops"} 3
# HELP youtrack_users Number of user accounts (guest excluded) by user type (YouTrack 2026.2+, otherwise unknown) and ban state.
# TYPE youtrack_users gauge
youtrack_users{banned="false",type="agent"} 1
youtrack_users{banned="false",type="reporter"} 0
youtrack_users{banned="false",type="standard_user"} 1
youtrack_users{banned="false",type="unknown"} 1
youtrack_users{banned="true",type="agent"} 0
youtrack_users{banned="true",type="reporter"} 0
youtrack_users{banned="true",type="standard_user"} 1
# HELP youtrack_scrape_collector_success Whether the last run of the collector succeeded.
# TYPE youtrack_scrape_collector_success gauge
youtrack_scrape_collector_success{collector="backup"} 1
youtrack_scrape_collector_success{collector="info"} 1
youtrack_scrape_collector_success{collector="license"} 0
youtrack_scrape_collector_success{collector="projects"} 1
youtrack_scrape_collector_success{collector="queries"} 1
youtrack_scrape_collector_success{collector="telemetry"} 1
youtrack_scrape_collector_success{collector="users"} 1
`
	names := []string{
		"youtrack_up", "youtrack_build_info", "youtrack_memory_bytes", "youtrack_start_time_seconds",
		"youtrack_database_size_bytes", "youtrack_database_queries_cache_hit_ratio", "youtrack_transactions_total",
		"youtrack_online_users", "youtrack_backup_error", "youtrack_backup_files", "youtrack_backup_last_timestamp_seconds",
		"youtrack_backup_last_size_bytes", "youtrack_project_info", "youtrack_project_issues",
		"youtrack_project_issues_unresolved", "youtrack_query_issues", "youtrack_users", "youtrack_scrape_collector_success",
	}
	if err := testutil.CollectAndCompare(e, strings.NewReader(expected), names...); err != nil {
		t.Error(err)
	}
	// an unparsable telemetry value ("n/a") is skipped, the license (403) collector reports failure only
	if n := testutil.CollectAndCount(e, "youtrack_notification_analyzer_threads", "youtrack_license_valid"); n != 0 {
		t.Errorf("got %d unexpected metrics", n)
	}
	problems, err := testutil.CollectAndLint(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("lint %s: %s", p.Metric, p.Text)
	}
}

func TestExporterDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	cfg := config.Default().Collectors
	e := newTestExporter(t, srv.URL, cfg)

	expected := `
# HELP youtrack_up Whether the YouTrack REST API answered the version request of this scrape.
# TYPE youtrack_up gauge
youtrack_up 0
`
	if err := testutil.CollectAndCompare(e, strings.NewReader(expected), "youtrack_up"); err != nil {
		t.Error(err)
	}
	// no background result yet: neither project nor user metrics, only the failure
	if n := testutil.CollectAndCount(e, "youtrack_project_issues", "youtrack_users",
		"youtrack_scrape_collector_last_success_timestamp_seconds"); n != 0 {
		t.Errorf("got %d unexpected metrics", n)
	}
}
