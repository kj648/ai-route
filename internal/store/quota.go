package store

import (
	"fmt"
	"strings"
	"time"
)

// Quota is a plan's own allowance (coding plans count requests per 5 hours
// or per week, some count tokens). The gateway does not refuse traffic over
// it: the provider moves behind the others in every routing order until the
// window has room again, and is still tried as a last resort.
type Quota struct {
	// Period: "5h" (the last five hours, rolling), "day", "week" (from
	// Monday) or "month" (from the 1st), calendar periods in local time.
	Period   string `json:"period"`
	Requests int64  `json:"requests,omitempty"` // 0 = not counted
	Tokens   int64  `json:"tokens,omitempty"`   // input + output, 0 = not counted
}

// QuotaPeriods are the accepted periods, in display order.
var QuotaPeriods = []string{"5h", "day", "week", "month"}

func normalizeQuotas(qs []Quota) ([]Quota, error) {
	out := []Quota{}
	seen := map[string]bool{}
	for i, q := range qs {
		q.Period = strings.ToLower(strings.TrimSpace(q.Period))
		valid := false
		for _, p := range QuotaPeriods {
			valid = valid || p == q.Period
		}
		if !valid {
			return nil, fmt.Errorf("quotas[%d]: period must be one of %s, got %q", i, strings.Join(QuotaPeriods, ", "), q.Period)
		}
		if q.Requests < 0 || q.Tokens < 0 {
			return nil, fmt.Errorf("quotas[%d]: limits must not be negative", i)
		}
		if q.Requests == 0 && q.Tokens == 0 {
			continue
		}
		if seen[q.Period] {
			return nil, fmt.Errorf("quotas[%d]: period %s is listed twice", i, q.Period)
		}
		seen[q.Period] = true
		out = append(out, q)
	}
	return out, nil
}

// QuotaStart is where a quota's window begins at now, in unix ms.
func QuotaStart(period string, now time.Time) int64 {
	y, m, d := now.Date()
	loc := now.Location()
	switch period {
	case "5h":
		return now.Add(-5 * time.Hour).UnixMilli()
	case "day":
		return time.Date(y, m, d, 0, 0, 0, 0, loc).UnixMilli()
	case "week":
		back := (int(now.Weekday()) + 6) % 7 // days since Monday
		return time.Date(y, m, d-back, 0, 0, 0, 0, loc).UnixMilli()
	case "month":
		return time.Date(y, m, 1, 0, 0, 0, 0, loc).UnixMilli()
	}
	return now.UnixMilli()
}

// ProviderUsage counts the requests a provider answered since the given
// unix ms, and their tokens (input + output). Whole hours come from the
// hourly rollup; a window starting mid-hour reads that first partial hour
// from the raw logs.
func (s *Store) ProviderUsage(prefix string, since int64) (requests, tokens int64, err error) {
	firstHour := since - since%hourMs
	if firstHour < since {
		firstHour += hourMs
		var n, tk int64
		if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(input_tokens + output_tokens), 0) FROM request_logs
			WHERE provider = ? AND created_at >= ? AND created_at < ?`, prefix, since, firstHour).Scan(&n, &tk); err != nil {
			return 0, 0, err
		}
		requests, tokens = n, tk
	}
	var n, tk int64
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(requests), 0), COALESCE(SUM(input_tokens + output_tokens), 0) FROM request_stats
		WHERE provider = ? AND hour >= ?`, prefix, firstHour).Scan(&n, &tk); err != nil {
		return 0, 0, err
	}
	return requests + n, tokens + tk, nil
}
