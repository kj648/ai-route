package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
)

// Request capture: an administrator asks for the full bodies of the next N
// requests of one API key and / or model (to debug a client or a protocol
// conversion). Off unless a rule is active; captures are deleted after
// CaptureRetention and sealed with SECRET_KEY like the other secrets.

const (
	// CaptureRetention is how long captured bodies are kept.
	CaptureRetention = 24 * time.Hour
	// MaxCaptureBody caps each captured body; longer ones are cut.
	MaxCaptureBody = 1 << 20
	// MaxCaptureCount caps one rule; maxCaptures caps the stored total.
	MaxCaptureCount = 50
	maxCaptures     = 200
	// MaxCaptureTTL is the longest a rule waits for matching requests.
	MaxCaptureTTL = 24 * time.Hour
)

const captureSchema = `
CREATE TABLE IF NOT EXISTS capture_rules (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	key_id INTEGER NOT NULL DEFAULT 0,
	model TEXT NOT NULL DEFAULT '',
	total INTEGER NOT NULL DEFAULT 0,
	remaining INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS captures (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	rule_id INTEGER NOT NULL DEFAULT 0,
	request_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	key_id INTEGER NOT NULL DEFAULT 0,
	key_name TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	inbound TEXT NOT NULL DEFAULT '',
	status INTEGER NOT NULL DEFAULT 0,
	size INTEGER NOT NULL DEFAULT 0,
	data TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_captures_created ON captures(created_at);
CREATE INDEX IF NOT EXISTS idx_captures_request ON captures(request_id);
`

// CaptureRule asks for the next Total requests of a key and / or model.
type CaptureRule struct {
	ID        int64  `json:"id"`
	KeyID     int64  `json:"key_id"` // 0 = any key
	Model     string `json:"model"`  // public model name, "" = any
	Total     int    `json:"total"`
	Remaining int    `json:"remaining"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
}

// Active reports whether the rule still wants requests at now (unix ms).
func (r *CaptureRule) Active(now int64) bool { return r.Remaining > 0 && now < r.ExpiresAt }

// Matches reports whether a request of key / model falls under the rule.
func (r *CaptureRule) Matches(keyID int64, model string) bool {
	return (r.KeyID == 0 || r.KeyID == keyID) && (r.Model == "" || r.Model == model)
}

// Capture is one captured request: what the client sent, every upstream
// attempt, and what the client got back.
type Capture struct {
	ID        int64  `json:"id"`
	RuleID    int64  `json:"rule_id"`
	RequestID string `json:"request_id"`
	CreatedAt int64  `json:"created_at"`
	KeyID     int64  `json:"key_id"`
	KeyName   string `json:"key_name"`
	Model     string `json:"model"`
	Inbound   string `json:"inbound"`
	Status    int    `json:"status"` // HTTP status the client got
	Size      int64  `json:"size"`   // bytes of captured bodies

	// the bodies; empty in listings
	*CaptureData `json:",omitempty"`
}

// CaptureData are the captured bodies.
type CaptureData struct {
	ClientRequest  string           `json:"client_request"`
	Attempts       []CaptureAttempt `json:"attempts"`
	ClientResponse string           `json:"client_response"`
	Truncated      bool             `json:"truncated"`
	// Unreadable: sealed with a key this instance does not have
	Unreadable bool `json:"unreadable,omitempty"`
}

// CaptureAttempt is one upstream call of a captured request.
type CaptureAttempt struct {
	Target   string `json:"target"`
	Protocol string `json:"protocol"`
	URL      string `json:"url"`
	Request  string `json:"request"`
	Status   int    `json:"status"`
	Response string `json:"response"`
}

func (d *CaptureData) size() int64 {
	n := len(d.ClientRequest) + len(d.ClientResponse)
	for _, a := range d.Attempts {
		n += len(a.Request) + len(a.Response)
	}
	return int64(n)
}

func (s *Store) listCaptureRules() ([]*CaptureRule, error) {
	rows, err := s.db.Query(`SELECT id, key_id, model, total, remaining, created_at, expires_at FROM capture_rules ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CaptureRule{}
	for rows.Next() {
		r := &CaptureRule{}
		if err := rows.Scan(&r.ID, &r.KeyID, &r.Model, &r.Total, &r.Remaining, &r.CreatedAt, &r.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListCaptureRules returns every rule, newest first, with current counts.
func (s *Store) ListCaptureRules() ([]*CaptureRule, error) { return s.listCaptureRules() }

// CreateCaptureRule starts capturing the next count requests of a key
// and / or model for at most ttl.
func (s *Store) CreateCaptureRule(r *CaptureRule, ttl time.Duration) error {
	r.Model = strings.TrimSpace(r.Model)
	if r.Total <= 0 || r.Total > MaxCaptureCount {
		return errors.New("count must be between 1 and 50")
	}
	if ttl <= 0 || ttl > MaxCaptureTTL {
		return errors.New("ttl must be between 1 minute and 24 hours")
	}
	if r.KeyID < 0 {
		return errors.New("invalid key_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Remaining, r.CreatedAt = r.Total, now()
	r.ExpiresAt = r.CreatedAt + ttl.Milliseconds()
	if err := s.db.QueryRow(`INSERT INTO capture_rules (key_id, model, total, remaining, created_at, expires_at) VALUES (?,?,?,?,?,?) RETURNING id`,
		r.KeyID, r.Model, r.Total, r.Remaining, r.CreatedAt, r.ExpiresAt).Scan(&r.ID); err != nil {
		return err
	}
	return s.reloadLocked()
}

// DeleteCaptureRule stops a rule; what it captured is kept.
func (s *Store) DeleteCaptureRule(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM capture_rules WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return s.reloadLocked()
}

// TakeCapture claims one capture of a rule; false when the rule is used up
// or expired (another instance may have taken the last one).
func (s *Store) TakeCapture(ruleID int64) bool {
	res, err := s.db.Exec(`UPDATE capture_rules SET remaining = remaining - 1 WHERE id = ? AND remaining > 0 AND expires_at > ?`, ruleID, now())
	if err != nil {
		slog.Warn("capture: claiming a slot failed", "rule", ruleID, "err", err)
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// SaveCapture stores a captured request, dropping the oldest ones beyond
// the cap.
func (s *Store) SaveCapture(c *Capture) error {
	d := c.CaptureData
	if d == nil {
		d = &CaptureData{}
	}
	c.Size = d.size()
	data := s.sec.seal(mustJSON(d))
	if err := s.db.QueryRow(`INSERT INTO captures (rule_id, request_id, created_at, key_id, key_name, model, inbound, status, size, data) VALUES (?,?,?,?,?,?,?,?,?,?) RETURNING id`,
		c.RuleID, clip(c.RequestID, 64), c.CreatedAt, c.KeyID, clip(c.KeyName, 128), clip(c.Model, 256), c.Inbound, c.Status, c.Size, data).Scan(&c.ID); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM captures WHERE id <= (SELECT id FROM captures ORDER BY id DESC LIMIT 1 OFFSET ?)`, maxCaptures)
	return err
}

const captureCols = `id, rule_id, request_id, created_at, key_id, key_name, model, inbound, status, size`

func scanCapture(sc interface{ Scan(...any) error }, extra ...any) (*Capture, error) {
	c := &Capture{}
	dest := append([]any{&c.ID, &c.RuleID, &c.RequestID, &c.CreatedAt, &c.KeyID, &c.KeyName, &c.Model, &c.Inbound, &c.Status, &c.Size}, extra...)
	return c, sc.Scan(dest...)
}

// ListCaptures returns captured requests without their bodies, newest first.
func (s *Store) ListCaptures() ([]*Capture, error) {
	rows, err := s.db.Query(`SELECT ` + captureCols + ` FROM captures ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Capture{}
	for rows.Next() {
		c, err := scanCapture(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCapture returns one captured request with its bodies.
func (s *Store) GetCapture(id int64) (*Capture, error) {
	var data string
	c, err := scanCapture(s.db.QueryRow(`SELECT `+captureCols+`, data FROM captures WHERE id=?`, id), &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	c.CaptureData = &CaptureData{}
	plain, err := s.sec.open(data)
	if err != nil {
		c.Unreadable = true
		return c, nil
	}
	_ = json.Unmarshal([]byte(plain), c.CaptureData)
	return c, nil
}

// DeleteCaptures removes every captured request.
func (s *Store) DeleteCaptures() error {
	_, err := s.db.Exec(`DELETE FROM captures`)
	return err
}

// cleanupCaptures drops captures past retention and rules that ended a
// while ago.
func (s *Store) cleanupCaptures() {
	cutoff := time.Now().Add(-CaptureRetention).UnixMilli()
	if _, err := s.db.Exec(`DELETE FROM captures WHERE created_at < ?`, cutoff); err != nil {
		slog.Error("cleanup captures failed", "err", err)
	}
	res, err := s.db.Exec(`DELETE FROM capture_rules WHERE expires_at < ? OR (remaining = 0 AND created_at < ?)`, cutoff, cutoff)
	if err != nil {
		slog.Error("cleanup capture rules failed", "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.reloadLocked(); err != nil {
			slog.Error("reload after capture cleanup failed", "err", err)
		}
	}
}
