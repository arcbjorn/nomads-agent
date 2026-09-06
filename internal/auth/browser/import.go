// Package browser bootstraps a session from a browser you are already logged
// into.
//
// This is the fallback the main auth path needs when a magic link cannot be
// redeemed programmatically. Nomads.com serves "Link is corrupted!" to
// non-browser HTTP clients even when the link is still valid, so the practical
// way to obtain a session is to lift the cookies from a browser that already
// holds one.
//
// Nothing here runs a browser or drives a page: it only accepts cookies the
// user exports, so Playwright is not a runtime dependency.
package browser

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/arcbjorn/nomads-agent/internal/storage"
	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

// SessionCookies are the cookies that actually authenticate a Nomads.com
// session. Others (analytics, UI preferences) are ignored on import.
var SessionCookies = map[string]bool{
	"PHPSESSID":      true,
	"logged_in_hash": true,
}

// RequiredCookie is the one cookie a session is useless without.
const RequiredCookie = "logged_in_hash"

// ParseCookieHeader reads a `document.cookie` string, the form a browser
// console produces:
//
//	PHPSESSID=abc; logged_in_hash=def; ref=xyz
func ParseCookieHeader(raw string) ([]storage.Cookie, error) {
	var out []storage.Cookie
	for _, part := range strings.Split(raw, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if name == "" || value == "" || !SessionCookies[name] {
			continue
		}
		out = append(out, storage.Cookie{Name: name, Value: value, Path: "/"})
	}
	return validate(out)
}

// devtoolsCookie is one entry of a DevTools / extension cookie export.
type devtoolsCookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
}

// ParseCookieJSON reads a JSON cookie export, as produced by DevTools or by a
// cookie-export extension. Accepts either an array or {"cookies": [...]}.
func ParseCookieJSON(raw []byte) ([]storage.Cookie, error) {
	var list []devtoolsCookie
	if err := json.Unmarshal(raw, &list); err != nil {
		var wrapped struct {
			Cookies []devtoolsCookie `json:"cookies"`
		}
		if err2 := json.Unmarshal(raw, &wrapped); err2 != nil {
			return nil, nomads.Errorf(nomads.ErrInvalidInput,
				"could not parse cookies: expected a JSON array or {\"cookies\": [...]}")
		}
		list = wrapped.Cookies
	}

	var out []storage.Cookie
	for _, c := range list {
		if !SessionCookies[c.Name] || c.Value == "" {
			continue
		}
		// Ignore cookies belonging to some other site in the same export.
		if c.Domain != "" && !strings.Contains(c.Domain, "nomads.com") {
			continue
		}
		path := c.Path
		if path == "" {
			path = "/"
		}
		out = append(out, storage.Cookie{Name: c.Name, Value: c.Value, Path: path})
	}
	return validate(out)
}

// validate checks that an import actually carries a usable session.
func validate(cookies []storage.Cookie) ([]storage.Cookie, error) {
	if len(cookies) == 0 {
		return nil, nomads.Errorf(nomads.ErrInvalidInput,
			"no Nomads.com session cookies found; expected at least %s", RequiredCookie)
	}
	for _, c := range cookies {
		if c.Name == RequiredCookie {
			return cookies, nil
		}
	}
	names := make([]string, 0, len(cookies))
	for _, c := range cookies {
		names = append(names, c.Name)
	}
	return nil, nomads.Errorf(nomads.ErrInvalidInput,
		"%s is missing (found: %s); make sure you are logged in to Nomads.com in that browser",
		RequiredCookie, strings.Join(names, ", "))
}

// Instructions tells the user how to export their session, without asking them
// to paste anything into a third-party tool.
func Instructions() string {
	return fmt.Sprintf(`To import your session from a browser you are already logged into:

  1. Open https://nomads.com in that browser and make sure you are logged in.
  2. Open DevTools (F12 or Cmd+Option+I) and pick the Console tab.
  3. Run:

       copy(document.cookie)

     That copies the cookie string to your clipboard without displaying it.
  4. Paste it into:

       nomads auth import --cookies "<paste>"

     or pipe it in to keep it out of your shell history:

       pbpaste | nomads auth import --stdin

Only %s and %s are stored; everything else in the string is discarded.

These cookies are the equivalent of a password for your account. They are
written to the config file with 0600 permissions and are never logged.`,
		"PHPSESSID", RequiredCookie)
}
