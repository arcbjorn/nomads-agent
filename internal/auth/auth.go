// Package auth turns a Nomads.com magic link into a persistent cookie session.
//
// The design goal is that nothing outside this package ever needs the login
// secret: auth exchanges it once for cookies, and the HTTP client works purely
// from the resulting cookie jar.
package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/arcbjorn/nomads-agent/internal/storage"
	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

// BaseURL is the Nomads.com origin.
const BaseURL = "https://nomads.com"

// UserAgent identifies this tool honestly to the server.
const UserAgent = "nomads-agent/0.1"

// hashPattern matches the 32-40 hex secret used by magic links and user ids.
var hashPattern = regexp.MustCompile(`\b[a-fA-F0-9]{32,64}\b`)

// sensitiveParams are query keys whose values are credentials.
var sensitiveParams = map[string]bool{
	"hash": true, "user_id": true, "token": true, "key": true, "password": true,
}

// Redact removes anything credential-shaped from a string bound for a log or an
// error message. It is deliberately aggressive: over-redacting a diagnostic is
// harmless, leaking a permanent login secret is not.
func Redact(s string) string {
	return hashPattern.ReplaceAllString(s, "[REDACTED]")
}

// RedactURL strips credential query values, keeping the shape of the URL so it
// stays useful for debugging.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return Redact(raw)
	}
	q := u.Query()
	for k := range q {
		if sensitiveParams[strings.ToLower(k)] {
			q.Set(k, "[REDACTED]")
		}
	}
	u.RawQuery = q.Encode()
	return Redact(u.String())
}

// ExtractHash pulls the login secret out of a full magic-link URL, or accepts a
// bare hash. The returned value is a credential.
func ExtractHash(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", nomads.Errorf(nomads.ErrInvalidInput, "empty login link")
	}
	// A bare hash.
	if hashPattern.MatchString(input) && !strings.Contains(input, "/") {
		return input, nil
	}
	u, err := url.Parse(input)
	if err != nil {
		return "", nomads.Errorf(nomads.ErrInvalidInput, "login link is not a valid URL")
	}
	if h := u.Query().Get("hash"); h != "" {
		return h, nil
	}
	return "", nomads.Errorf(nomads.ErrInvalidInput,
		"login link has no hash parameter; expected .../user/api?action=login_by_email&hash=...")
}

// Authenticator exchanges login secrets for sessions and restores saved ones.
type Authenticator struct {
	store   *storage.Store
	baseURL string
	timeout time.Duration
}

// New builds an Authenticator backed by store.
func New(store *storage.Store) *Authenticator {
	return &Authenticator{store: store, baseURL: BaseURL, timeout: 30 * time.Second}
}

// WithBaseURL overrides the origin, for tests against a local server.
func (a *Authenticator) WithBaseURL(u string) *Authenticator {
	a.baseURL = strings.TrimRight(u, "/")
	return a
}

// Login redeems a magic link and persists the resulting session.
//
// The hash is stored so `auth refresh` can renew the session later without the
// user digging the email out again; it is written with 0600 like the cookies.
func (a *Authenticator) Login(ctx context.Context, loginLink string) (*storage.Session, error) {
	hash, err := ExtractHash(loginLink)
	if err != nil {
		return nil, err
	}
	sess, err := a.redeem(ctx, hash)
	if err != nil {
		return nil, err
	}
	// Preserve any API key and username already configured.
	if prev, err := a.store.LoadSession(); err == nil {
		sess.APIKey = prev.APIKey
		if sess.Username == "" {
			sess.Username = prev.Username
		}
	}
	if err := a.store.SaveSession(sess); err != nil {
		return nil, fmt.Errorf("persist session: %w", err)
	}
	return sess, nil
}

// Refresh redeems the stored hash again to renew expiring cookies.
func (a *Authenticator) Refresh(ctx context.Context) (*storage.Session, error) {
	old, err := a.store.LoadSession()
	if err != nil {
		return nil, err
	}
	if old == nil || old.LoginHash == "" {
		return nil, nomads.Errorf(nomads.ErrAuthExpired,
			"no stored login link to refresh from; run 'nomads auth login' with your magic link")
	}
	sess, err := a.redeem(ctx, old.LoginHash)
	if err != nil {
		return nil, err
	}
	if sess.Username == "" {
		sess.Username = old.Username
	}
	sess.CreatedAt = old.CreatedAt
	if err := a.store.SaveSession(sess); err != nil {
		return nil, fmt.Errorf("persist session: %w", err)
	}
	return sess, nil
}

// redeem performs the magic-link GET and harvests the session cookies.
func (a *Authenticator) redeem(ctx context.Context, hash string) (*storage.Session, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{Jar: jar, Timeout: a.timeout}

	endpoint := fmt.Sprintf("%s/user/api?action=login_by_email&hash=%s", a.baseURL, url.QueryEscape(hash))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		// Redact: the failed URL contains the secret.
		return nil, nomads.Wrap(err, nomads.ErrNetwork, "login request failed: %s", Redact(err.Error()))
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return nil, nomads.Errorf(nomads.ErrNetwork, "Nomads.com returned %d during login", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, nomads.Errorf(nomads.ErrRateLimited, "rate limited during login")
	}

	base, _ := url.Parse(a.baseURL)
	stored := collectCookies(jar, base)
	if len(stored) == 0 {
		return nil, nomads.Errorf(nomads.ErrAuthExpired,
			"login link did not produce a session; it may be expired or already invalidated").
			WithDetail("status", resp.StatusCode)
	}
	return &storage.Session{Cookies: stored, LoginHash: hash, CreatedAt: time.Now().UTC()}, nil
}

// collectCookies snapshots the jar for persistence.
func collectCookies(jar http.CookieJar, base *url.URL) []storage.Cookie {
	var out []storage.Cookie
	for _, c := range jar.Cookies(base) {
		out = append(out, storage.Cookie{
			Name: c.Name, Value: c.Value, Domain: base.Hostname(), Path: "/",
		})
	}
	return out
}

// Jar rebuilds a cookie jar from a stored session.
func Jar(sess *storage.Session, baseURL string) (http.CookieJar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	if sess == nil || len(sess.Cookies) == 0 {
		return jar, nil
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	cookies := make([]*http.Cookie, 0, len(sess.Cookies))
	for _, c := range sess.Cookies {
		path := c.Path
		if path == "" {
			path = "/"
		}
		cookies = append(cookies, &http.Cookie{Name: c.Name, Value: c.Value, Path: path})
	}
	jar.SetCookies(u, cookies)
	return jar, nil
}

// SaveAPIKey stores the personal API key from https://nomads.com/settings,
// keeping any existing browser session intact.
func (a *Authenticator) SaveAPIKey(username, key string) (*storage.Session, error) {
	sess, err := a.store.LoadSession()
	if err != nil {
		return nil, err
	}
	if sess == nil {
		sess = &storage.Session{}
	}
	if u := strings.TrimPrefix(strings.TrimSpace(username), "@"); u != "" {
		sess.Username = u
	}
	if sess.Username == "" {
		return nil, nomads.Errorf(nomads.ErrInvalidInput,
			"a username is required alongside the API key")
	}
	sess.APIKey = strings.TrimSpace(key)
	if err := a.store.SaveSession(sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// Logout deletes the locally stored session.
func (a *Authenticator) Logout() error { return a.store.Clear() }

// Load returns the stored session, erroring semantically when absent.
func (a *Authenticator) Load() (*storage.Session, error) {
	sess, err := a.store.LoadSession()
	if err != nil {
		return nil, err
	}
	if sess.IsEmpty() {
		return nil, nomads.Errorf(nomads.ErrAuthExpired,
			"not logged in; run 'nomads auth login --link <magic link>'")
	}
	return sess, nil
}
