package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"ai-route/internal/store"
)

// Request capture (see store/capture.go): while a capture rule is active,
// matching requests keep their full bodies: the client's request, each
// upstream request and response, and what the client got back. Requests
// that match no rule pay nothing.

// capBuf keeps the first MaxCaptureBody bytes written to it.
type capBuf struct {
	b         []byte
	truncated bool
}

func (c *capBuf) Write(p []byte) (int, error) {
	room := store.MaxCaptureBody - len(c.b)
	if len(p) > room {
		c.truncated = true
		p = p[:max(room, 0)]
	}
	c.b = append(c.b, p...)
	return len(p), nil
}

func (c *capBuf) String() string { return string(c.b) }

// teeBody copies what is read from an upstream body into a capBuf.
type teeBody struct {
	io.ReadCloser
	buf *capBuf
}

func (t teeBody) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	_, _ = t.buf.Write(p[:n])
	return n, err
}

// captureWriter records the response sent to the client. Unwrap keeps
// http.ResponseController (flush, write deadlines) working.
type captureWriter struct {
	http.ResponseWriter
	status int
	buf    capBuf
}

func (w *captureWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *captureWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_, _ = w.buf.Write(p)
	return w.ResponseWriter.Write(p)
}

func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// capture collects one request's bodies. Every part of a request runs on
// the request's goroutine, so it needs no lock.
type capture struct {
	rec       store.Capture
	data      store.CaptureData
	client    *captureWriter
	truncated bool
}

// captureAttempt is an upstream call being recorded.
type captureAttempt struct {
	c   *capture
	i   int
	buf *capBuf // the upstream response, as read
}

func clipBody(b []byte) (string, bool) {
	if len(b) > store.MaxCaptureBody {
		return string(b[:store.MaxCaptureBody]), true
	}
	return string(b), false
}

// startCapture claims a slot of the first active rule matching the request
// and wraps w to record the response; nil when nothing is captured.
func (g *Gateway) startCapture(snap *store.Snapshot, w http.ResponseWriter, key *store.APIKey, model, inbound, requestID string, body []byte) *capture {
	if len(snap.Captures) == 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	for _, r := range snap.Captures {
		if !r.Active(now) || !r.Matches(key.ID, model) || g.captureUsedUp(r.ID) {
			continue
		}
		if !g.store.TakeCapture(r.ID) {
			g.capUsed.Store(r.ID, true)
			continue
		}
		c := &capture{rec: store.Capture{RuleID: r.ID, RequestID: requestID, CreatedAt: now, KeyID: key.ID, KeyName: key.Name, Model: model, Inbound: inbound},
			client: &captureWriter{ResponseWriter: w}}
		c.data.ClientRequest, c.truncated = clipBody(body)
		return c
	}
	return nil
}

// WaitCaptures waits for captures still being saved.
func (g *Gateway) WaitCaptures() { g.capWG.Wait() }

func (g *Gateway) captureUsedUp(id int64) bool {
	_, ok := g.capUsed.Load(id)
	return ok
}

// requestBody reads back the body of an upstream request.
func requestBody(req *http.Request) []byte {
	if req == nil || req.GetBody == nil {
		return nil
	}
	rc, err := req.GetBody()
	if err != nil {
		return nil
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return b
}

// attempt starts recording an upstream call.
func (c *capture) attempt(target, proto, url string, req []byte) *captureAttempt {
	body, cut := clipBody(req)
	c.truncated = c.truncated || cut
	c.data.Attempts = append(c.data.Attempts, store.CaptureAttempt{Target: target, Protocol: proto, URL: url, Request: body})
	return &captureAttempt{c: c, i: len(c.data.Attempts) - 1}
}

// tee records an upstream body while it is read.
func (a *captureAttempt) tee(body io.ReadCloser) io.ReadCloser {
	if a == nil {
		return body
	}
	a.buf = &capBuf{}
	return teeBody{ReadCloser: body, buf: a.buf}
}

// response records an upstream answer read in full.
func (a *captureAttempt) response(b []byte) {
	if a == nil {
		return
	}
	a.buf = &capBuf{}
	_, _ = a.buf.Write(b)
}

// done fills in the attempt's status, and its error when nothing was read.
func (a *captureAttempt) done(status int, errMsg string) {
	if a == nil {
		return
	}
	at := &a.c.data.Attempts[a.i]
	at.Status = status
	if a.buf != nil {
		at.Response = a.buf.String()
		a.c.truncated = a.c.truncated || a.buf.truncated
	}
	if at.Response == "" && errMsg != "" {
		at.Response = "(" + errMsg + ")"
	}
}

// saveCapture stores the capture once the request is finished.
func (g *Gateway) saveCapture(c *capture) {
	c.rec.Status = c.client.status
	c.data.ClientResponse = c.client.buf.String()
	c.data.Truncated = c.truncated || c.client.buf.truncated
	c.rec.CaptureData = &c.data
	g.capWG.Add(1)
	go func() {
		defer g.capWG.Done()
		if err := g.store.SaveCapture(&c.rec); err != nil {
			slog.Warn("capture: saving failed", "request", c.rec.RequestID, "err", err)
		}
	}()
}
