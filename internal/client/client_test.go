package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

// stub is a fake Nomads.com used to exercise protocol handling.
type stub struct {
	mu       sync.Mutex
	profile  string
	requests []recorded
	handler  func(w http.ResponseWriter, r *http.Request) bool
}

type recorded struct {
	Method string
	Path   string
	Form   map[string][]string
}

func newStub(t *testing.T) (*stub, *httptest.Server) {
	t.Helper()
	s := &stub{profile: mustRead(t, "../../fixtures/profile.html")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		s.requests = append(s.requests, recorded{r.Method, r.URL.Path, r.PostForm})
		s.mu.Unlock()

		if s.handler != nil && s.handler(w, r) {
			return
		}
		if strings.HasPrefix(r.URL.Path, "/@") {
			w.Header().Set("Content-Type", "text/html")
			s.mu.Lock()
			body := s.profile
			s.mu.Unlock()
			_, _ = w.Write([]byte(body))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	t.Cleanup(srv.Close)
	return s, srv
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (s *stub) calls() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recorded(nil), s.requests...)
}

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(nil, WithBaseURL(srv.URL), WithGeocoder(&StaticGeocoder{
		Places: map[string]Place{
			"lisbon|portugal": {City: "Lisbon", Country: "Portugal", Latitude: 38.7223, Longitude: -9.1393},
			"porto|portugal":  {City: "Porto", Country: "Portugal", Latitude: 41.1495, Longitude: -8.6108},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	c.SetUsername("testuser")
	return c
}

func TestGetProfileAndListTrips(t *testing.T) {
	_, srv := newStub(t)
	c := newTestClient(t, srv)

	p, err := c.GetProfile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.Bio == "" || p.Username != "testuser" {
		t.Fatalf("unexpected profile: %+v", p)
	}

	trips, err := c.ListTrips(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(trips) != 3 {
		t.Fatalf("want 3 trips, got %d", len(trips))
	}
}

// UpdateProfile must send one request per changed field, and none for the rest.
func TestUpdateProfileSendsOnlyChangedFields(t *testing.T) {
	s, srv := newStub(t)
	c := newTestClient(t, srv)

	bio := "Remote developer, currently in Europe."
	if _, err := c.UpdateProfile(context.Background(), nomads.ProfilePatch{Bio: &bio}); err != nil {
		t.Fatal(err)
	}

	var actions []string
	for _, r := range s.calls() {
		if a := r.Form["action"]; len(a) > 0 {
			actions = append(actions, a[0])
		}
	}
	if len(actions) != 1 || actions[0] != "change_bio" {
		t.Fatalf("expected exactly one change_bio call, got %v", actions)
	}
}

func TestUpdateProfileUsesObservedActionNames(t *testing.T) {
	s, srv := newStub(t)
	c := newTestClient(t, srv)

	v := "exampleuser"
	web := "https://example.com/"
	_, err := c.UpdateProfile(context.Background(), nomads.ProfilePatch{
		Twitter: &v, Instagram: &v, Website: &web,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Each social field has its own action and its own parameter name.
	want := map[string]string{
		"set_twitter":   "twitter_username",
		"set_instagram": "instagram_username",
		"set_website":   "website_url",
	}
	seen := map[string]bool{}
	for _, r := range s.calls() {
		a := r.Form["action"]
		if len(a) == 0 {
			continue
		}
		param, ok := want[a[0]]
		if !ok {
			continue
		}
		if _, present := r.Form[param]; !present {
			t.Errorf("%s: missing parameter %s (sent %v)", a[0], param, r.Form)
		}
		if r.Method != http.MethodPost {
			t.Errorf("%s: want POST, got %s", a[0], r.Method)
		}
		// Profile writes go to /user/api with NO trailing slash.
		if r.Path != "/user/api" {
			t.Errorf("%s: want path /user/api, got %s", a[0], r.Path)
		}
		seen[a[0]] = true
	}
	for a := range want {
		if !seen[a] {
			t.Errorf("action %s was never sent", a)
		}
	}
}

// A blank bio is rejected by the real API; that must surface semantically.
func TestUpdateProfileMapsValidationFailure(t *testing.T) {
	s, srv := newStub(t)
	s.handler = func(w http.ResponseWriter, r *http.Request) bool {
		if r.PostForm.Get("action") == "change_bio" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":false,"message":"Can't be blank"}`))
			return true
		}
		return false
	}
	c := newTestClient(t, srv)

	blank := ""
	_, err := c.UpdateProfile(context.Background(), nomads.ProfilePatch{Bio: &blank})
	if !nomads.IsCode(err, nomads.ErrProfileValidationFailed) {
		t.Fatalf("want PROFILE_VALIDATION_FAILED, got %s (%v)", nomads.CodeOf(err), err)
	}
}

func TestCreateTripSendsGeocodedCoordinates(t *testing.T) {
	s, srv := newStub(t)
	s.handler = func(w http.ResponseWriter, r *http.Request) bool {
		if r.PostForm.Get("action") == "trip" && r.Method == http.MethodPut {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"trip_id":"newid123","city":"Lisbon",
				"country":"Portugal","city_slug":"lisbon-portugal","trip_length":"8d"}`))
			return true
		}
		return false
	}
	c := newTestClient(t, srv)

	trip, err := c.CreateTrip(context.Background(), nomads.CreateTripRequest{
		City: "Lisbon", Country: "Portugal",
		StartDate: nomads.MustParseDate("2030-04-10"),
		EndDate:   nomads.MustParseDate("2030-04-18"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if trip.ID != "newid123" {
		t.Fatalf("trip id not adopted from reply: %q", trip.ID)
	}

	var form map[string][]string
	for _, r := range s.calls() {
		if r.Form["action"] != nil && r.Form["action"][0] == "trip" {
			form = r.Form
		}
	}
	// Coordinates are mandatory: the server rejects "No geo data sent".
	if form["latitude"][0] == "" || form["longitude"][0] == "" {
		t.Fatal("coordinates were not sent")
	}
	// An empty trip_id is what signals "create" to the API.
	if form["trip_id"][0] != "" {
		t.Errorf("create must send an empty trip_id, got %q", form["trip_id"][0])
	}
	if form["date_start"][0] != "2030-04-10" || form["date_end"][0] != "2030-04-18" {
		t.Errorf("dates not sent as ISO: %v %v", form["date_start"], form["date_end"])
	}
}

// The observed API reassigns the trip id on edit; the client must adopt it.
func TestUpdateTripAdoptsReassignedID(t *testing.T) {
	s, srv := newStub(t)
	s.handler = func(w http.ResponseWriter, r *http.Request) bool {
		if r.PostForm.Get("action") == "trip" && r.Method == http.MethodPut {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"trip_id":"REASSIGNED","city":"Lisbon",
				"country":"Portugal","trip_length":"9d"}`))
			return true
		}
		return false
	}
	c := newTestClient(t, srv)

	end := nomads.MustParseDate("2030-04-25")
	got, err := c.UpdateTrip(context.Background(),
		"aaaa0000000000000000000000000000000000000000000001",
		nomads.TripPatch{EndDate: &end})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "REASSIGNED" {
		t.Fatalf("client kept the stale id %q; the API reassigns it on edit", got.ID)
	}
	// Unpatched fields must be carried over from the existing trip.
	var form map[string][]string
	for _, r := range s.calls() {
		if r.Form["action"] != nil && r.Form["action"][0] == "trip" {
			form = r.Form
		}
	}
	if form["date_start"][0] != "2030-04-10" {
		t.Errorf("start date not preserved from current trip: %v", form["date_start"])
	}
	if form["city"][0] != "Lisbon" {
		t.Errorf("city not preserved: %v", form["city"])
	}
}

// DELETE returns success even for an unknown id, so deletion must be verified.
func TestDeleteTripVerifiesRemoval(t *testing.T) {
	s, srv := newStub(t)
	// The profile keeps listing the trip: the delete silently did nothing.
	c := newTestClient(t, srv)

	err := c.DeleteTrip(context.Background(), "aaaa0000000000000000000000000000000000000000000001")
	if !nomads.IsCode(err, nomads.ErrRemoteStateMismatch) {
		t.Fatalf("want REMOTE_STATE_MISMATCH when the trip survives, got %s (%v)",
			nomads.CodeOf(err), err)
	}
	_ = s
}

func TestDeleteTripSucceedsWhenGone(t *testing.T) {
	s, srv := newStub(t)
	s.handler = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodDelete {
			// After the delete, serve a profile without that trip.
			s.mu.Lock()
			s.profile = strings.Replace(s.profile,
				`data-trip-id="aaaa0000000000000000000000000000000000000000000001"`,
				`data-trip-id=""`, 1)
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true}`))
			return true
		}
		return false
	}
	c := newTestClient(t, srv)

	if err := c.DeleteTrip(context.Background(),
		"aaaa0000000000000000000000000000000000000000000001"); err != nil {
		t.Fatalf("delete should succeed once the trip is gone: %v", err)
	}
}

// HTML where JSON was expected is the signature of an expired session.
func TestLoginPageResponseBecomesAuthExpired(t *testing.T) {
	s, srv := newStub(t)
	s.handler = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body>Please log in to Nomads.com
			<a href="/user/api?action=login_by_email">sign in</a></body></html>`))
		return true
	}
	c := newTestClient(t, srv)

	_, err := c.ListTrips(context.Background())
	if !nomads.IsCode(err, nomads.ErrAuthExpired) {
		t.Fatalf("want AUTH_EXPIRED, got %s (%v)", nomads.CodeOf(err), err)
	}
}

func TestUnexpectedJSONShapeBecomesAPIChanged(t *testing.T) {
	s, srv := newStub(t)
	s.handler = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut {
			w.Header().Set("Content-Type", "application/json")
			// success:true but no trip_id: the contract moved.
			_, _ = w.Write([]byte(`{"success":true}`))
			return true
		}
		return false
	}
	c := newTestClient(t, srv)

	_, err := c.CreateTrip(context.Background(), nomads.CreateTripRequest{
		City: "Lisbon", Country: "Portugal",
		StartDate: nomads.MustParseDate("2030-04-10"),
		EndDate:   nomads.MustParseDate("2030-04-18"),
	})
	if !nomads.IsCode(err, nomads.ErrAPIChanged) {
		t.Fatalf("want NOMADS_API_CHANGED, got %s (%v)", nomads.CodeOf(err), err)
	}
}

func TestRateLimitIsReportedWithRetryAfter(t *testing.T) {
	s, srv := newStub(t)
	s.handler = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut {
			w.Header().Set("Retry-After", "42")
			w.WriteHeader(http.StatusTooManyRequests)
			return true
		}
		return false
	}
	c := newTestClient(t, srv)

	_, err := c.CreateTrip(context.Background(), nomads.CreateTripRequest{
		City: "Lisbon", Country: "Portugal",
		StartDate: nomads.MustParseDate("2030-04-10"),
		EndDate:   nomads.MustParseDate("2030-04-18"),
	})
	if !nomads.IsCode(err, nomads.ErrRateLimited) {
		t.Fatalf("want RATE_LIMITED, got %s", nomads.CodeOf(err))
	}
	var ne *nomads.Error
	if asNomadsError(err, &ne) && ne.RetryAfter.Seconds() != 42 {
		t.Errorf("Retry-After not honoured: %v", ne.RetryAfter)
	}
}

// A failed mutation must never be retried automatically.
func TestMutationsAreNotRetried(t *testing.T) {
	s, srv := newStub(t)
	s.handler = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	}
	c := newTestClient(t, srv)

	_, _ = c.CreateTrip(context.Background(), nomads.CreateTripRequest{
		City: "Lisbon", Country: "Portugal",
		StartDate: nomads.MustParseDate("2030-04-10"),
		EndDate:   nomads.MustParseDate("2030-04-18"),
	})

	puts := 0
	for _, r := range s.calls() {
		if r.Method == http.MethodPut {
			puts++
		}
	}
	if puts != 1 {
		t.Fatalf("a create must be attempted exactly once, got %d attempts", puts)
	}
}

func TestCreateTripRejectsInvalidDates(t *testing.T) {
	_, srv := newStub(t)
	c := newTestClient(t, srv)

	_, err := c.CreateTrip(context.Background(), nomads.CreateTripRequest{
		City:      "Lisbon",
		StartDate: nomads.MustParseDate("2030-04-18"),
		EndDate:   nomads.MustParseDate("2030-04-10"),
	})
	if !nomads.IsCode(err, nomads.ErrInvalidInput) {
		t.Fatalf("want INVALID_INPUT for reversed dates, got %s", nomads.CodeOf(err))
	}
}

func TestValueMatchesToleratesServerNormalisation(t *testing.T) {
	cases := []struct {
		want, got string
		match     bool
	}{
		{"@handle", "handle", true},
		{"handle", "@handle", true},
		{"https://example.com/", "example.com", true},
		{"Example", "example", true},
		{"one", "two", false},
	}
	for _, c := range cases {
		if got := valueMatches(c.want, c.got); got != c.match {
			t.Errorf("valueMatches(%q,%q)=%v want %v", c.want, c.got, got, c.match)
		}
	}
}
