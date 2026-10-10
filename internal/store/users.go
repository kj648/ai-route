package store

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Accounts: administrators create users, who sign in to the console with
// a username and password, see the models they may use, and manage their
// own API keys and logs. A user's limits cap every key they create. Users
// with the admin role get the whole console, like ADMIN_TOKEN.

const (
	RoleUser  = "user"
	RoleAdmin = "admin"

	// SessionTTL is how long a console sign-in lasts.
	SessionTTL = 7 * 24 * time.Hour
	// MinPasswordLen is the shortest password accepted.
	MinPasswordLen = 8
	// pbkdf2Iter follows OWASP's recommendation for PBKDF2-HMAC-SHA256.
	pbkdf2Iter = 600_000
)

const usersSchema = `
CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	username TEXT NOT NULL UNIQUE,
	display_name TEXT NOT NULL DEFAULT '',
	password_hash TEXT NOT NULL DEFAULT '',
	role TEXT NOT NULL DEFAULT 'user',
	enabled INTEGER NOT NULL DEFAULT 1,
	allowed_models TEXT NOT NULL DEFAULT '[]',
	monthly_budget REAL NOT NULL DEFAULT 0,
	rpm INTEGER NOT NULL DEFAULT 0,
	tpm INTEGER NOT NULL DEFAULT 0,
	max_keys INTEGER NOT NULL DEFAULT 0,
	remark TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	last_login_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
`

// User is a console account.
type User struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"` // user | admin
	Enabled     bool   `json:"enabled"`
	// AllowedModels limits the public models the user's keys may use
	// (empty = all); MonthlyBudget (display currency), RPM and TPM cap all
	// of the user's keys together; MaxKeys caps how many keys the user may
	// create (0 = unlimited). 0 = unlimited for the others too.
	AllowedModels []string `json:"allowed_models"`
	MonthlyBudget float64  `json:"monthly_budget"`
	RPM           int      `json:"rpm"`
	TPM           int      `json:"tpm"`
	MaxKeys       int      `json:"max_keys"`
	Remark        string   `json:"remark"`
	CreatedAt     int64    `json:"created_at"`
	LastLoginAt   int64    `json:"last_login_at"`

	// PasswordHash is never sent to the console; exports carry it.
	PasswordHash string `json:"password_hash,omitempty"`
	// Password is only read when creating a user or setting a new one.
	Password string `json:"password,omitempty"`
}

// Allows reports whether the user may use a public model.
func (u *User) Allows(model string) bool {
	if u == nil || len(u.AllowedModels) == 0 {
		return true
	}
	for _, m := range u.AllowedModels {
		if m == model {
			return true
		}
	}
	return false
}

// ---------- passwords ----------

// HashPassword returns a salted PBKDF2-SHA256 hash of a password.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// CheckPassword reports whether pw matches a hash from HashPassword.
func CheckPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash keeps a login for an unknown username as slow as a wrong
// password, so timing does not reveal which usernames exist.
var dummyHash = sync.OnceValue(func() string {
	h, _ := HashPassword("ai-route-no-such-user")
	return h
})

func validPassword(pw string) error {
	if utf8.RuneCountInString(pw) < MinPasswordLen {
		return fmt.Errorf("password must have at least %d characters", MinPasswordLen)
	}
	if len(pw) > 256 {
		return errors.New("password too long")
	}
	return nil
}

// ---------- users ----------

const userCols = `id, username, display_name, password_hash, role, enabled, allowed_models, monthly_budget, rpm, tpm, max_keys, remark, created_at, last_login_at`

func scanUser(sc interface{ Scan(...any) error }) (*User, error) {
	u := &User{}
	var allowed string
	var enabled int
	if err := sc.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.Role, &enabled, &allowed, &u.MonthlyBudget, &u.RPM, &u.TPM, &u.MaxKeys, &u.Remark, &u.CreatedAt, &u.LastLoginAt); err != nil {
		return nil, err
	}
	u.Enabled = enabled == 1
	_ = json.Unmarshal([]byte(allowed), &u.AllowedModels)
	if u.AllowedModels == nil {
		u.AllowedModels = []string{}
	}
	return u, nil
}

func (s *Store) listUsers() ([]*User, error) {
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListUsers returns every user (with password hashes: strip them before
// sending users anywhere).
func (s *Store) ListUsers() ([]*User, error) { return s.listUsers() }

// GetUser returns one user.
func (s *Store) GetUser(id int64) (*User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func normalizeUser(u *User) error {
	u.Username = strings.ToLower(strings.TrimSpace(u.Username))
	if u.Username == "" {
		return errors.New("username is required")
	}
	if len(u.Username) > 64 || strings.ContainsAny(u.Username, " \t\r\n/\\:") {
		return errors.New("username must be at most 64 characters without spaces, slashes or colons")
	}
	u.DisplayName = strings.TrimSpace(u.DisplayName)
	switch u.Role {
	case "":
		u.Role = RoleUser
	case RoleUser, RoleAdmin:
	default:
		return fmt.Errorf("role must be user or admin, got %q", u.Role)
	}
	u.AllowedModels = cleanList(u.AllowedModels)
	if u.MonthlyBudget < 0 || u.RPM < 0 || u.TPM < 0 || u.MaxKeys < 0 {
		return errors.New("monthly_budget, rpm, tpm and max_keys must not be negative")
	}
	u.Remark = strings.TrimSpace(u.Remark)
	return nil
}

// CreateUser stores a new user; Password is required.
func (s *Store) CreateUser(u *User) error {
	if err := normalizeUser(u); err != nil {
		return err
	}
	if err := validPassword(u.Password); err != nil {
		return err
	}
	hash, err := HashPassword(u.Password)
	if err != nil {
		return err
	}
	u.Password, u.PasswordHash = "", hash
	s.mu.Lock()
	defer s.mu.Unlock()
	u.CreatedAt = now()
	if err := s.db.QueryRow(`INSERT INTO users (username, display_name, password_hash, role, enabled, allowed_models, monthly_budget, rpm, tpm, max_keys, remark, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`,
		u.Username, u.DisplayName, u.PasswordHash, u.Role, b2i(u.Enabled), mustJSON(u.AllowedModels), u.MonthlyBudget, u.RPM, u.TPM, u.MaxKeys, u.Remark, u.CreatedAt).Scan(&u.ID); err != nil {
		return friendlyErr(err)
	}
	return s.reloadLocked()
}

// UpdateUser changes a user's profile, role and limits; a non-empty
// Password sets a new password and signs the user out everywhere.
// Disabling a user also ends their sessions.
func (s *Store) UpdateUser(u *User) error {
	if err := normalizeUser(u); err != nil {
		return err
	}
	var hash string
	if u.Password != "" {
		if err := validPassword(u.Password); err != nil {
			return err
		}
		var err error
		if hash, err = HashPassword(u.Password); err != nil {
			return err
		}
		u.Password = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE users SET username=?, display_name=?, role=?, enabled=?, allowed_models=?, monthly_budget=?, rpm=?, tpm=?, max_keys=?, remark=? WHERE id=?`,
		u.Username, u.DisplayName, u.Role, b2i(u.Enabled), mustJSON(u.AllowedModels), u.MonthlyBudget, u.RPM, u.TPM, u.MaxKeys, u.Remark, u.ID)
	if err != nil {
		return friendlyErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if hash != "" {
		if _, err := tx.Exec(`UPDATE users SET password_hash=? WHERE id=?`, hash, u.ID); err != nil {
			return err
		}
	}
	if hash != "" || !u.Enabled {
		if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id=?`, u.ID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.reloadLocked()
}

// DeleteUser removes a user with their API keys and sessions; their
// request logs stay.
func (s *Store) DeleteUser(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM users WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	for _, q := range []string{`DELETE FROM api_keys WHERE user_id=?`, `DELETE FROM sessions WHERE user_id=?`} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.reloadLocked()
}

// ChangePassword sets a user's own new password after checking the old
// one; other sessions are ended, keepSession (a token hash) stays.
func (s *Store) ChangePassword(id int64, oldPw, newPw, keepSession string) error {
	u, err := s.GetUser(id)
	if err != nil {
		return err
	}
	if !CheckPassword(u.PasswordHash, oldPw) {
		return errors.New("current password is wrong")
	}
	if err := validPassword(newPw); err != nil {
		return err
	}
	hash, err := HashPassword(newPw)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE users SET password_hash=? WHERE id=?`, hash, id); err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM sessions WHERE user_id=? AND token_hash<>?`, id, keepSession)
	return err
}

// ---------- sessions ----------

// ErrBadLogin is returned for an unknown user, a wrong password or a
// disabled account alike.
var ErrBadLogin = errors.New("wrong username or password")

// TokenHash is how session tokens and hashed API keys are stored.
func TokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// Login checks a username and password and starts a session; the token is
// only returned here (the database keeps its hash).
func (s *Store) Login(username, password string) (string, *User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE username=?`, strings.ToLower(strings.TrimSpace(username))))
	if errors.Is(err, sql.ErrNoRows) {
		CheckPassword(dummyHash(), password)
		return "", nil, ErrBadLogin
	} else if err != nil {
		return "", nil, err
	}
	if !CheckPassword(u.PasswordHash, password) || !u.Enabled {
		return "", nil, ErrBadLogin
	}
	token := RandomToken("ses-", 32)
	t := now()
	if _, err := s.db.Exec(`INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?,?,?,?)`,
		TokenHash(token), u.ID, t, t+SessionTTL.Milliseconds()); err != nil {
		return "", nil, err
	}
	_, _ = s.db.Exec(`UPDATE users SET last_login_at=? WHERE id=?`, t, u.ID)
	u.LastLoginAt = t
	return token, u, nil
}

// SessionUser returns the enabled user a session token belongs to, or
// ErrNotFound.
func (s *Store) SessionUser(token string) (*User, error) {
	if !strings.HasPrefix(token, "ses-") {
		return nil, ErrNotFound
	}
	var uid, expires int64
	err := s.db.QueryRow(`SELECT user_id, expires_at FROM sessions WHERE token_hash=?`, TokenHash(token)).Scan(&uid, &expires)
	if errors.Is(err, sql.ErrNoRows) || err == nil && expires <= now() {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	u, ok := s.Snapshot().Users[uid]
	if !ok || !u.Enabled {
		return nil, ErrNotFound
	}
	return u, nil
}

// Logout ends a session.
func (s *Store) Logout(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash=?`, TokenHash(token))
	return err
}

func (s *Store) cleanupSessions() {
	_, _ = s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now())
}

// UserSpend returns each user's cost since the given unix ms (rounded down
// to the hour), in the display currency, from the hourly rollup.
func (s *Store) UserSpend(since int64) (map[int64]float64, error) {
	usd, cny := s.GetSettings().CurrencyFactors()
	rows, err := s.db.Query(`SELECT user_id, SUM(cost_usd * CAST(? AS DOUBLE PRECISION) + cost_cny * CAST(? AS DOUBLE PRECISION))
		FROM request_stats WHERE hour >= ? AND user_id > 0 AND (cost_usd > 0 OR cost_cny > 0) GROUP BY user_id`, usd, cny, since-since%hourMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]float64{}
	for rows.Next() {
		var id int64
		var v float64
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = v
	}
	return out, rows.Err()
}

// ---------- a user's own API keys ----------

// ListUserKeys returns the keys a user created.
func (s *Store) ListUserKeys(userID int64) ([]*APIKey, error) {
	rows, err := s.db.Query(`SELECT `+keyCols+` FROM api_keys WHERE user_id=? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*APIKey{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// checkUserKey keeps a user's key within the user's own allowance.
func checkUserKey(u *User, k *APIKey) error {
	if err := normalizeKey(k); err != nil {
		return err
	}
	for _, m := range k.AllowedModels {
		if !u.Allows(m) {
			return fmt.Errorf("model %q is not available to your account", m)
		}
	}
	return nil
}

// CreateUserKey creates a key for a user. Only its hash is stored: the
// value is returned in k.Key this once. Rate and concurrency limits come
// from the account; a user can narrow models, expiry and budget.
func (s *Store) CreateUserKey(u *User, k *APIKey) (string, error) {
	k.RPM, k.TPM, k.MaxConcurrency = 0, 0, 0
	if err := checkUserKey(u, k); err != nil {
		return "", err
	}
	v := NewAPIKeyValue()
	s.mu.Lock()
	defer s.mu.Unlock()
	if u.MaxKeys > 0 {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE user_id=?`, u.ID).Scan(&n); err != nil {
			return "", err
		}
		if n >= u.MaxKeys {
			return "", fmt.Errorf("your account may have at most %d API keys", u.MaxKeys)
		}
	}
	k.UserID, k.Hint, k.CreatedAt = u.ID, keyHint(v), now()
	k.Key = HashedKeyPrefix + TokenHash(v)
	if err := s.db.QueryRow(`INSERT INTO api_keys (name, key, enabled, allowed_models, expires_at, monthly_budget, rpm, tpm, max_concurrency, created_at, user_id, hint) VALUES (?,?,?,?,?,?,0,0,0,?,?,?) RETURNING id`,
		k.Name, k.Key, b2i(k.Enabled), mustJSON(k.AllowedModels), k.ExpiresAt, k.MonthlyBudget, k.CreatedAt, k.UserID, k.Hint).Scan(&k.ID); err != nil {
		return "", friendlyErr(err)
	}
	return v, s.reloadLocked()
}

// UpdateUserKey changes a user's own key: name, enabled, models, expiry,
// budget.
func (s *Store) UpdateUserKey(u *User, k *APIKey) error {
	if err := checkUserKey(u, k); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE api_keys SET name=?, enabled=?, allowed_models=?, expires_at=?, monthly_budget=? WHERE id=? AND user_id=?`,
		k.Name, b2i(k.Enabled), mustJSON(k.AllowedModels), k.ExpiresAt, k.MonthlyBudget, k.ID, u.ID)
	if err != nil {
		return friendlyErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return s.reloadLocked()
}

// RotateUserKey gives a user's own key a new value (shown once).
func (s *Store) RotateUserKey(u *User, id int64) (string, error) { return s.rotateKey(id, u.ID) }

// DeleteUserKey deletes a user's own key.
func (s *Store) DeleteUserKey(u *User, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM api_keys WHERE id=? AND user_id=?`, id, u.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return s.reloadLocked()
}
