// Package client speaks the undocumented Nomads.com HTTP protocol.
//
// This is the only package that knows endpoint paths, form field names and
// response quirks. Everything above it works with the normalised types in
// pkg/nomads, so a change to the remote API is contained here.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/arcbjorn/nomads-agent/internal/auth"
	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

const (
	defaultTimeout = 30 * time.Second
	// maxGetRetries bounds retries for reads. Mutations are never retried.
	maxGetRetries = 3
	// maxBodySnippet caps how much of an unexpected body we keep for diagnostics.
	maxBodySnippet = 240
)

// apiReply is the envelope every /user/api response shares.
type apiReply struct {
	Success any             `json:"success"`
	Message string          `json:"message"`
	Error   string          `json:"error"`
	Raw     json.RawMessage `json:"-"`
}

// ok normalises the success flag, which the API sends as a bool or as a string.
func (r apiReply) ok() bool {
	switch v := r.Success.(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	case float64:
		return v == 1
	}
	return false
}

// reason returns the server's explanation, if any.
func (r apiReply) reason() string {
	if r.Message != "" {
		return r.Message
	}
	return r.Error
}

// doForm posts a form to the API and decodes the JSON envelope.
//
// method carries meaning in this API: PUT creates and updates, DELETE deletes,
// POST writes profile fields.
func (c *Client) doForm(ctx context.Context, method, path string, form url.Values, op string) (json.RawMessage, error) {
	endpoint := c.baseURL + path
	body := form.Encode()

	req, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(body))
	if err != nil {
		return nil, nomads.Wrap(err, nomads.ErrInvalidInput, "build request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("User-Agent", auth.UserAgent)
	req.Header.Set("Referer", c.baseURL+"/")

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logOp(op, method, path, 0, time.Since(start), 0, "network_error")
		return nil, nomads.Wrap(err, nomads.ErrNetwork, "%s: %s", op, auth.Redact(err.Error()))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nomads.Wrap(err, nomads.ErrNetwork, "%s: reading response", op)
	}
	c.logOp(op, method, path, resp.StatusCode, time.Since(start), 0, "ok")

	if err := c.checkHTTP(resp, raw, op); err != nil {
		return nil, err
	}

	var reply apiReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, apiChanged(op, "response was not JSON", resp, raw)
	}
	if !reply.ok() {
		return nil, c.classifyFailure(op, reply, raw)
	}
	return raw, nil
}

// checkHTTP maps transport-level outcomes onto semantic errors.
func (c *Client) checkHTTP(resp *http.Response, raw []byte, op string) error {
	// A redirect to the login page means the cookies are no longer good.
	if loc := resp.Header.Get("Location"); strings.Contains(loc, "/login") {
		return nomads.Errorf(nomads.ErrAuthExpired, "%s: redirected to login", op)
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		e := nomads.Errorf(nomads.ErrRateLimited, "%s: rate limited by Nomads.com", op)
		if d := retryAfter(resp); d > 0 {
			e.RetryAfter = d
			e.WithDetail("retry_after_seconds", int(d.Seconds()))
		}
		return e
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nomads.Errorf(nomads.ErrAuthExpired,
			"%s: session rejected (HTTP %d); run 'nomads auth refresh'", op, resp.StatusCode)
	case resp.StatusCode >= 500:
		return nomads.Errorf(nomads.ErrNetwork, "%s: Nomads.com returned HTTP %d", op, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return apiChanged(op, fmt.Sprintf("unexpected HTTP %d", resp.StatusCode), resp, raw)
	}
	// HTML where JSON belongs is the classic signal of a logged-out session or
	// a reshaped frontend.
	if looksLikeHTML(raw) {
		if looksLikeLoginPage(raw) {
			return nomads.Errorf(nomads.ErrAuthExpired,
				"%s: received the login page instead of data; run 'nomads auth refresh'", op)
		}
		return apiChanged(op, "received HTML instead of JSON", resp, raw)
	}
	return nil
}

// classifyFailure turns a {"success":false} envelope into a semantic error.
func (c *Client) classifyFailure(op string, reply apiReply, raw []byte) error {
	reason := reply.reason()
	lower := strings.ToLower(reason)

	switch {
	case strings.Contains(lower, "re-auth"), strings.Contains(lower, "log in"),
		strings.Contains(lower, "logged in"), strings.Contains(lower, "not logged"):
		return nomads.Errorf(nomads.ErrAuthExpired, "%s: %s", op, reason)
	case strings.Contains(lower, "blank"), strings.Contains(lower, "invalid"),
		strings.Contains(lower, "too long"), strings.Contains(lower, "too short"),
		strings.Contains(lower, "already taken"), strings.Contains(lower, "not allowed"):
		return nomads.Errorf(nomads.ErrProfileValidationFailed, "%s: %s", op, reason).
			WithDetail("server_message", reason)
	case strings.Contains(lower, "no geo data"):
		return nomads.Errorf(nomads.ErrGeocodeFailed,
			"%s: Nomads.com requires coordinates for this trip (%s)", op, reason)
	}
	if reason == "" {
		return apiChanged(op, "server reported failure with no message", nil, raw)
	}
	return nomads.Errorf(nomads.ErrAPIChanged, "%s: %s", op, reason).
		WithDetail("server_message", reason)
}

// apiChanged builds a NOMADS_API_CHANGED error with non-secret diagnostics.
func apiChanged(op, why string, resp *http.Response, raw []byte) error {
	e := nomads.Errorf(nomads.ErrAPIChanged,
		"%s: %s — the Nomads.com frontend protocol may have changed; "+
			"see docs/api-observations.md to recapture it", op, why)
	if resp != nil {
		e.WithDetail("status", resp.StatusCode)
		e.WithDetail("content_type", resp.Header.Get("Content-Type"))
	}
	e.WithDetail("body_snippet", auth.Redact(snippet(raw)))
	return e
}

// snippet trims a body down to a loggable excerpt.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxBodySnippet {
		return s[:maxBodySnippet] + "..."
	}
	return s
}

func looksLikeHTML(b []byte) bool {
	s := strings.TrimSpace(strings.ToLower(string(b)))
	return strings.HasPrefix(s, "<!doctype") || strings.HasPrefix(s, "<html")
}

func looksLikeLoginPage(b []byte) bool {
	s := strings.ToLower(string(b))
	return strings.Contains(s, "login_by_email") ||
		strings.Contains(s, "sign in") || strings.Contains(s, "log in to")
}

// retryAfter reads the Retry-After header in either supported form.
func retryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// getHTML fetches a page, retrying safely since GETs have no side effects.
func (c *Client) getHTML(ctx context.Context, path, op string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < maxGetRetries; attempt++ {
		if attempt > 0 {
			delay := backoff(attempt, lastErr)
			c.log.Debug("retrying read", "operation", op, "attempt", attempt+1,
				"delay", delay.String())
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		body, err := c.getHTMLOnce(ctx, path, op, attempt)
		if err == nil {
			return body, nil
		}
		lastErr = err
		// Only transient classes are worth another attempt.
		switch nomads.CodeOf(err) {
		case nomads.ErrNetwork, nomads.ErrRateLimited:
			continue
		default:
			return nil, err
		}
	}
	return nil, lastErr
}

// backoff grows the delay per attempt, honouring Retry-After when given.
func backoff(attempt int, lastErr error) time.Duration {
	var e *nomads.Error
	if lastErr != nil && strings.Contains(lastErr.Error(), string(nomads.ErrRateLimited)) {
		if ok := asNomadsError(lastErr, &e); ok && e.RetryAfter > 0 {
			return e.RetryAfter
		}
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

func (c *Client) getHTMLOnce(ctx context.Context, path, op string, attempt int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, nomads.Wrap(err, nomads.ErrInvalidInput, "build request")
	}
	req.Header.Set("User-Agent", auth.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logOp(op, http.MethodGet, path, 0, time.Since(start), attempt, "network_error")
		return nil, nomads.Wrap(err, nomads.ErrNetwork, "%s: %s", op, auth.Redact(err.Error()))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, nomads.Wrap(err, nomads.ErrNetwork, "%s: reading response", op)
	}
	c.logOp(op, http.MethodGet, path, resp.StatusCode, time.Since(start), attempt, "ok")

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		e := nomads.Errorf(nomads.ErrRateLimited, "%s: rate limited by Nomads.com", op)
		if d := retryAfter(resp); d > 0 {
			e.RetryAfter = d
		}
		return nil, e
	case resp.StatusCode >= 500:
		return nil, nomads.Errorf(nomads.ErrNetwork, "%s: Nomads.com returned HTTP %d", op, resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return nil, nomads.Errorf(nomads.ErrAPIChanged, "%s: page not found (HTTP 404)", op).
			WithDetail("path", path)
	case resp.StatusCode != http.StatusOK:
		return nil, apiChanged(op, fmt.Sprintf("unexpected HTTP %d", resp.StatusCode), resp, body)
	}
	return body, nil
}

// logOp emits one structured line per request. Bodies are never logged here.
func (c *Client) logOp(op, method, path string, status int, dur time.Duration, retries int, result string) {
	c.log.Debug("nomads request",
		"operation", op,
		"method", method,
		"endpoint", auth.RedactURL(path),
		"status", status,
		"duration_ms", dur.Milliseconds(),
		"retry_count", retries,
		"result", result,
	)
}

// slogLevel picks the level for the client logger.
func slogLevel(debug bool) slog.Level {
	if debug {
		return slog.LevelDebug
	}
	return slog.LevelWarn
}
