package browser

import (
	"strings"
	"testing"

	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

func TestParseCookieHeaderKeepsOnlySessionCookies(t *testing.T) {
	raw := "PHPSESSID=abc123; logged_in_hash=deadbeefcafe; ref=marketing; " +
		"last_tested_internet_speed=42; hide_customer_feedback_modal=1"

	got, err := ParseCookieHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want only the 2 session cookies, got %d: %+v", len(got), got)
	}
	byName := map[string]string{}
	for _, c := range got {
		byName[c.Name] = c.Value
		if c.Path != "/" {
			t.Errorf("%s: path should default to /", c.Name)
		}
	}
	if byName["PHPSESSID"] != "abc123" || byName["logged_in_hash"] != "deadbeefcafe" {
		t.Errorf("values not parsed: %+v", byName)
	}
	// Tracking and UI cookies must never be persisted.
	if _, ok := byName["ref"]; ok {
		t.Error("non-session cookie was imported")
	}
}

// Without logged_in_hash the session is not authenticated, so importing it
// would store a credential file that silently does nothing.
func TestParseCookieHeaderRequiresTheAuthCookie(t *testing.T) {
	_, err := ParseCookieHeader("PHPSESSID=abc123; ref=marketing")
	if err == nil {
		t.Fatal("expected an error when logged_in_hash is absent")
	}
	if !nomads.IsCode(err, nomads.ErrInvalidInput) {
		t.Fatalf("want INVALID_INPUT, got %s", nomads.CodeOf(err))
	}
	if !strings.Contains(err.Error(), RequiredCookie) {
		t.Errorf("error should name the missing cookie: %v", err)
	}
}

func TestParseCookieHeaderRejectsEmptyInput(t *testing.T) {
	for _, in := range []string{"", "   ", "nonsense", "a=; b="} {
		if _, err := ParseCookieHeader(in); err == nil {
			t.Errorf("expected an error for %q", in)
		}
	}
}

func TestParseCookieJSONArrayAndWrapped(t *testing.T) {
	array := `[{"name":"PHPSESSID","value":"abc","domain":".nomads.com","path":"/"},
	           {"name":"logged_in_hash","value":"def","domain":".nomads.com","path":"/"}]`
	got, err := ParseCookieJSON([]byte(array))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 cookies, got %d", len(got))
	}

	wrapped := `{"cookies":[{"name":"logged_in_hash","value":"def","domain":"nomads.com"}]}`
	got, err = ParseCookieJSON([]byte(wrapped))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Value != "def" {
		t.Fatalf("wrapped form not parsed: %+v", got)
	}
}

// A full browser export contains other sites; those must not be imported.
func TestParseCookieJSONIgnoresOtherDomains(t *testing.T) {
	raw := `[{"name":"logged_in_hash","value":"mine","domain":".nomads.com"},
	         {"name":"logged_in_hash","value":"theirs","domain":".example.com"}]`
	got, err := ParseCookieJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Value != "mine" {
		t.Fatalf("cookies from another domain leaked in: %+v", got)
	}
}

func TestParseCookieJSONRejectsGarbage(t *testing.T) {
	if _, err := ParseCookieJSON([]byte(`not json`)); err == nil {
		t.Error("expected a parse error")
	}
}

// The instructions must not tell the user to print their cookies to a screen
// or paste them into a third-party site.
func TestInstructionsUseClipboardNotDisplay(t *testing.T) {
	got := Instructions()
	if !strings.Contains(got, "copy(document.cookie)") {
		t.Error("instructions should use copy() so the value is never displayed")
	}
	if !strings.Contains(got, "0600") {
		t.Error("instructions should state how the credential is stored")
	}
}
