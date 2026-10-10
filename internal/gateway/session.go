package gateway

import (
	"net/http"
	"strconv"
	"strings"
)

// Sessions: a caller marks the requests that belong together (one chat, one
// agent job) with X-Session-Id. The gateway keeps a session on the same
// upstream within a priority level, hands it to providers as $session (e.g.
// OpenCode Go's x-opencode-session) and records it in the request log, so
// callers never deal with each vendor's own session header.

// SessionHeader is the gateway's session header.
const SessionHeader = "X-Session-Id"

// sessionHeaders are read in order: the gateway's own header, then clients'
// native ones (Claude Code, Codex, OpenCode).
var sessionHeaders = []string{SessionHeader, "X-Claude-Code-Session-Id", "Session-Id", "X-Opencode-Session"}

// maxSessionID caps the caller's session id.
const maxSessionID = 128

// clientSession returns the caller's session id; "" when it sent none.
func clientSession(h http.Header) string {
	for _, k := range sessionHeaders {
		if v := strings.TrimSpace(h.Get(k)); v != "" {
			if len(v) > maxSessionID {
				v = v[:maxSessionID]
			}
			return v
		}
	}
	return ""
}

// sessionAffinity scopes a session to the API key: the same id sent with
// two keys is two sessions. firstMessage stands in when the caller sent no
// session id; both empty means no affinity.
func sessionAffinity(keyID int64, session, firstMessage string) string {
	k := strconv.FormatInt(keyID, 10)
	switch {
	case session != "":
		return k + "\x00session\x00" + session
	case firstMessage != "":
		return k + "\x00" + firstMessage
	}
	return ""
}
