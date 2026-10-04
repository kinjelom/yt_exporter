package collector

import (
	"context"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kinjelom/yt_exporter/internal/youtrack"
)

// usersScraper counts the user accounts by type and ban state, the guest account is not counted.
type usersScraper struct {
	client *youtrack.Client
}

var (
	usersDesc = newDesc("users",
		"Number of user accounts (guest excluded) by user type (YouTrack 2026.2+, otherwise unknown) and ban state.",
		"type", "banned")
	knownUserTypes = []string{"standard_user", "agent", "reporter"}
)

func (s *usersScraper) Name() string { return "users" }

func (s *usersScraper) Scrape(ctx context.Context, emit func(prometheus.Metric)) error {
	users, err := s.client.Users(ctx)
	if err != nil {
		return err
	}
	type key struct {
		userType string
		banned   bool
	}
	counts := map[key]int{}
	// the known types are always exported, also with 0
	for _, t := range knownUserTypes {
		counts[key{t, false}], counts[key{t, true}] = 0, 0
	}
	for _, u := range users {
		if u.Guest {
			continue
		}
		t := "unknown"
		if u.UserType != nil && u.UserType.ID != "" {
			t = strings.ToLower(u.UserType.ID)
		}
		counts[key{t, u.Banned}]++
	}
	for k, n := range counts {
		emit(prometheus.MustNewConstMetric(usersDesc, prometheus.GaugeValue, float64(n), k.userType, strconv.FormatBool(k.banned)))
	}
	return nil
}
