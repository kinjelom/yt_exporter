package youtrack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Options{URL: srv.URL + "/youtrack/", Token: "perm:test", Timeout: 5 * time.Second, MaxConcurrentRequests: 2})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCountIssuesRetriesUntilReady(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/youtrack/api/issuesGetter/count" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer perm:test" {
			t.Errorf("Authorization = %q", got)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["query"] != "project: {ABC} #Unresolved" {
			t.Errorf("body = %v, err %v", body, err)
		}
		count := -1
		if calls.Add(1) == 3 {
			count = 42
		}
		fmt.Fprintf(w, `{"count":%d,"$type":"IssueCountResponse"}`, count)
	}))

	n, err := c.CountIssues(context.Background(), "project: {ABC} #Unresolved")
	if err != nil {
		t.Fatal(err)
	}
	if n != 42 || calls.Load() != 3 {
		t.Errorf("count = %d after %d calls, want 42 after 3", n, calls.Load())
	}
}

func TestIssuePages(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/youtrack/api/issues" || q.Get("query") != "sort by: updated desc" || q.Get("fields") != issueFields ||
			!slices.Equal(q["customFields"], []string{"Type"}) || q.Get("$top") != "2" {
			t.Errorf("request %s", r.URL)
		}
		calls.Add(1)
		fmt.Fprintf(w, `[
			{"id":"2-%[1]s","updated":1%[1]s,"resolved":null,"project":{"shortName":"ABC","$type":"Project"},
			 "customFields":[{"name":"Type","value":{"name":"Bug","$type":"EnumBundleElement"}}]},
			{"id":"3-%[1]s","updated":1,"resolved":1700000000000,"customFields":[]}]`, q.Get("$skip"))
	}))

	var pages [][]Issue
	err := c.IssuePages(context.Background(), "sort by: updated desc", 2, []string{"Type"}, func(page []Issue) bool {
		pages = append(pages, page)
		return len(pages) < 2
	})
	if err != nil {
		t.Fatal(err)
	}
	// full pages are read until fn stops
	if calls.Load() != 2 || len(pages) != 2 || pages[1][0].ID != "2-2" {
		t.Fatalf("%d calls, pages %+v", calls.Load(), pages)
	}
	first, second := pages[0][0], pages[0][1]
	if first.Updated != 10 || first.Resolved != nil || first.Project.ShortName != "ABC" ||
		first.CustomFields[0].ValueName() != "Bug" {
		t.Errorf("first issue %+v", first)
	}
	if second.Resolved == nil || *second.Resolved != 1700000000000 || second.Project != nil {
		t.Errorf("second issue %+v", second)
	}
}

func TestIssueCustomFieldValueName(t *testing.T) {
	for value, want := range map[string]string{
		`{"name":"Bug","$type":"EnumBundleElement"}`: "Bug",
		`[{"name":"Bug"},{"name":"Task"}]`:           "Bug",
		`[]`:                                         "",
		`null`:                                       "",
		`"text"`:                                     "",
		``:                                           "",
	} {
		if got := (IssueCustomField{Value: json.RawMessage(value)}).ValueName(); got != want {
			t.Errorf("ValueName(%s) = %q, want %q", value, got, want)
		}
	}
}

func TestGetAllPaginates(t *testing.T) {
	const total = pageSize + 7
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		top, _ := strconv.Atoi(r.URL.Query().Get("$top"))
		skip, _ := strconv.Atoi(r.URL.Query().Get("$skip"))
		users := []User{}
		for i := skip; i < min(skip+top, total); i++ {
			users = append(users, User{ID: strconv.Itoa(i)})
		}
		_ = json.NewEncoder(w).Encode(users)
	}))

	users, err := c.Users(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != total || users[total-1].ID != strconv.Itoa(total-1) {
		t.Errorf("got %d users, want %d", len(users), total)
	}
}

func TestPathIDs(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/youtrack/api/admin/projects/0-1%2Fx/customFields" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`[{"field":{"name":"Type"},"bundle":{"values":[{"name":"Bug"}]}}]`))
	}))

	fields, err := c.ProjectCustomFields(context.Background(), "0-1/x")
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 1 || fields[0].Field.Name != "Type" || fields[0].Bundle.Values[0].Name != "Bug" {
		t.Errorf("fields = %+v", fields)
	}
	// the endpoint label keeps the placeholder, not the project ID
	if n := testutil.ToFloat64(c.requests.WithLabelValues("/api/admin/projects/{id}/customFields", "200")); n != 1 {
		t.Errorf("requests with the endpoint label = %v, want 1", n)
	}
}

func TestAPIError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"Forbidden","error_description":"Low-level Admin Read is required"}`))
	}))

	_, err := c.Telemetry(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden ||
		apiErr.Message != "Forbidden: Low-level Admin Read is required" {
		t.Fatalf("err = %v", err)
	}
	if !IsPermissionError(err) {
		t.Error("IsPermissionError = false")
	}
}

func TestValueAcceptsStringsAndNumbers(t *testing.T) {
	var tel Telemetry
	err := json.Unmarshal([]byte(`{"availableProcessors":8,"usedMemory":"1.0 GB","databaseSize":null}`), &tel)
	if err != nil {
		t.Fatal(err)
	}
	if tel.AvailableProcessors != "8" || tel.UsedMemory != "1.0 GB" || tel.DatabaseSize != "" {
		t.Errorf("got %+v", tel)
	}
}
