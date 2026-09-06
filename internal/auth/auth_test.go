package auth

import (
	"strings"
	"testing"

	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

// A syntactically valid but fabricated magic-link secret, used only to prove
// that credential-shaped strings are redacted. Not a real credential.
const fakeHash = "0123456789abcdef0123456789abcdef01234567"

func TestExtractHashFromLinkOrBareValue(t *testing.T) {
	cases := []string{
		"https://nomads.com/user/api?action=login_by_email&hash=" + fakeHash,
		"http://nomads.com/user/api?hash=" + fakeHash + "&other=1",
		fakeHash,
		"  " + fakeHash + "  ",
	}
	for _, in := range cases {
		got, err := ExtractHash(in)
		if err != nil {
			t.Errorf("%.40s...: %v", in, err)
			continue
		}
		if got != fakeHash {
			t.Errorf("%.40s...: got %q", in, got)
		}
	}
}

func TestExtractHashRejectsLinksWithoutAHash(t *testing.T) {
	for _, in := range []string{"", "https://nomads.com/login", "not a url at all /x"} {
		if _, err := ExtractHash(in); err == nil {
			t.Errorf("expected an error for %q", in)
		} else if !nomads.IsCode(err, nomads.ErrInvalidInput) {
			t.Errorf("%q: want INVALID_INPUT, got %s", in, nomads.CodeOf(err))
		}
	}
}

// Redaction is a security control: a leaked login hash is a permanent account
// compromise, so it must never survive into a log line or an error message.
func TestRedactRemovesCredentialShapedTokens(t *testing.T) {
	in := "login failed for hash=" + fakeHash + " retrying"
	got := Redact(in)
	if strings.Contains(got, fakeHash) {
		t.Fatalf("the hash survived redaction: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected a redaction marker: %q", got)
	}
	// Ordinary text must be preserved so diagnostics stay useful.
	if !strings.Contains(got, "login failed") || !strings.Contains(got, "retrying") {
		t.Errorf("redaction destroyed the message: %q", got)
	}
}

func TestRedactURLStripsSensitiveParams(t *testing.T) {
	raw := "https://nomads.com/api/trips?username=someone&key=" + fakeHash + "&limit=10"
	got := RedactURL(raw)
	if strings.Contains(got, fakeHash) {
		t.Fatalf("the key survived redaction: %q", got)
	}
	// The shape must remain legible for debugging.
	if !strings.Contains(got, "username=someone") || !strings.Contains(got, "limit=10") {
		t.Errorf("non-sensitive params were lost: %q", got)
	}
	if !strings.Contains(got, "key=") {
		t.Errorf("the parameter name should remain: %q", got)
	}
}

func TestRedactURLHandlesUserIDAndToken(t *testing.T) {
	for _, param := range []string{"user_id", "token", "password"} {
		raw := "https://nomads.com/x?" + param + "=" + fakeHash
		if got := RedactURL(raw); strings.Contains(got, fakeHash) {
			t.Errorf("%s was not redacted: %q", param, got)
		}
	}
}

// Redaction must not mangle a URL it cannot parse.
func TestRedactURLFallsBackOnGarbage(t *testing.T) {
	got := RedactURL("://nonsense " + fakeHash)
	if strings.Contains(got, fakeHash) {
		t.Errorf("hash survived in unparseable input: %q", got)
	}
}
