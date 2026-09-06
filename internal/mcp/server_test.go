package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/arcbjorn/nomads-agent/internal/client"
	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

// roundTrip feeds requests through the server and returns the responses.
func roundTrip(t *testing.T, s *Server, reqs ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	in := strings.NewReader(strings.Join(reqs, "\n") + "\n")
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var replies []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad reply %q: %v", line, err)
		}
		replies = append(replies, m)
	}
	return replies
}

// unconfigured returns a server whose client cannot be built.
func unconfigured() *Server {
	return NewServer(func() (*client.Hybrid, error) {
		return nil, nomads.Errorf(nomads.ErrAuthExpired, "no credentials configured")
	})
}

func TestInitializeAndToolsList(t *testing.T) {
	replies := roundTrip(t, unconfigured(),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)

	if len(replies) != 2 {
		t.Fatalf("want 2 replies, got %d", len(replies))
	}
	init := replies[0]["result"].(map[string]any)
	if init["protocolVersion"] != ProtocolVersion {
		t.Errorf("protocol version: %v", init["protocolVersion"])
	}

	list := replies[1]["result"].(map[string]any)["tools"].([]any)
	got := map[string]bool{}
	for _, x := range list {
		got[x.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{
		"nomads_get_profile", "nomads_update_profile", "nomads_list_trips",
		"nomads_add_trip", "nomads_update_trip", "nomads_delete_trip",
		"nomads_plan_trip_sync", "nomads_sync_trips",
	} {
		if !got[want] {
			t.Errorf("missing tool %s", want)
		}
	}
}

// Every tool must declare a valid JSON Schema, or clients reject the server.
func TestToolSchemasAreWellFormed(t *testing.T) {
	for _, tl := range tools() {
		if tl.Name == "" || tl.Description == "" {
			t.Errorf("tool %q missing name or description", tl.Name)
		}
		if tl.InputSchema.Type != "object" {
			t.Errorf("%s: input schema must be an object", tl.Name)
		}
		// Every required field must actually be declared.
		for _, req := range tl.InputSchema.Required {
			if _, ok := tl.InputSchema.Properties[req]; !ok {
				t.Errorf("%s: required field %q is not in properties", tl.Name, req)
			}
		}
		if _, err := json.Marshal(tl); err != nil {
			t.Errorf("%s: schema does not marshal: %v", tl.Name, err)
		}
	}
}

// A notification must not produce a reply.
func TestNotificationsAreNotAnswered(t *testing.T) {
	replies := roundTrip(t, unconfigured(),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":9,"method":"ping","params":{}}`)
	if len(replies) != 1 {
		t.Fatalf("a notification must not be answered; got %d replies", len(replies))
	}
	if replies[0]["id"].(float64) != 9 {
		t.Errorf("wrong reply: %v", replies[0])
	}
}

func TestUnknownMethodReturnsError(t *testing.T) {
	replies := roundTrip(t, unconfigured(),
		`{"jsonrpc":"2.0","id":1,"method":"does/not/exist","params":{}}`)
	e, ok := replies[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error, got %v", replies[0])
	}
	if int(e["code"].(float64)) != codeMethodNotFound {
		t.Errorf("wrong code: %v", e["code"])
	}
}

func TestMalformedJSONIsReported(t *testing.T) {
	replies := roundTrip(t, unconfigured(), `{not json`)
	if _, ok := replies[0]["error"]; !ok {
		t.Fatalf("expected a parse error, got %v", replies[0])
	}
}

// A missing credential must come back as a readable tool error carrying the
// semantic code, not as a transport-level failure.
func TestToolErrorsCarrySemanticCode(t *testing.T) {
	replies := roundTrip(t, unconfigured(),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nomads_list_trips","arguments":{}}}`)

	res := replies[0]["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("expected isError, got %v", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("tool error should be JSON: %v", err)
	}
	if payload["error"] != string(nomads.ErrAuthExpired) {
		t.Errorf("want AUTH_EXPIRED, got %v", payload["error"])
	}
}

func TestToDesiredValidatesDates(t *testing.T) {
	if _, err := toDesired([]tripArg{{City: "Rome", From: "not-a-date", To: "2026-01-05"}}); err == nil {
		t.Error("expected an error for a malformed start date")
	}
	if _, err := toDesired([]tripArg{{City: "Rome", From: "2026-01-10", To: "2026-01-05"}}); err == nil {
		t.Error("expected an error when the end precedes the start")
	}
	got, err := toDesired([]tripArg{
		{City: "Lisbon", Country: "Portugal", From: "2030-04-10", To: "2030-04-18"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].StartDate.String() != "2030-04-10" || got[0].City != "Lisbon" {
		t.Errorf("got %+v", got[0])
	}
}

func TestSyncOptionsParseDeleteHorizon(t *testing.T) {
	opts, err := syncOptions(syncArgs{DeleteMissing: true, DeleteAfter: "2026-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.DeleteMissing || opts.DeleteHorizon == nil {
		t.Fatalf("options not applied: %+v", opts)
	}
	if opts.DeleteHorizon.String() != "2026-01-01" {
		t.Errorf("horizon: %s", opts.DeleteHorizon)
	}
	if _, err := syncOptions(syncArgs{DeleteAfter: "nope"}); err == nil {
		t.Error("expected an error for a malformed delete_after")
	}
}

// The plan tool must be documented as non-mutating, and the sync tool must
// default to not deleting.
func TestSyncToolDefaultsAreSafe(t *testing.T) {
	var a syncArgs
	if err := json.Unmarshal([]byte(`{"trips":[]}`), &a); err != nil {
		t.Fatal(err)
	}
	if a.DeleteMissing {
		t.Fatal("delete_missing must default to false")
	}
	for _, tl := range tools() {
		if tl.Name == "nomads_plan_trip_sync" && !strings.Contains(tl.Description, "NEVER") {
			t.Error("the plan tool must state that it never mutates")
		}
	}
}
