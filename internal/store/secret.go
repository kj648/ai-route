package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// Secrets at rest. With SECRET_KEY set, upstream API keys, alert webhook
// URLs and secrets, and captured request bodies are stored sealed with
// AES-256-GCM ("enc:v1:<base64 nonce|ciphertext>"); the snapshot and the
// admin API see them in plain text. Values written before the key was set
// are sealed on the next start, and SECRET_KEY_PREVIOUS lets a key be
// rotated (or encryption turned off) the same way.

const sealedPrefix = "enc:v1:"

// MinSecretKeyLen is the shortest SECRET_KEY accepted.
const MinSecretKeyLen = 16

// ErrSecretKeyMissing means the database holds sealed values but no key
// that opens them was given.
var ErrSecretKeyMissing = errors.New("the database holds encrypted secrets: set SECRET_KEY (and SECRET_KEY_PREVIOUS while rotating) to the key they were written with")

// Option configures a Store when it is opened.
type Option func(*options)

type options struct {
	secretKey, previousKey string
}

// WithSecretKey seals secrets with key; values sealed with previous (the
// key before a rotation) can still be read and are re-sealed on open.
// key "" with a previous key decrypts everything back to plain text.
func WithSecretKey(key, previous string) Option {
	return func(o *options) { o.secretKey, o.previousKey = key, previous }
}

// sealer seals with cur (nil: store plain text) and opens with any key.
type sealer struct {
	cur  cipher.AEAD
	keys []cipher.AEAD // cur first, then the previous key
}

func newAEAD(key string) (cipher.AEAD, error) {
	if len(key) < MinSecretKeyLen {
		return nil, fmt.Errorf("secret key too short: use at least %d random characters (e.g. `openssl rand -hex 32`)", MinSecretKeyLen)
	}
	k, err := hkdf.Key(sha256.New, []byte(key), nil, "ai-route secrets v1", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func newSealer(o options) (*sealer, error) {
	s := &sealer{}
	if o.secretKey != "" {
		a, err := newAEAD(o.secretKey)
		if err != nil {
			return nil, fmt.Errorf("SECRET_KEY: %w", err)
		}
		s.cur = a
		s.keys = append(s.keys, a)
	}
	if o.previousKey != "" && o.previousKey != o.secretKey {
		a, err := newAEAD(o.previousKey)
		if err != nil {
			return nil, fmt.Errorf("SECRET_KEY_PREVIOUS: %w", err)
		}
		s.keys = append(s.keys, a)
	}
	return s, nil
}

// IsSealed reports whether v is an encrypted value.
func IsSealed(v string) bool { return strings.HasPrefix(v, sealedPrefix) }

// seal encrypts v with the current key; without one, or for "", v is
// returned as is.
func (s *sealer) seal(v string) string {
	if s == nil || s.cur == nil || v == "" || IsSealed(v) {
		return v
	}
	nonce := make([]byte, s.cur.NonceSize())
	_, _ = rand.Read(nonce)
	return sealedPrefix + base64.RawStdEncoding.EncodeToString(s.cur.Seal(nonce, nonce, []byte(v), nil))
}

// open decrypts a sealed value; plain values are returned as is.
func (s *sealer) open(v string) (string, error) {
	if !IsSealed(v) {
		return v, nil
	}
	if s == nil || len(s.keys) == 0 {
		return "", ErrSecretKeyMissing
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(v, sealedPrefix))
	if err != nil {
		return "", fmt.Errorf("corrupt encrypted value: %w", err)
	}
	for _, a := range s.keys {
		if len(raw) < a.NonceSize() {
			break
		}
		if plain, err := a.Open(nil, raw[:a.NonceSize()], raw[a.NonceSize():], nil); err == nil {
			return string(plain), nil
		}
	}
	return "", errors.New("encrypted secrets do not open with SECRET_KEY (or SECRET_KEY_PREVIOUS): wrong key")
}

// stale reports whether a stored value is not in the form it should be
// stored in (plain while a key is set, sealed with an older key, or
// sealed while encryption is being turned off).
func (s *sealer) stale(v string) bool {
	if v == "" {
		return false
	}
	if !IsSealed(v) {
		return s.cur != nil
	}
	if s.cur == nil {
		return true
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(v, sealedPrefix))
	if err != nil || len(raw) < s.cur.NonceSize() {
		return true
	}
	_, err = s.cur.Open(nil, raw[:s.cur.NonceSize()], raw[s.cur.NonceSize():], nil)
	return err != nil
}

// Encrypted reports whether secrets are being sealed (SECRET_KEY is set).
func (s *Store) Encrypted() bool { return s.sec.cur != nil }

// sealAlerts / openAlerts handle the secret parts of an alert config:
// webhook URLs carry access tokens, signing secrets are secrets.
func (s *sealer) sealAlerts(a AlertConfig) AlertConfig {
	hooks := make([]Webhook, len(a.Webhooks))
	for i, h := range a.Webhooks {
		h.URL, h.Secret = s.seal(h.URL), s.seal(h.Secret)
		hooks[i] = h
	}
	a.Webhooks = hooks
	return a
}

func (s *sealer) openAlerts(a *AlertConfig) error {
	for i := range a.Webhooks {
		h := &a.Webhooks[i]
		var err error
		if h.URL, err = s.open(h.URL); err != nil {
			return err
		}
		if h.Secret, err = s.open(h.Secret); err != nil {
			return err
		}
	}
	return nil
}

// resealSecrets rewrites every stored secret that is not in its proper
// form (see stale). It runs on open, before the first snapshot.
func (s *Store) resealSecrets() error {
	rows, err := s.db.Query(`SELECT id, api_key FROM providers`)
	if err != nil {
		return err
	}
	type upd struct {
		id  int64
		key string
	}
	var updates []upd
	for rows.Next() {
		var u upd
		if err := rows.Scan(&u.id, &u.key); err != nil {
			rows.Close()
			return err
		}
		if s.sec.stale(u.key) {
			updates = append(updates, u)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, u := range updates {
		plain, err := s.sec.open(u.key)
		if err != nil {
			return err
		}
		if _, err := s.db.Exec(`UPDATE providers SET api_key=? WHERE id=?`, s.sec.seal(plain), u.id); err != nil {
			return err
		}
	}
	a, err := s.getAlerts() // opened
	if err != nil {
		return err
	}
	var raw string
	if s.db.QueryRow(`SELECT v FROM settings WHERE k='alerts'`).Scan(&raw) == nil {
		var stored AlertConfig
		_ = json.Unmarshal([]byte(raw), &stored)
		resealed := false
		for _, h := range stored.Webhooks {
			if s.sec.stale(h.URL) || s.sec.stale(h.Secret) {
				resealed = true
			}
		}
		if resealed {
			if err := s.SetKV("alerts", mustJSON(s.sec.sealAlerts(a))); err != nil {
				return err
			}
		}
	}
	if n := len(updates); n > 0 {
		if s.sec.cur != nil {
			slog.Info("store: sealed upstream API keys with SECRET_KEY", "providers", n)
		} else {
			slog.Info("store: decrypted upstream API keys (SECRET_KEY not set)", "providers", n)
		}
	}
	return nil
}
