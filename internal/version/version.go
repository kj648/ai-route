// Package version holds the release version, bumped when a release is
// tagged; builds can override it with
// -ldflags "-X ai-route/internal/version.Version=...".
package version

var Version = "0.5.0"

// UserAgent is the gateway's own fingerprint sent to upstreams.
func UserAgent() string { return "ai-route/" + Version }
