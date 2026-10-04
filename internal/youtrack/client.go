// Package youtrack is a minimal client for the parts of the YouTrack REST API the exporter reads.
package youtrack

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/version"
)

const (
	maxResponseBytes = 32 << 20
	pageSize         = 500
	maxPages         = 2000

	// /api/issuesGetter/count answers -1 until the count is ready, see countIssues.
	countFirstDelay   = 250 * time.Millisecond
	countMaxDelay     = 4 * time.Second
	countMaxAttempts  = 25
	telemetryFields   = "availableProcessors,availableMemory,allocatedMemory,usedMemory,startedTime,databaseBackgroundThreads,pendingAsyncJobs,cachedResultsCountInDBQueriesCache,databaseQueriesCacheHitRate,blobStringsCacheHitRate,totalTransactions,transactionsPerSecond,requestsPerSecond,databaseSize,fullDatabaseSize,textIndexSize,onlineUsers(users),reportCalculatorThreads,notificationAnalyzerThreads"
	backupStatusField = "backupInProgress,backupCancelled,backupError(date,errorMessage)"
	issueFields       = "id,updated,resolved,project(shortName),customFields(name,value(name))"
)

type Options struct {
	// URL is the YouTrack base URL, e.g. https://youtrack.example.com or http://127.0.0.1:8080.
	URL   string
	Token string
	// Timeout limits a single HTTP request.
	Timeout time.Duration
	// MaxConcurrentRequests limits the requests in flight to YouTrack across all collectors.
	MaxConcurrentRequests int
	TLSConfig             *tls.Config
	// Registerer receives the client request metrics, nil disables them.
	Registerer prometheus.Registerer
}

type Client struct {
	base     *url.URL
	token    string
	timeout  time.Duration
	http     *http.Client
	sem      chan struct{}
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// APIError is a non-2xx answer of the YouTrack REST API.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.StatusCode, e.Message)
}

func New(o Options) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(o.URL, "/"))
	if err != nil {
		return nil, fmt.Errorf("youtrack url: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" || base.Host == "" {
		return nil, fmt.Errorf("youtrack url %q: expected http(s)://host[:port][/path]", o.URL)
	}
	if o.MaxConcurrentRequests < 1 {
		o.MaxConcurrentRequests = 1
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = o.TLSConfig
	transport.MaxIdleConnsPerHost = o.MaxConcurrentRequests

	c := &Client{
		base:    base,
		token:   o.Token,
		timeout: o.Timeout,
		http:    &http.Client{Transport: transport},
		sem:     make(chan struct{}, o.MaxConcurrentRequests),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "yt_exporter_youtrack_api_requests_total",
			Help: "Requests sent to the YouTrack REST API, by endpoint and HTTP status code (error = no response).",
		}, []string{"endpoint", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "yt_exporter_youtrack_api_request_duration_seconds",
			Help:    "Duration of YouTrack REST API requests, by endpoint.",
			Buckets: []float64{.025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		}, []string{"endpoint"}),
	}
	if o.Registerer != nil {
		o.Registerer.MustRegister(c.requests, c.duration)
	}
	return c, nil
}

// AppConfig returns the YouTrack version and build.
func (c *Client) AppConfig(ctx context.Context) (AppConfig, error) {
	var cfg AppConfig
	err := c.do(ctx, http.MethodGet, "/api/config", nil, url.Values{"fields": {"version,build"}}, nil, &cfg)
	return cfg, err
}

// Telemetry needs the Low-level Admin Read permission.
func (c *Client) Telemetry(ctx context.Context) (Telemetry, error) {
	var t Telemetry
	err := c.do(ctx, http.MethodGet, "/api/admin/telemetry", nil, url.Values{"fields": {telemetryFields}}, nil, &t)
	return t, err
}

func (c *Client) BackupStatus(ctx context.Context) (BackupStatus, error) {
	var s BackupStatus
	err := c.do(ctx, http.MethodGet, "/api/admin/databaseBackup/settings/backupStatus", nil,
		url.Values{"fields": {backupStatusField}}, nil, &s)
	return s, err
}

func (c *Client) Backups(ctx context.Context) ([]BackupFile, error) {
	return getAll[BackupFile](ctx, c, "/api/admin/databaseBackup/backups", "name,size,creationDate")
}

func (c *Client) License(ctx context.Context) (License, error) {
	var l License
	err := c.do(ctx, http.MethodGet, "/api/admin/globalSettings/license", nil, url.Values{"fields": {"error"}}, nil, &l)
	return l, err
}

func (c *Client) Projects(ctx context.Context) ([]Project, error) {
	return getAll[Project](ctx, c, "/api/admin/projects", "id,shortName,name,archived")
}

// ProjectCustomFields lists the custom fields of a project with the values of their bundles.
func (c *Client) ProjectCustomFields(ctx context.Context, projectID string) ([]ProjectCustomField, error) {
	return getAll[ProjectCustomField](ctx, c, "/api/admin/projects/{id}/customFields",
		"field(name),bundle(values(name))", projectID)
}

// Users lists all user accounts; userType is returned by YouTrack 2026.2 and later.
func (c *Client) Users(ctx context.Context) ([]User, error) {
	return getAll[User](ctx, c, "/api/users", "id,login,guest,banned,userType(id)")
}

// IssuePages reads the issues matching a search query page by page and calls fn with every page that is not empty,
// until a page is shorter than pageSize or fn returns false. Only the values of the named custom fields are read.
func (c *Client) IssuePages(ctx context.Context, query string, pageSize int, customFields []string,
	fn func([]Issue) bool) error {
	for page := 0; page < maxPages; page++ {
		q := url.Values{
			"fields":       {issueFields},
			"customFields": customFields,
			"$top":         {strconv.Itoa(pageSize)},
			"$skip":        {strconv.Itoa(page * pageSize)},
		}
		if query != "" {
			q.Set("query", query)
		}
		var issues []Issue
		if err := c.do(ctx, http.MethodGet, "/api/issues", nil, q, nil, &issues); err != nil {
			return err
		}
		if len(issues) == 0 || !fn(issues) || len(issues) < pageSize {
			return nil
		}
	}
	return fmt.Errorf("GET /api/issues: more than %d pages", maxPages)
}

// CountIssues returns the number of issues matching a YouTrack search query, "" counts all issues.
// YouTrack answers -1 while it is still counting, the request is then repeated with a growing delay.
func (c *Client) CountIssues(ctx context.Context, query string) (int64, error) {
	body := map[string]string{}
	if query != "" {
		body["query"] = query
	}
	delay := countFirstDelay
	for attempt := 1; ; attempt++ {
		var resp struct {
			Count *int64 `json:"count"`
		}
		if err := c.do(ctx, http.MethodPost, "/api/issuesGetter/count", nil, url.Values{"fields": {"count"}}, body, &resp); err != nil {
			return 0, err
		}
		if resp.Count == nil {
			return 0, fmt.Errorf("count %q: no count in the response", query)
		}
		if *resp.Count >= 0 {
			return *resp.Count, nil
		}
		if attempt == countMaxAttempts {
			return 0, fmt.Errorf("count %q: not ready after %d attempts", query, attempt)
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("count %q: %w", query, ctx.Err())
		case <-time.After(delay):
		}
		delay = min(delay*2, countMaxDelay)
	}
}

// getAll reads a collection resource page by page ($top/$skip). ids replace the "{id}" placeholders of path.
func getAll[T any](ctx context.Context, c *Client, path, fields string, ids ...string) ([]T, error) {
	var all []T
	for page := 0; page < maxPages; page++ {
		q := url.Values{
			"fields": {fields},
			"$top":   {strconv.Itoa(pageSize)},
			"$skip":  {strconv.Itoa(page * pageSize)},
		}
		var items []T
		if err := c.do(ctx, http.MethodGet, path, ids, q, nil, &items); err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(items) < pageSize {
			return all, nil
		}
	}
	return nil, fmt.Errorf("GET %s: more than %d pages", path, maxPages)
}

// do sends one request. path is an API path in which each "{id}" placeholder is replaced by the next of ids. The
// path with the placeholders is the endpoint label of the client metrics, so the label does not depend on IDs.
func (c *Client) do(ctx context.Context, method, path string, ids []string, query url.Values, body, out any) error {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return fmt.Errorf("%s %s: %w", method, path, ctx.Err())
	}
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	expanded := path
	for _, id := range ids {
		expanded = strings.Replace(expanded, "{id}", url.PathEscape(id), 1)
	}
	u := c.base.JoinPath(expanded)
	u.RawQuery = query.Encode()
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "github.com/kinjelom/yt_exporter/"+version.Version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	start := time.Now()
	resp, err := c.http.Do(req)
	c.duration.WithLabelValues(path).Observe(time.Since(start).Seconds())
	if err != nil {
		c.requests.WithLabelValues(path, "error").Inc()
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	c.requests.WithLabelValues(path, strconv.Itoa(resp.StatusCode)).Inc()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Method: method, Path: path, StatusCode: resp.StatusCode, Message: errorMessage(data)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

// errorMessage extracts the message of a YouTrack error response: {"error": "...", "error_description": "..."}.
func errorMessage(body []byte) string {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(body, &e) == nil {
		switch {
		case e.Error != "" && e.Description != "":
			return e.Error + ": " + e.Description
		case e.Error != "" || e.Description != "":
			return e.Error + e.Description
		}
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	if msg == "" {
		msg = "empty response"
	}
	return msg
}

// IsPermissionError reports whether err is a 401/403 answer, i.e. the token lacks a permission.
func IsPermissionError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden)
}
