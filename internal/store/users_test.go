package store

import (
	"errors"
	"strings"
	"testing"
)

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$600000$") || !CheckPassword(h, "correct horse") || CheckPassword(h, "wrong horse") || CheckPassword("garbage", "x") {
		t.Fatalf("hash %q", h)
	}
}

func TestUsersSessionsAndKeys(t *testing.T) {
	s, err := openTest(t)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateUser(&User{Username: "bob", Password: "short"}); err == nil {
		t.Fatal("short password accepted")
	}
	u := &User{Username: " Alice ", Password: "secret-pass", Enabled: true, AllowedModels: []string{"coder"}, MaxKeys: 2}
	if err := s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	if u.Username != "alice" || s.Snapshot().Users[u.ID].PasswordHash != "" {
		t.Fatalf("user %+v / snapshot keeps hash", u)
	}
	if err := s.CreateUser(&User{Username: "alice", Password: "secret-pass"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate: %v", err)
	}

	// sign-in
	if _, _, err := s.Login("alice", "nope-nope"); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, _, err := s.Login("nobody", "secret-pass"); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("unknown user: %v", err)
	}
	tok, _, err := s.Login("ALICE", "secret-pass")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.SessionUser(tok); err != nil || got.ID != u.ID {
		t.Fatalf("session: %v %v", got, err)
	}
	var stored string
	_ = s.db.QueryRow(`SELECT token_hash FROM sessions`).Scan(&stored)
	if stored == tok || stored != TokenHash(tok) {
		t.Fatal("session token stored in plain text")
	}

	// keys: hashed, shown once, within the account's allowance
	k := &APIKey{Name: "laptop", Enabled: true}
	v, err := s.CreateUserKey(u, k)
	if err != nil {
		t.Fatal(err)
	}
	if !k.Hashed() || strings.Contains(k.Key, v) || !strings.HasPrefix(k.Hint, v[:13]) {
		t.Fatalf("key %+v value %s", k, v)
	}
	snap := s.Snapshot()
	if got, ok := snap.LookupKey(v); !ok || got.ID != k.ID || snap.KeyUser(got).ID != u.ID {
		t.Fatal("hashed key not found by its value")
	}
	if _, ok := snap.LookupKey(k.Key); ok {
		t.Fatal("the stored hash works as a key")
	}
	if _, err := s.CreateUserKey(u, &APIKey{Name: "x", AllowedModels: []string{"other"}}); err == nil {
		t.Fatal("key for a model outside the account accepted")
	}
	if _, err := s.CreateUserKey(u, &APIKey{Name: "second", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUserKey(u, &APIKey{Name: "third"}); err == nil || !strings.Contains(err.Error(), "at most 2") {
		t.Fatalf("max keys: %v", err)
	}
	nv, err := s.RotateUserKey(u, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Snapshot().LookupKey(v); ok {
		t.Fatal("old value still works after rotation")
	}
	if got, ok := s.Snapshot().LookupKey(nv); !ok || !got.Hashed() {
		t.Fatal("rotated value not hashed")
	}
	// someone else's key is not theirs to touch
	other := &User{Username: "carol", Password: "secret-pass", Enabled: true}
	if err := s.CreateUser(other); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUserKey(other, k.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user delete: %v", err)
	}
	if _, err := s.RotateUserKey(other, k.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user rotate: %v", err)
	}

	// password change ends other sessions, keeps the current one
	tok2, _, _ := s.Login("alice", "secret-pass")
	if err := s.ChangePassword(u.ID, "wrong", "new-secret-1", ""); err == nil {
		t.Fatal("wrong old password accepted")
	}
	if err := s.ChangePassword(u.ID, "secret-pass", "new-secret-1", TokenHash(tok2)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(tok); err == nil {
		t.Fatal("other session survived a password change")
	}
	if _, err := s.SessionUser(tok2); err != nil {
		t.Fatal("current session ended by a password change")
	}

	// disabling ends sessions and blocks sign-in
	u2 := *s.Snapshot().Users[u.ID]
	u2.Enabled = false
	if err := s.UpdateUser(&u2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(tok2); err == nil {
		t.Fatal("session of a disabled user still valid")
	}
	if _, _, err := s.Login("alice", "new-secret-1"); !errors.Is(err, ErrBadLogin) {
		t.Fatal("disabled user signed in")
	}

	// export / import keep accounts and their hashed keys
	exp, err := s.Export()
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.Users) != 2 || exp.Users[0].PasswordHash == "" {
		t.Fatalf("export users %+v", exp.Users)
	}
	if err := s.Import(exp); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Snapshot().LookupKey(nv); !ok || got.UserID != u.ID {
		t.Fatal("hashed key lost its owner on import")
	}

	// deleting a user deletes their keys
	if err := s.DeleteUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Snapshot().LookupKey(nv); ok {
		t.Fatal("deleted user's key still works")
	}
}
