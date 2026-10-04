// Package config loads the exporter configuration file.
package config

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const minInterval = 10 * time.Second

var labelNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

type Config struct {
	YouTrack   YouTrack   `yaml:"youtrack"`
	Collectors Collectors `yaml:"collectors"`
}

type YouTrack struct {
	// URL is the YouTrack base URL (env YOUTRACK_URL).
	URL string `yaml:"url"`
	// Token is a permanent token (env YOUTRACK_TOKEN), TokenFile a file holding it (env YOUTRACK_TOKEN_FILE).
	Token                 string        `yaml:"token"`
	TokenFile             string        `yaml:"token_file"`
	Timeout               time.Duration `yaml:"timeout"`
	MaxConcurrentRequests int           `yaml:"max_concurrent_requests"`
	CAFile                string        `yaml:"ca_file"`
	InsecureSkipVerify    bool          `yaml:"insecure_skip_verify"`
}

type Collectors struct {
	Telemetry  Toggle     `yaml:"telemetry"`
	Backup     Toggle     `yaml:"backup"`
	License    Toggle     `yaml:"license"`
	Projects   Projects   `yaml:"projects"`
	IssueTypes IssueTypes `yaml:"issue_types"`
	Users      Users      `yaml:"users"`
	Queries    Queries    `yaml:"queries"`
}

// Toggle configures a collector that queries YouTrack on every scrape.
type Toggle struct {
	Enabled bool `yaml:"enabled"`
}

// Projects counts the issues of every project; it runs in the background every Interval.
type Projects struct {
	Enabled         bool          `yaml:"enabled"`
	Interval        time.Duration `yaml:"interval"`
	IncludeArchived bool          `yaml:"include_archived"`
	// Include limits the collector to these project short names, Exclude skips them.
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`
}

// IssueTypes counts the open and closed issues of every project by issue type from an index of the issues kept in
// memory; it applies the changed issues every Interval and reloads every project every FullResyncInterval.
// Archived projects are not counted.
type IssueTypes struct {
	Enabled            bool          `yaml:"enabled"`
	Interval           time.Duration `yaml:"interval"`
	FullResyncInterval time.Duration `yaml:"full_resync_interval"`
	// TypeField is the name of the custom field holding the issue type.
	TypeField string `yaml:"type_field"`
	// Include limits the collector to these project short names.
	Include []string `yaml:"include"`
}

// Users counts the user accounts; it runs in the background every Interval.
type Users struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
}

// Queries counts the issues matching YouTrack search queries; it runs in the background every Interval.
type Queries struct {
	Interval time.Duration `yaml:"interval"`
	Items    []Query       `yaml:"items"`
}

type Query struct {
	Name   string            `yaml:"name"`
	Query  string            `yaml:"query"`
	Labels map[string]string `yaml:"labels"`
}

func Default() Config {
	return Config{
		YouTrack: YouTrack{
			Timeout:               10 * time.Second,
			MaxConcurrentRequests: 4,
		},
		Collectors: Collectors{
			Telemetry:  Toggle{Enabled: true},
			Backup:     Toggle{Enabled: true},
			License:    Toggle{Enabled: true},
			Projects:   Projects{Enabled: true, Interval: 5 * time.Minute},
			IssueTypes: IssueTypes{Interval: 2 * time.Minute, FullResyncInterval: 24 * time.Hour, TypeField: "Type"},
			Users:      Users{Enabled: true, Interval: 15 * time.Minute},
			Queries:    Queries{Interval: 5 * time.Minute},
		},
	}
}

// Load reads the configuration file (optional, path "" uses the defaults), applies the environment overrides
// YOUTRACK_URL, YOUTRACK_TOKEN and YOUTRACK_TOKEN_FILE, resolves the token and validates the result.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return cfg, err
		}
		defer f.Close()
		dec := yaml.NewDecoder(f)
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return cfg, fmt.Errorf("%s: %w", path, err)
		}
	}
	cfg.applyDefaults()

	if v := os.Getenv("YOUTRACK_URL"); v != "" {
		cfg.YouTrack.URL = v
	}
	if v := os.Getenv("YOUTRACK_TOKEN_FILE"); v != "" {
		cfg.YouTrack.TokenFile = v
	}
	if v := os.Getenv("YOUTRACK_TOKEN"); v != "" {
		cfg.YouTrack.Token, cfg.YouTrack.TokenFile = v, ""
	}
	if cfg.YouTrack.TokenFile != "" {
		b, err := os.ReadFile(cfg.YouTrack.TokenFile)
		if err != nil {
			return cfg, fmt.Errorf("youtrack.token_file: %w", err)
		}
		cfg.YouTrack.Token = strings.TrimSpace(string(b))
	}
	return cfg, cfg.validate()
}

// applyDefaults fills the values an empty YAML section (e.g. "projects:") resets to zero.
func (c *Config) applyDefaults() {
	d := Default()
	if c.YouTrack.Timeout == 0 {
		c.YouTrack.Timeout = d.YouTrack.Timeout
	}
	if c.YouTrack.MaxConcurrentRequests == 0 {
		c.YouTrack.MaxConcurrentRequests = d.YouTrack.MaxConcurrentRequests
	}
	if c.Collectors.Projects.Interval == 0 {
		c.Collectors.Projects.Interval = d.Collectors.Projects.Interval
	}
	if c.Collectors.IssueTypes.Interval == 0 {
		c.Collectors.IssueTypes.Interval = d.Collectors.IssueTypes.Interval
	}
	if c.Collectors.IssueTypes.FullResyncInterval == 0 {
		c.Collectors.IssueTypes.FullResyncInterval = d.Collectors.IssueTypes.FullResyncInterval
	}
	if c.Collectors.IssueTypes.TypeField == "" {
		c.Collectors.IssueTypes.TypeField = d.Collectors.IssueTypes.TypeField
	}
	if c.Collectors.Users.Interval == 0 {
		c.Collectors.Users.Interval = d.Collectors.Users.Interval
	}
	if c.Collectors.Queries.Interval == 0 {
		c.Collectors.Queries.Interval = d.Collectors.Queries.Interval
	}
}

func (c *Config) validate() error {
	var errs []error
	y := c.YouTrack
	if y.URL == "" {
		errs = append(errs, errors.New("youtrack.url (or YOUTRACK_URL) is required"))
	} else if u, err := url.Parse(y.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("youtrack.url %q: expected http(s)://host[:port][/path]", y.URL))
	}
	if y.Token == "" {
		errs = append(errs, errors.New("a permanent token is required: youtrack.token_file, YOUTRACK_TOKEN_FILE or YOUTRACK_TOKEN"))
	}
	if y.Timeout < 0 {
		errs = append(errs, errors.New("youtrack.timeout must be positive"))
	}
	if y.MaxConcurrentRequests < 1 {
		errs = append(errs, errors.New("youtrack.max_concurrent_requests must be at least 1"))
	}

	col := c.Collectors
	if col.Projects.Enabled && col.Projects.Interval < minInterval {
		errs = append(errs, fmt.Errorf("collectors.projects.interval must be at least %s", minInterval))
	}
	if col.IssueTypes.Enabled && col.IssueTypes.Interval < minInterval {
		errs = append(errs, fmt.Errorf("collectors.issue_types.interval must be at least %s", minInterval))
	}
	if col.IssueTypes.Enabled && col.IssueTypes.FullResyncInterval < col.IssueTypes.Interval {
		errs = append(errs, errors.New("collectors.issue_types.full_resync_interval must not be shorter than the interval"))
	}
	if col.Users.Enabled && col.Users.Interval < minInterval {
		errs = append(errs, fmt.Errorf("collectors.users.interval must be at least %s", minInterval))
	}
	if len(col.Queries.Items) > 0 && col.Queries.Interval < minInterval {
		errs = append(errs, fmt.Errorf("collectors.queries.interval must be at least %s", minInterval))
	}
	names := map[string]bool{}
	for i, q := range col.Queries.Items {
		switch {
		case q.Name == "":
			errs = append(errs, fmt.Errorf("collectors.queries.items[%d]: name is required", i))
		case names[q.Name]:
			errs = append(errs, fmt.Errorf("collectors.queries.items[%d]: duplicate name %q", i, q.Name))
		}
		names[q.Name] = true
		for l := range q.Labels {
			if !labelNameRe.MatchString(l) || l == "name" || strings.HasPrefix(l, "__") {
				errs = append(errs, fmt.Errorf("collectors.queries.items[%d]: invalid label name %q", i, l))
			}
		}
	}
	return errors.Join(errs...)
}

// TLSConfig returns the TLS settings for the YouTrack connection, nil means the system defaults.
func (y YouTrack) TLSConfig() (*tls.Config, error) {
	if y.CAFile == "" && !y.InsecureSkipVerify {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: y.InsecureSkipVerify} //nolint:gosec // opt-in
	if y.CAFile != "" {
		pem, err := os.ReadFile(y.CAFile)
		if err != nil {
			return nil, fmt.Errorf("youtrack.ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("youtrack.ca_file %s: no PEM certificates found", y.CAFile)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}
