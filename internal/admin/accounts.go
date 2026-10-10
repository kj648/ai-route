package admin

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ai-route/internal/store"
	"ai-route/internal/version"
)

// Console sign-in: the administrator uses ADMIN_TOKEN (or an account with
// the admin role); other users sign in with a username and password and
// only reach /admin/api/me, /admin/api/logout and /admin/api/my/*. Both
// kinds of credential travel as "Authorization: Bearer ...".

// principal is who is calling the console API.
type principal struct {
	admin   bool
	user    *store.User // nil for ADMIN_TOKEN
	session string      // the session token, for sign-out
}

type principalKey struct{}

func principalFrom(ctx context.Context) *principal {
	p, _ := ctx.Value(principalKey{}).(*principal)
	return p
}

// identify resolves the bearer credential, or returns nil.
func (a *Admin) identify(r *http.Request) (*principal, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		return nil, nil
	}
	if subtle.ConstantTimeCompare([]byte(tok), []byte(a.token)) == 1 {
		return &principal{admin: true}, nil
	}
	u, err := a.store.SessionUser(tok)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return &principal{admin: u.Role == store.RoleAdmin, user: u, session: tok}, nil
}

// authAs wraps a handler: adminOnly requires ADMIN_TOKEN or an admin
// account, otherwise any signed-in user passes. Wrong credentials count
// towards the per-IP lockout; valid ones are never locked out (behind a
// shared NAT, someone else's failures must not shut a real user out).
func (a *Admin) authAs(adminOnly bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.identify(r)
		if err != nil {
			storeErr(w, err)
			return
		}
		if p != nil {
			if adminOnly && !p.admin {
				writeErr(w, http.StatusForbidden, errors.New("administrators only"))
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
			return
		}
		ip := a.clientIP(r)
		if locked, left := a.tooManyFailures(ip); locked {
			w.Header().Set("Retry-After", strconv.Itoa(int(left.Seconds())+1))
			writeErr(w, http.StatusTooManyRequests, errors.New("too many failed logins, try again later"))
			return
		}
		a.recordFailure(ip)
		writeErr(w, http.StatusUnauthorized, errors.New("unauthorized"))
	})
}

func (a *Admin) registerAccounts(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/api/login", a.login)
	signedIn := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, a.authAs(false, h)) }
	signedIn("GET /admin/api/me", a.me)
	signedIn("POST /admin/api/logout", a.logout)
	signedIn("POST /admin/api/my/password", a.changePassword)
	signedIn("GET /admin/api/my/models", a.myModels)
	signedIn("GET /admin/api/my/keys", a.myKeys)
	signedIn("POST /admin/api/my/keys", a.myCreateKey)
	signedIn("PUT /admin/api/my/keys/{id}", a.myUpdateKey)
	signedIn("DELETE /admin/api/my/keys/{id}", a.myDeleteKey)
	signedIn("POST /admin/api/my/keys/{id}/rotate", a.myRotateKey)
	signedIn("GET /admin/api/my/logs", a.myLogs)
	signedIn("GET /admin/api/my/stats", a.myStats)
}

func (a *Admin) registerUserAdmin(api *http.ServeMux) {
	api.HandleFunc("GET /admin/api/users", a.listUsers)
	api.HandleFunc("POST /admin/api/users", a.createUser)
	api.HandleFunc("PUT /admin/api/users/{id}", a.updateUser)
	api.HandleFunc("DELETE /admin/api/users/{id}", a.deleteUser)
}

// ---------- sign-in ----------

func (a *Admin) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// both the address and the account are rate limited: a password
	// guessed from many addresses is still capped per account
	ip, acct := a.clientIP(r), "user:"+strings.ToLower(strings.TrimSpace(req.Username))
	for _, k := range []string{ip, acct} {
		if locked, left := a.tooManyFailures(k); locked {
			w.Header().Set("Retry-After", strconv.Itoa(int(left.Seconds())+1))
			writeErr(w, http.StatusTooManyRequests, errors.New("too many failed logins, try again later"))
			return
		}
	}
	token, u, err := a.store.Login(req.Username, req.Password)
	if errors.Is(err, store.ErrBadLogin) {
		a.recordFailure(ip)
		a.recordFailure(acct)
		writeErr(w, http.StatusUnauthorized, err)
		return
	} else if err != nil {
		storeErr(w, err)
		return
	}
	u.PasswordHash = ""
	writeJSON(w, map[string]any{"token": token, "user": u, "role": u.Role})
}

func (a *Admin) me(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	role := store.RoleUser
	if p.admin {
		role = store.RoleAdmin
	}
	out := map[string]any{"role": role, "user": p.user, "version": version.Version}
	if p.user != nil {
		st := a.store.GetSettings()
		spend, _ := a.store.UserSpend(store.MonthStart(time.Now()))
		out["month_cost"], out["currency"] = spend[p.user.ID], st.Currency
	}
	writeJSON(w, out)
}

func (a *Admin) logout(w http.ResponseWriter, r *http.Request) {
	if p := principalFrom(r.Context()); p.session != "" {
		if err := a.store.Logout(p.session); err != nil {
			storeErr(w, err)
			return
		}
	}
	writeJSON(w, map[string]any{"ok": true})
}

// account returns the signed-in user, or answers 400 for ADMIN_TOKEN,
// which has no account of its own.
func account(w http.ResponseWriter, r *http.Request) *store.User {
	p := principalFrom(r.Context())
	if p.user == nil {
		writeErr(w, http.StatusBadRequest, errors.New("signed in with ADMIN_TOKEN: there is no account; sign in with a username to use this"))
		return nil
	}
	return p.user
}

func (a *Admin) changePassword(w http.ResponseWriter, r *http.Request) {
	u := account(w, r)
	if u == nil {
		return
	}
	var req struct {
		Old string `json:"old_password"`
		New string `json:"new_password"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.store.ChangePassword(u.ID, req.Old, req.New, store.TokenHash(principalFrom(r.Context()).session)); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---------- a user's own view ----------

// myModels lists the public models the account may use: names and
// descriptions only, never where they are routed.
func (a *Admin) myModels(w http.ResponseWriter, r *http.Request) {
	u := account(w, r)
	if u == nil {
		return
	}
	type modelView struct {
		Name        string   `json:"name"`
		Aliases     []string `json:"aliases"`
		Tags        []string `json:"tags"`
		Description string   `json:"description"`
	}
	out := []modelView{}
	for _, m := range a.store.Snapshot().Models {
		if m.Enabled && u.Allows(m.Name) {
			out = append(out, modelView{m.Name, m.Aliases, m.Tags, m.Description})
		}
	}
	writeJSON(w, out)
}

// myKeyView never carries the key value (only its hint) after creation.
type myKeyView struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	Hint          string   `json:"hint"`
	Enabled       bool     `json:"enabled"`
	AllowedModels []string `json:"allowed_models"`
	ExpiresAt     int64    `json:"expires_at"`
	MonthlyBudget float64  `json:"monthly_budget"`
	CreatedAt     int64    `json:"created_at"`
	LastUsedAt    int64    `json:"last_used_at"`
	MonthCost     float64  `json:"month_cost"`
	Key           string   `json:"key,omitempty"` // only right after creation / rotation
}

func toMyKey(k *store.APIKey, cost float64) myKeyView {
	return myKeyView{ID: k.ID, Name: k.Name, Hint: k.Hint, Enabled: k.Enabled, AllowedModels: k.AllowedModels, ExpiresAt: k.ExpiresAt,
		MonthlyBudget: k.MonthlyBudget, CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt, MonthCost: cost}
}

func (a *Admin) myKeys(w http.ResponseWriter, r *http.Request) {
	u := account(w, r)
	if u == nil {
		return
	}
	ks, err := a.store.ListUserKeys(u.ID)
	if err != nil {
		storeErr(w, err)
		return
	}
	spend, err := a.store.KeySpend(store.MonthStart(time.Now()))
	if err != nil {
		storeErr(w, err)
		return
	}
	out := make([]myKeyView, 0, len(ks))
	for _, k := range ks {
		out = append(out, toMyKey(k, spend[k.ID]))
	}
	writeJSON(w, out)
}

func (a *Admin) myCreateKey(w http.ResponseWriter, r *http.Request) {
	u := account(w, r)
	if u == nil {
		return
	}
	var k store.APIKey
	if err := decode(r, &k); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	v, err := a.store.CreateUserKey(u, &k)
	if err != nil {
		storeErr(w, err)
		return
	}
	view := toMyKey(&k, 0)
	view.Key = v
	writeJSON(w, view)
}

func (a *Admin) myUpdateKey(w http.ResponseWriter, r *http.Request) {
	u := account(w, r)
	if u == nil {
		return
	}
	var k store.APIKey
	if err := decode(r, &k); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	k.ID = pathID(r)
	if err := a.store.UpdateUserKey(u, &k); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *Admin) myDeleteKey(w http.ResponseWriter, r *http.Request) {
	u := account(w, r)
	if u == nil {
		return
	}
	if err := a.store.DeleteUserKey(u, pathID(r)); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *Admin) myRotateKey(w http.ResponseWriter, r *http.Request) {
	u := account(w, r)
	if u == nil {
		return
	}
	v, err := a.store.RotateUserKey(u, pathID(r))
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"key": v})
}

// myLog is a request as its user sees it: no upstream, routing or
// upstream error details.
type myLog struct {
	ID             int64   `json:"id"`
	CreatedAt      int64   `json:"created_at"`
	KeyID          int64   `json:"key_id"`
	KeyName        string  `json:"key_name"`
	RequestedModel string  `json:"requested_model"`
	PublicModel    string  `json:"public_model"`
	Inbound        string  `json:"inbound"`
	Stream         bool    `json:"stream"`
	Success        bool    `json:"success"`
	HTTPStatus     int     `json:"http_status"`
	LatencyMs      int64   `json:"latency_ms"`
	TTFBMs         int64   `json:"ttfb_ms"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	CachedTokens   int64   `json:"cached_tokens"`
	Cost           float64 `json:"cost"` // display currency
	Error          string  `json:"error"`
	RequestID      string  `json:"request_id"`
}

// userError is what a user may see of a failure: the gateway's own
// refusals in full, upstream failures only as such.
func userError(l *store.RequestLog) string {
	switch {
	case l.Success || l.Error == "":
		return ""
	case l.Provider == "" && len(l.Attempts) == 0:
		return l.Error // refused by the gateway itself (limits, model, auth)
	case l.HTTPStatus == 499:
		return "client disconnected"
	default:
		return "the upstream request failed; give the request ID to your administrator for details"
	}
}

func (a *Admin) myLogs(w http.ResponseWriter, r *http.Request) {
	u := account(w, r)
	if u == nil {
		return
	}
	q := r.URL.Query()
	lq := store.LogQuery{UserID: u.ID, Model: q.Get("model"), Status: q.Get("status")}
	lq.KeyID, _ = strconv.ParseInt(q.Get("key_id"), 10, 64)
	lq.Limit, _ = strconv.Atoi(q.Get("limit"))
	lq.Offset, _ = strconv.Atoi(q.Get("offset"))
	logs, total, err := a.store.QueryLogs(lq)
	if err != nil {
		storeErr(w, err)
		return
	}
	st := a.store.GetSettings()
	items := make([]myLog, 0, len(logs))
	for _, l := range logs {
		items = append(items, myLog{ID: l.ID, CreatedAt: l.CreatedAt, KeyID: l.KeyID, KeyName: l.KeyName, RequestedModel: l.RequestedModel,
			PublicModel: l.PublicModel, Inbound: l.Inbound, Stream: l.Stream, Success: l.Success, HTTPStatus: l.HTTPStatus,
			LatencyMs: l.LatencyMs, TTFBMs: l.TTFBMs, InputTokens: l.InputTokens, OutputTokens: l.OutputTokens, CachedTokens: l.CachedTokens,
			Cost: st.ToDisplayCurrency(l.Cost, l.Currency), Error: userError(l), RequestID: l.RequestID})
	}
	writeJSON(w, map[string]any{"total": total, "items": items, "currency": st.Currency})
}

func (a *Admin) myStats(w http.ResponseWriter, r *http.Request) {
	u := account(w, r)
	if u == nil {
		return
	}
	d, bucket := statsRange(r.URL.Query().Get("range"))
	st, err := a.store.GetUserStats(time.Now().Add(-d).UnixMilli(), bucket.Milliseconds(), u.ID)
	if err != nil {
		storeErr(w, err)
		return
	}
	// where requests went is the administrator's business
	st.ByProvider, st.ByTarget = []store.StatRow{}, []store.StatRow{}
	writeJSON(w, st)
}

// ---------- user administration ----------

type userView struct {
	*store.User
	Keys      int     `json:"keys"`
	MonthCost float64 `json:"month_cost"`
}

func (a *Admin) listUsers(w http.ResponseWriter, r *http.Request) {
	us, err := a.store.ListUsers()
	if err != nil {
		storeErr(w, err)
		return
	}
	spend, err := a.store.UserSpend(store.MonthStart(time.Now()))
	if err != nil {
		storeErr(w, err)
		return
	}
	count := map[int64]int{}
	for _, k := range a.store.Snapshot().Keys {
		count[k.UserID]++
	}
	out := make([]userView, 0, len(us))
	for _, u := range us {
		u.PasswordHash = ""
		out = append(out, userView{u, count[u.ID], spend[u.ID]})
	}
	writeJSON(w, out)
}

func (a *Admin) createUser(w http.ResponseWriter, r *http.Request) {
	var u store.User
	if err := decode(r, &u); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.store.CreateUser(&u); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"id": u.ID})
}

// self reports whether the caller is editing their own account.
func self(r *http.Request, id int64) bool {
	p := principalFrom(r.Context())
	return p != nil && p.user != nil && p.user.ID == id
}

func (a *Admin) updateUser(w http.ResponseWriter, r *http.Request) {
	var u store.User
	if err := decode(r, &u); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	u.ID = pathID(r)
	if self(r, u.ID) && (u.Role != store.RoleAdmin || !u.Enabled) {
		writeErr(w, http.StatusBadRequest, errors.New("you cannot disable your own account or remove your own admin role"))
		return
	}
	if err := a.store.UpdateUser(&u); err != nil {
		storeErr(w, err)
		return
	}
	a.gw.Limiter.Forget()
	writeJSON(w, map[string]any{"ok": true})
}

func (a *Admin) deleteUser(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if self(r, id) {
		writeErr(w, http.StatusBadRequest, errors.New("you cannot delete your own account"))
		return
	}
	if err := a.store.DeleteUser(id); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
