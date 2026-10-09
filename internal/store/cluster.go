package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Several gateway instances can share one PostgreSQL database. Cluster holds
// what they coordinate through it: a registry with heartbeats, config change
// notices, sliding-window counters (rate limits), in-flight slot counts,
// circuit-breaker cooldowns and short leases (alert de-duplication, periodic
// jobs). With SQLite there is a single instance and no Cluster.

const (
	// HeartbeatEvery is how often an instance reports that it is alive;
	// instances silent for InstanceTimeout no longer hold slots.
	HeartbeatEvery  = 5 * time.Second
	InstanceTimeout = 20 * time.Second
	// PollEvery is how often config changes and breaker cooldowns made by
	// other instances are picked up.
	PollEvery = time.Second
	// windowSpan is the rate-limit window: per-second buckets over a minute.
	windowSpan = 60
)

const clusterSchema = `
-- UNLOGGED: this state is rebuilt within seconds (heartbeats, resyncs), so
-- it skips the write-ahead log; commits touching only these tables do not
-- wait for a disk flush while holding a key's lock. After a PostgreSQL crash
-- the tables start empty.
CREATE UNLOGGED TABLE IF NOT EXISTS cluster_instances (
	id TEXT PRIMARY KEY,
	host TEXT NOT NULL DEFAULT '',
	version TEXT NOT NULL DEFAULT '',
	started_at BIGINT NOT NULL,
	seen_at BIGINT NOT NULL
);
CREATE UNLOGGED TABLE IF NOT EXISTS cluster_counters (
	k TEXT NOT NULL,
	bucket BIGINT NOT NULL,
	n BIGINT NOT NULL DEFAULT 0,
	PRIMARY KEY (k, bucket)
);
CREATE UNLOGGED TABLE IF NOT EXISTS cluster_slots (
	k TEXT NOT NULL,
	instance TEXT NOT NULL,
	n BIGINT NOT NULL DEFAULT 0,
	PRIMARY KEY (k, instance)
);
CREATE SEQUENCE IF NOT EXISTS cluster_breaker_seq;
CREATE UNLOGGED TABLE IF NOT EXISTS cluster_breaker (
	k TEXT PRIMARY KEY,
	open_until BIGINT NOT NULL DEFAULT 0,
	opens BIGINT NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	at BIGINT NOT NULL DEFAULT 0,
	origin TEXT NOT NULL DEFAULT '',
	seq BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_cluster_breaker_seq ON cluster_breaker(seq);
CREATE UNLOGGED TABLE IF NOT EXISTS cluster_leases (
	k TEXT PRIMARY KEY,
	until_ms BIGINT NOT NULL,
	owner TEXT NOT NULL DEFAULT ''
);
-- check-and-count in one round trip. The advisory lock serializes one key
-- until the statement commits; each statement of a volatile function sees
-- what was committed before it, so the sum includes the previous holder.
CREATE OR REPLACE FUNCTION ai_route_take_window(p_k TEXT, p_now BIGINT, p_limit BIGINT, p_span BIGINT)
RETURNS BOOLEAN LANGUAGE plpgsql AS $$
DECLARE used BIGINT;
BEGIN
	PERFORM pg_advisory_xact_lock(hashtext('w:' || p_k));
	SELECT COALESCE(SUM(c.n), 0) INTO used FROM cluster_counters c WHERE c.k = p_k AND c.bucket > p_now - p_span;
	IF used >= p_limit THEN
		RETURN FALSE;
	END IF;
	INSERT INTO cluster_counters (k, bucket, n) VALUES (p_k, p_now, 1)
		ON CONFLICT (k, bucket) DO UPDATE SET n = cluster_counters.n + 1;
	RETURN TRUE;
END $$;
-- publishers take one lock, so sequence numbers become visible in order and
-- a poller that has seen seq N never misses a smaller one
CREATE OR REPLACE FUNCTION ai_route_publish_breaker(p_k TEXT, p_until BIGINT, p_opens BIGINT, p_error TEXT, p_at BIGINT, p_origin TEXT)
RETURNS BOOLEAN LANGUAGE plpgsql AS $$
BEGIN
	PERFORM pg_advisory_xact_lock(hashtext('breaker-publish'));
	INSERT INTO cluster_breaker (k, open_until, opens, error, at, origin, seq)
		VALUES (p_k, p_until, p_opens, p_error, p_at, p_origin, nextval('cluster_breaker_seq'))
		ON CONFLICT (k) DO UPDATE SET open_until = excluded.open_until, opens = excluded.opens, error = excluded.error,
		at = excluded.at, origin = excluded.origin, seq = excluded.seq;
	RETURN TRUE;
END $$;
CREATE OR REPLACE FUNCTION ai_route_acquire(p_k TEXT, p_inst TEXT, p_mine BIGINT, p_limit BIGINT, p_alive BIGINT)
RETURNS BOOLEAN LANGUAGE plpgsql AS $$
DECLARE others BIGINT;
BEGIN
	PERFORM pg_advisory_xact_lock(hashtext('s:' || p_k));
	SELECT COALESCE(SUM(s.n), 0) INTO others FROM cluster_slots s JOIN cluster_instances i ON i.id = s.instance
		WHERE s.k = p_k AND s.instance <> p_inst AND i.seen_at >= p_alive;
	IF others + p_mine >= p_limit THEN
		RETURN FALSE;
	END IF;
	INSERT INTO cluster_slots (k, instance, n) VALUES (p_k, p_inst, p_mine + 1)
		ON CONFLICT (k, instance) DO UPDATE SET n = excluded.n;
	RETURN TRUE;
END $$;
`

// Instance is one gateway process sharing the database.
type Instance struct {
	ID        string `json:"id"`
	Host      string `json:"host"`
	Version   string `json:"version"`
	StartedAt int64  `json:"started_at"`
	SeenAt    int64  `json:"seen_at"`
	Alive     bool   `json:"alive"`
	Self      bool   `json:"self"`
}

// Cooldown is a circuit-breaker state change shared between instances.
// Key "*" with a zero OpenUntil clears every entry.
type Cooldown struct {
	Key       string
	OpenUntil time.Time // zero: closed (cleared)
	Opens     int
	Error     string
	At        time.Time
}

// Bucket is one second of a sliding-window counter.
type Bucket struct {
	At int64 // unix seconds
	N  int64
}

type Cluster struct {
	s       *Store
	id      string
	host    string
	version string
	started int64
	now     func() time.Time

	seenConfig  atomic.Int64 // config version this instance has loaded
	seenBreaker atomic.Int64 // last breaker change seen
	// offset is the database clock minus this machine's (ms), measured at
	// each heartbeat: heartbeats, windows and leases written by different
	// machines are compared on one clock.
	offset atomic.Int64

	slotMu sync.Mutex
	slots  map[string]*slotState // this instance's in-flight counts, the source of its rows

	errMu   sync.Mutex
	lastErr time.Time
}

func newCluster(s *Store) *Cluster {
	host, _ := os.Hostname()
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	id := host + "-" + hex.EncodeToString(b)
	if host == "" {
		id = "ai-route-" + hex.EncodeToString(b)
	}
	return &Cluster{s: s, id: id, host: host, started: time.Now().UnixMilli(), now: time.Now, slots: map[string]*slotState{}}
}

// Cluster returns the coordination layer, or nil with SQLite (one instance).
func (s *Store) Cluster() *Cluster { return s.cluster }

// Now is the current time on the database's clock.
func (c *Cluster) Now() time.Time { return c.now().Add(c.Offset()) }

// Offset is how far the database clock is ahead of this machine's.
func (c *Cluster) Offset() time.Duration { return time.Duration(c.offset.Load()) * time.Millisecond }

// syncClock measures the offset to the database clock.
func (c *Cluster) syncClock() {
	before := c.now()
	var dbMs int64
	if err := c.s.db.QueryRow(`SELECT CAST(EXTRACT(EPOCH FROM clock_timestamp()) * 1000 AS BIGINT)`).Scan(&dbMs); err != nil {
		c.logErr("read database clock", err)
		return
	}
	after := c.now()
	mid := before.Add(after.Sub(before) / 2)
	c.offset.Store(dbMs - mid.UnixMilli())
}

// ID identifies this instance.
func (c *Cluster) ID() string { return c.id }

// SetVersion records the program version shown in the instance list.
func (c *Cluster) SetVersion(v string) { c.version = v }

// logErr reports coordination errors at most once a minute: when the
// database is unreachable the gateway keeps serving (limits fail open).
func (c *Cluster) logErr(what string, err error) {
	if err == nil {
		return
	}
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if time.Since(c.lastErr) < time.Minute {
		return
	}
	c.lastErr = time.Now()
	slog.Warn("cluster operation failed", "op", what, "err", err)
}

// ---------- registry and change polling ----------

// Handlers react to changes made by other instances.
type Handlers struct {
	Breaker func([]Cooldown) // breaker cooldowns opened / cleared elsewhere
}

// Run sends heartbeats, reloads the configuration when another instance
// changed it, delivers breaker changes and cleans up after dead instances,
// until ctx is done. It then removes this instance from the registry.
func (c *Cluster) Run(ctx context.Context, h Handlers) {
	c.Heartbeat()
	c.catchUp(h)
	beat := time.NewTicker(HeartbeatEvery)
	defer beat.Stop()
	poll := time.NewTicker(PollEvery)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			c.leave()
			return
		case <-beat.C:
			c.Heartbeat()
			c.resyncSlots()
			if c.Claim("cluster-cleanup", time.Minute) {
				c.cleanup()
			}
		case <-poll.C:
			c.Poll(h)
		}
	}
}

// Heartbeat registers this instance as alive (Run does it periodically).
// catchUp applies cooldowns still running when this instance starts; older
// breaker history is skipped.
func (c *Cluster) catchUp(h Handlers) {
	var seq int64
	if err := c.s.db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM cluster_breaker`).Scan(&seq); err != nil {
		c.logErr("read breaker", err)
		return
	}
	rows, err := c.s.db.Query(`SELECT k, open_until, opens, error, at FROM cluster_breaker WHERE open_until > ? AND seq <= ?`, c.Now().UnixMilli(), seq)
	if err != nil {
		c.logErr("read breaker", err)
		return
	}
	var open []Cooldown
	for rows.Next() {
		var cd Cooldown
		var until, at int64
		if err := rows.Scan(&cd.Key, &until, &cd.Opens, &cd.Error, &at); err != nil {
			break
		}
		cd.OpenUntil, cd.At = c.local(until), c.local(at)
		open = append(open, cd)
	}
	rows.Close()
	c.seenBreaker.Store(seq)
	if len(open) > 0 && h.Breaker != nil {
		h.Breaker(open)
	}
}

// local converts a database-clock unix ms value to this machine's clock.
func (c *Cluster) local(ms int64) time.Time { return time.UnixMilli(ms).Add(-c.Offset()) }

func (c *Cluster) Heartbeat() {
	c.syncClock()
	now := c.Now().UnixMilli()
	_, err := c.s.db.Exec(`INSERT INTO cluster_instances (id, host, version, started_at, seen_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET seen_at = excluded.seen_at, version = excluded.version`, c.id, c.host, c.version, c.started, now)
	c.logErr("heartbeat", err)
}

// Poll picks up configuration and breaker changes made by other instances.
func (c *Cluster) Poll(h Handlers) {
	var raw string
	err := c.s.db.QueryRow(`SELECT v FROM settings WHERE k='config_version'`).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		c.logErr("poll config", err)
		return
	}
	if v, _ := strconv.ParseInt(raw, 10, 64); v != c.seenConfig.Load() {
		if err := c.s.Reload(); err != nil {
			c.logErr("reload config", err)
		} else {
			c.seenConfig.Store(v)
		}
	}
	rows, err := c.s.db.Query(`SELECT k, open_until, opens, error, at, origin, seq FROM cluster_breaker WHERE seq > ? ORDER BY seq`, c.seenBreaker.Load())
	if err != nil {
		c.logErr("poll breaker", err)
		return
	}
	var changes []Cooldown
	last := c.seenBreaker.Load()
	for rows.Next() {
		var cd Cooldown
		var until, at, seq int64
		var origin string
		if err := rows.Scan(&cd.Key, &until, &cd.Opens, &cd.Error, &at, &origin, &seq); err != nil {
			rows.Close()
			c.logErr("poll breaker", err)
			return
		}
		last = seq
		if origin == c.id {
			continue
		}
		if until > 0 {
			cd.OpenUntil = c.local(until)
		}
		cd.At = c.local(at)
		changes = append(changes, cd)
	}
	rows.Close()
	c.seenBreaker.Store(last)
	if len(changes) > 0 && h.Breaker != nil {
		h.Breaker(changes)
	}
}

// bumpConfig tells the other instances to reload (after a config write) and
// returns the new version, or -1. It runs before this instance rebuilds its
// own snapshot, so a write another instance commits meanwhile is either in
// that snapshot or bumps the version again (and is picked up by Poll).
func (c *Cluster) bumpConfig() int64 {
	var raw string
	err := c.s.db.QueryRow(`INSERT INTO settings (k, v) VALUES ('config_version', '1')
		ON CONFLICT (k) DO UPDATE SET v = CAST(CAST(settings.v AS BIGINT) + 1 AS TEXT) RETURNING v`).Scan(&raw)
	if err != nil {
		c.logErr("bump config version", err)
		return -1
	}
	v, _ := strconv.ParseInt(raw, 10, 64)
	return v
}

func (c *Cluster) aliveSince() int64 { return c.Now().Add(-InstanceTimeout).UnixMilli() }

// Instances lists the registry, newest first.
func (c *Cluster) Instances() ([]Instance, error) {
	rows, err := c.s.db.Query(`SELECT id, host, version, started_at, seen_at FROM cluster_instances ORDER BY started_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	alive := c.aliveSince()
	out := []Instance{}
	for rows.Next() {
		var in Instance
		if err := rows.Scan(&in.ID, &in.Host, &in.Version, &in.StartedAt, &in.SeenAt); err != nil {
			return nil, err
		}
		in.Alive, in.Self = in.SeenAt >= alive, in.ID == c.id
		out = append(out, in)
	}
	return out, rows.Err()
}

// leave removes this instance (clean shutdown).
func (c *Cluster) leave() {
	_, _ = c.s.db.Exec(`DELETE FROM cluster_slots WHERE instance = ?`, c.id)
	_, _ = c.s.db.Exec(`DELETE FROM cluster_instances WHERE id = ?`, c.id)
}

// cleanup drops what dead instances and expired windows left behind.
func (c *Cluster) cleanup() {
	now := c.Now()
	dead := now.Add(-10 * time.Minute).UnixMilli()
	_, err := c.s.db.Exec(`DELETE FROM cluster_instances WHERE seen_at < ?`, dead)
	c.logErr("cleanup", err)
	// also rows written after an instance left (requests finishing during shutdown)
	_, _ = c.s.db.Exec(`DELETE FROM cluster_slots WHERE instance NOT IN (SELECT id FROM cluster_instances)`)
	_, _ = c.s.db.Exec(`DELETE FROM cluster_counters WHERE bucket < ?`, now.Unix()-2*windowSpan)
	_, _ = c.s.db.Exec(`DELETE FROM cluster_leases WHERE until_ms < ?`, now.Add(-24*time.Hour).UnixMilli())
	_, _ = c.s.db.Exec(`DELETE FROM cluster_breaker WHERE open_until < ? AND at < ?`, now.UnixMilli(), now.Add(-24*time.Hour).UnixMilli())
}

// ---------- sliding-window counters (RPM, TPM, admin lockout) ----------

func windowBuckets(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, k string, now time.Time) ([]Bucket, error) {
	rows, err := q.Query(`SELECT bucket, n FROM cluster_counters WHERE k = ? AND bucket > ? ORDER BY bucket`, k, now.Unix()-windowSpan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.At, &b.N); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// WindowSum totals a window's buckets.
func WindowSum(buckets []Bucket) int64 {
	var sum int64
	for _, b := range buckets {
		sum += b.N
	}
	return sum
}

// WindowRetry is when enough of a one-minute window of per-second buckets
// expires to get back under limit.
func WindowRetry(buckets []Bucket, limit int64, now time.Time) time.Duration {
	used := WindowSum(buckets)
	for _, b := range buckets {
		used -= b.N
		if used < limit {
			// buckets are whole seconds: so is the answer
			if d := time.Unix(b.At+windowSpan, 0).Sub(now).Round(time.Second); d > time.Second {
				return d
			}
			return time.Second
		}
	}
	return time.Minute
}

// Window returns the counter's per-second buckets of the minute before now.
func (c *Cluster) Window(k string, now time.Time) ([]Bucket, error) {
	return windowBuckets(c.s.db, k, now)
}

// AddWindow adds n to the counter's bucket for now.
func (c *Cluster) AddWindow(k string, n int64, now time.Time) error {
	_, err := c.s.db.Exec(`INSERT INTO cluster_counters (k, bucket, n) VALUES (?, ?, ?)
		ON CONFLICT (k, bucket) DO UPDATE SET n = cluster_counters.n + excluded.n`, k, now.Unix(), n)
	return err
}

// TakeWindow counts one event unless the last minute already holds limit
// events; then it returns the buckets so the caller can say when to retry.
// The check and the increment are atomic across instances.
func (c *Cluster) TakeWindow(k string, limit int64, now time.Time) (ok bool, buckets []Bucket, err error) {
	if err := c.s.db.QueryRow(`SELECT ai_route_take_window(?, ?, ?, ?)`, k, now.Unix(), limit, windowSpan).Scan(&ok); err != nil {
		return false, nil, err
	}
	if ok {
		return true, nil, nil
	}
	buckets, err = windowBuckets(c.s.db, k, now)
	return false, buckets, err
}

// ---------- in-flight slots (key and provider concurrency caps) ----------

// slotState is this instance's count for one key; mu serializes the
// instance's writes of that row.
type slotState struct {
	mu sync.Mutex
	n  int
}

func (c *Cluster) slot(k string) *slotState {
	c.slotMu.Lock()
	defer c.slotMu.Unlock()
	st, ok := c.slots[k]
	if !ok {
		st = &slotState{}
		c.slots[k] = st
	}
	return st
}

// Acquire takes one of limit slots shared by every live instance. Each
// instance writes only its own row, so a crashed instance's slots simply
// stop counting once its heartbeat is stale. When the database cannot be
// reached the slot is granted (counted locally, written on the next resync)
// and the error returned for logging; every true result needs a Release.
func (c *Cluster) Acquire(k string, limit int) (bool, error) {
	st := c.slot(k)
	st.mu.Lock()
	defer st.mu.Unlock()
	ok, err := c.acquire(k, limit, st.n)
	if err != nil || ok {
		st.n++
		return true, err
	}
	return false, nil
}

func (c *Cluster) acquire(k string, limit, mine int) (ok bool, err error) {
	err = c.s.db.QueryRow(`SELECT ai_route_acquire(?, ?, ?, ?, ?)`, k, c.id, mine, limit, c.aliveSince()).Scan(&ok)
	return ok, err
}

// Release gives back a slot taken by Acquire.
func (c *Cluster) Release(k string) {
	st := c.slot(k)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.n > 0 {
		st.n--
	}
	_, err := c.s.db.Exec(`UPDATE cluster_slots SET n = ? WHERE k = ? AND instance = ?`, st.n, k, c.id)
	c.logErr("release slot", err) // resyncSlots repairs the row later
}

func setSlot(q interface {
	Exec(string, ...any) (sql.Result, error)
}, k, instance string, n int) error {
	_, err := q.Exec(`INSERT INTO cluster_slots (k, instance, n) VALUES (?, ?, ?)
		ON CONFLICT (k, instance) DO UPDATE SET n = excluded.n`, k, instance, n)
	return err
}

// resyncSlots rewrites this instance's rows from memory, repairing any
// write that failed.
func (c *Cluster) resyncSlots() {
	c.slotMu.Lock()
	keys := make(map[string]*slotState, len(c.slots))
	for k, st := range c.slots {
		keys[k] = st
	}
	c.slotMu.Unlock()
	for k, st := range keys {
		st.mu.Lock()
		err := setSlot(c.s.db, k, c.id, st.n)
		st.mu.Unlock()
		if err != nil {
			c.logErr("resync slots", err)
			return
		}
	}
}

// SlotTotals sums in-flight counts over live instances, by key.
func (c *Cluster) SlotTotals(prefix string) (map[string]int, error) {
	rows, err := c.s.db.Query(`SELECT s.k, SUM(s.n) FROM cluster_slots s JOIN cluster_instances i ON i.id = s.instance
		WHERE s.k LIKE ? AND i.seen_at >= ? GROUP BY s.k`, prefix+"%", c.aliveSince())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		if n > 0 {
			out[k[len(prefix):]] = int(n)
		}
	}
	return out, rows.Err()
}

// ---------- circuit breaker ----------

// PublishCooldown shares a breaker change with the other instances. The
// breaker's times are on this machine's clock; the table holds the
// database's.
func (c *Cluster) PublishCooldown(cd Cooldown) error {
	var until int64
	if !cd.OpenUntil.IsZero() {
		until = cd.OpenUntil.Add(c.Offset()).UnixMilli()
	}
	if cd.At.IsZero() {
		cd.At = time.Now()
	}
	var ok bool
	return c.s.db.QueryRow(`SELECT ai_route_publish_breaker(?, ?, ?, ?, ?, ?)`,
		cd.Key, until, cd.Opens, clip(cd.Error, 500), cd.At.Add(c.Offset()).UnixMilli(), c.id).Scan(&ok)
}

// ---------- leases ----------

// Claim takes the named lease for d unless another holder's lease is still
// running: used so one instance sends an alert or runs a periodic job.
func (c *Cluster) Claim(k string, d time.Duration) bool {
	now := c.Now().UnixMilli()
	var got string
	err := c.s.db.QueryRow(`INSERT INTO cluster_leases (k, until_ms, owner) VALUES (?, ?, ?)
		ON CONFLICT (k) DO UPDATE SET until_ms = excluded.until_ms, owner = excluded.owner
		WHERE cluster_leases.until_ms <= ? RETURNING k`, k, now+d.Milliseconds(), c.id, now).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		c.logErr("claim lease", err)
		return true // fail open: a duplicate beats a lost alert or job
	}
	return true
}

// ReleaseLease ends a lease early.
func (c *Cluster) ReleaseLease(k string) {
	_, err := c.s.db.Exec(`DELETE FROM cluster_leases WHERE k = ?`, k)
	c.logErr("release lease", err)
}

// String is used in logs.
func (c *Cluster) String() string { return fmt.Sprintf("instance %s", c.id) }
