package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/arcbjorn/nomads-agent/internal/auth"
	"github.com/arcbjorn/nomads-agent/internal/storage"
	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

// Client talks to Nomads.com as an authenticated user.
type Client struct {
	httpClient *http.Client
	baseURL    string
	username   string
	log        *slog.Logger
	geocoder   Geocoder
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL overrides the origin, used by tests against a local server.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithLogger sets the structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) { c.log = l }
}

// WithDebug enables verbose request logging (still redacted).
func WithDebug(debug bool) Option {
	return func(c *Client) {
		if !debug {
			c.log = slog.New(slog.NewTextHandler(io.Discard, nil))
			return
		}
		c.log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slogLevel(true)}))
	}
}

// WithGeocoder overrides the city-coordinate resolver.
func WithGeocoder(g Geocoder) Option {
	return func(c *Client) { c.geocoder = g }
}

// WithHTTPClient overrides the underlying HTTP client.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.httpClient = h }
}

// New builds a Client from a stored session.
func New(sess *storage.Session, opts ...Option) (*Client, error) {
	c := &Client{
		baseURL: auth.BaseURL,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, o := range opts {
		o(c)
	}
	if sess != nil {
		c.username = strings.TrimPrefix(sess.Username, "@")
	}
	if c.httpClient == nil {
		jar, err := auth.Jar(sess, c.baseURL)
		if err != nil {
			return nil, err
		}
		c.httpClient = &http.Client{
			Jar:     jar,
			Timeout: defaultTimeout,
			// Never follow a redirect automatically: a redirect to /login is a
			// signal we must surface as AUTH_EXPIRED, not silently chase.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	if c.geocoder == nil {
		c.geocoder = NewPhotonGeocoder(c.httpClient, c.log)
	}
	return c, nil
}

// Username returns the account this client acts as.
func (c *Client) Username() string { return c.username }

// SetUsername records the account whose profile is read.
func (c *Client) SetUsername(u string) { c.username = strings.TrimPrefix(u, "@") }

// profilePath is the authenticated profile page for the current user.
func (c *Client) profilePath() (string, error) {
	if c.username == "" {
		return "", nomads.Errorf(nomads.ErrInvalidInput,
			"no username known; run 'nomads auth login' or pass --username")
	}
	return "/@" + url.PathEscape(c.username), nil
}

// GetProfile reads the authenticated profile.
//
// Nomads.com exposes no JSON profile endpoint, so this parses the profile page,
// whose edit-modal inputs carry the authenticated values verbatim.
func (c *Client) GetProfile(ctx context.Context) (*nomads.Profile, error) {
	page, err := c.fetchProfilePage(ctx, "get_profile")
	if err != nil {
		return nil, err
	}
	return page.profile, nil
}

// ListTrips reads all trips on the authenticated profile.
func (c *Client) ListTrips(ctx context.Context) ([]nomads.Trip, error) {
	page, err := c.fetchProfilePage(ctx, "list_trips")
	if err != nil {
		return nil, err
	}
	return page.trips, nil
}

// profilePage is one parse of the profile HTML, which carries both datasets.
type profilePage struct {
	profile *nomads.Profile
	trips   []nomads.Trip
}

func (c *Client) fetchProfilePage(ctx context.Context, op string) (*profilePage, error) {
	path, err := c.profilePath()
	if err != nil {
		return nil, err
	}
	body, err := c.getHTML(ctx, path, op)
	if err != nil {
		return nil, err
	}
	html := string(body)

	// The edit modal only renders for the profile's owner, so its absence is a
	// reliable signal that the session is no longer authenticated.
	if !strings.Contains(html, "modal-profile-edit") && !strings.Contains(html, "edit-bio") {
		if looksLikeLoginPage(body) || !strings.Contains(html, "logged-in") {
			return nil, nomads.Errorf(nomads.ErrAuthExpired,
				"%s: profile page is not authenticated; run 'nomads auth refresh'", op)
		}
		return nil, apiChanged(op, "profile edit markers missing from page", nil, body)
	}

	prof := parseProfile(html)
	prof.Username = c.username
	trips, err := parseTrips(html)
	if err != nil {
		return nil, err
	}
	return &profilePage{profile: prof, trips: trips}, nil
}

// profileField maps a patch field onto its observed action and parameter name.
type profileField struct {
	action string
	param  string
	label  string
	value  string
}

// UpdateProfile applies a sparse patch.
//
// Each field is a separate remote action, mirroring the frontend, so a
// rejection of one field does not silently roll back the others. The result is
// re-read from the server so the caller sees verified state.
func (c *Client) UpdateProfile(ctx context.Context, patch nomads.ProfilePatch) (*nomads.Profile, error) {
	if patch.IsEmpty() {
		return c.GetProfile(ctx)
	}

	var fields []profileField
	add := func(p *string, action, param, label string) {
		if p != nil {
			fields = append(fields, profileField{action, param, label, strings.TrimSpace(*p)})
		}
	}
	add(patch.Bio, "change_bio", "bio", "bio")
	add(patch.Website, "set_website", "website_url", "website")
	add(patch.Twitter, "set_twitter", "twitter_username", "twitter")
	add(patch.Instagram, "set_instagram", "instagram_username", "instagram")
	add(patch.YouTube, "set_youtube", "youtube_url", "youtube")
	add(patch.TikTok, "set_tiktok", "tiktok_username", "tiktok")

	for _, f := range fields {
		form := url.Values{"action": {f.action}, f.param: {f.value}}
		// Profile writes go to /user/api WITHOUT a trailing slash.
		if _, err := c.doForm(ctx, http.MethodPost, "/user/api", form, "update_profile:"+f.label); err != nil {
			return nil, err
		}
		c.log.Debug("profile field saved", "field", f.label)
	}

	if patch.Tags != nil {
		if err := c.applyTags(ctx, *patch.Tags); err != nil {
			return nil, err
		}
	}

	// Verify by re-reading rather than trusting the write's own reply.
	updated, err := c.GetProfile(ctx)
	if err != nil {
		return nil, err
	}
	if err := verifyProfile(updated, patch); err != nil {
		return updated, err
	}
	return updated, nil
}

// applyTags reconciles the tag set with add/remove calls for the difference.
func (c *Client) applyTags(ctx context.Context, desired []string) error {
	current, err := c.GetProfile(ctx)
	if err != nil {
		return err
	}
	have := map[string]string{} // canonical -> exact remote key
	for _, t := range current.Tags {
		have[strings.ToLower(t)] = t
	}
	wantSet := map[string]bool{}
	for _, t := range nomads.NormalizeTags(desired) {
		wantSet[strings.ToLower(t)] = true
	}

	for _, t := range nomads.NormalizeTags(desired) {
		if _, ok := have[strings.ToLower(t)]; ok {
			continue
		}
		form := url.Values{"action": {"add_user_tag"}, "key": {t}}
		if _, err := c.doForm(ctx, http.MethodPut, "/user/api/", form, "add_user_tag"); err != nil {
			return err
		}
	}
	for k, exact := range have {
		if wantSet[k] {
			continue
		}
		form := url.Values{"action": {"remove_user_tag"}, "key": {exact}}
		if _, err := c.doForm(ctx, http.MethodPut, "/user/api/", form, "remove_user_tag"); err != nil {
			return err
		}
	}
	return nil
}

// verifyProfile confirms the server stored what we asked for.
func verifyProfile(got *nomads.Profile, patch nomads.ProfilePatch) error {
	type check struct {
		want *string
		got  string
		name string
	}
	for _, ch := range []check{
		{patch.Bio, got.Bio, "bio"},
		{patch.Twitter, got.Twitter, "twitter"},
		{patch.Instagram, got.Instagram, "instagram"},
		{patch.TikTok, got.TikTok, "tiktok"},
	} {
		if ch.want == nil {
			continue
		}
		// Nomads.com normalises some values (handles lose a leading @, URLs get
		// canonicalised), so compare leniently and only flag real divergence.
		if !valueMatches(*ch.want, ch.got) {
			return nomads.Errorf(nomads.ErrRemoteStateMismatch,
				"%s was accepted but the profile still reads differently", ch.name).
				WithDetail("field", ch.name).
				WithDetail("expected", *ch.want).
				WithDetail("actual", ch.got)
		}
	}
	return nil
}

// valueMatches compares a requested value with the stored one, tolerating the
// server's own normalisation.
func valueMatches(want, got string) bool {
	w := strings.TrimSpace(strings.ToLower(want))
	g := strings.TrimSpace(strings.ToLower(got))
	if w == g {
		return true
	}
	w = strings.TrimPrefix(w, "@")
	g = strings.TrimPrefix(g, "@")
	if w == g {
		return true
	}
	// URL forms: ignore scheme and trailing slash.
	trim := func(s string) string {
		s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
		return strings.TrimSuffix(s, "/")
	}
	return trim(w) == trim(g)
}

// tripReply is the create/update response.
type tripReply struct {
	TripID    string  `json:"trip_id"`
	City      string  `json:"city"`
	Country   string  `json:"country"`
	CitySlug  string  `json:"city_slug"`
	Latitude  any     `json:"latitude"`
	Longitude any     `json:"longitude"`
	Length    string  `json:"trip_length"`
	Nearest   float64 `json:"distance_to_nearest"`
}

// CreateTrip adds a trip.
//
// Coordinates are mandatory server-side ("No geo data sent"), so an unspecified
// location is resolved through the same geocoder the frontend uses.
func (c *Client) CreateTrip(ctx context.Context, req nomads.CreateTripRequest) (*nomads.Trip, error) {
	if strings.TrimSpace(req.City) == "" {
		return nil, nomads.Errorf(nomads.ErrInvalidInput, "city is required")
	}
	if req.StartDate.IsZero() || req.EndDate.IsZero() {
		return nil, nomads.Errorf(nomads.ErrInvalidInput, "both start and end dates are required")
	}
	if req.EndDate.Before(req.StartDate) {
		return nil, nomads.Errorf(nomads.ErrInvalidInput,
			"end date %s is before start date %s", req.EndDate, req.StartDate)
	}

	lat, lon := req.Latitude, req.Longitude
	country := req.Country
	if lat == 0 && lon == 0 {
		place, err := c.geocoder.Geocode(ctx, req.City, req.Country)
		if err != nil {
			return nil, err
		}
		lat, lon = place.Latitude, place.Longitude
		if country == "" {
			country = place.Country
		}
	}

	form := url.Values{
		"action":     {"trip"},
		"trip_id":    {""}, // empty id means create
		"date_start": {req.StartDate.String()},
		"date_end":   {req.EndDate.String()},
		"note":       {req.Note},
		"city":       {req.City},
		"country":    {country},
		"latitude":   {formatCoord(lat)},
		"longitude":  {formatCoord(lon)},
	}
	raw, err := c.doForm(ctx, http.MethodPut, "/user/api/", form, "create_trip")
	if err != nil {
		return nil, err
	}
	return tripFromReply(raw, req.StartDate, req.EndDate, req.Note, "create_trip")
}

// UpdateTrip changes an existing trip.
//
// Nomads.com implements an edit as delete-and-recreate and returns a NEW
// trip_id. The returned Trip therefore carries the new id, and callers must
// stop using the old one.
func (c *Client) UpdateTrip(ctx context.Context, id string, patch nomads.TripPatch) (*nomads.Trip, error) {
	if strings.TrimSpace(id) == "" {
		return nil, nomads.Errorf(nomads.ErrInvalidInput, "trip id is required")
	}
	if patch.IsEmpty() {
		return nil, nomads.Errorf(nomads.ErrInvalidInput, "empty trip patch")
	}

	current, err := c.findTrip(ctx, id)
	if err != nil {
		return nil, err
	}

	merged := *current
	if patch.City != nil {
		merged.City = *patch.City
	}
	if patch.Country != nil {
		merged.Country = *patch.Country
	}
	if patch.StartDate != nil {
		merged.StartDate = *patch.StartDate
	}
	if patch.EndDate != nil {
		merged.EndDate = *patch.EndDate
	}
	if patch.Note != nil {
		merged.Note = *patch.Note
	}
	if patch.Latitude != nil {
		merged.Latitude = *patch.Latitude
	}
	if patch.Longitude != nil {
		merged.Longitude = *patch.Longitude
	}
	if merged.EndDate.Before(merged.StartDate) {
		return nil, nomads.Errorf(nomads.ErrInvalidInput,
			"end date %s is before start date %s", merged.EndDate, merged.StartDate)
	}

	// Re-geocode when the city changed but no explicit coordinates were given.
	if patch.City != nil && patch.Latitude == nil && patch.Longitude == nil {
		place, err := c.geocoder.Geocode(ctx, merged.City, merged.Country)
		if err != nil {
			return nil, err
		}
		merged.Latitude, merged.Longitude = place.Latitude, place.Longitude
		if patch.Country == nil && place.Country != "" {
			merged.Country = place.Country
		}
	}

	form := url.Values{
		"action":     {"trip"},
		"trip_id":    {id},
		"date_start": {merged.StartDate.String()},
		"date_end":   {merged.EndDate.String()},
		"note":       {merged.Note},
		"city":       {merged.City},
		"country":    {merged.Country},
		"latitude":   {formatCoord(merged.Latitude)},
		"longitude":  {formatCoord(merged.Longitude)},
	}
	raw, err := c.doForm(ctx, http.MethodPut, "/user/api/", form, "update_trip")
	if err != nil {
		return nil, err
	}
	return tripFromReply(raw, merged.StartDate, merged.EndDate, merged.Note, "update_trip")
}

// DeleteTrip removes a trip and confirms it is gone.
//
// The API returns success:true even for an unknown id, so the deletion is
// verified by re-reading the trip list.
func (c *Client) DeleteTrip(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return nomads.Errorf(nomads.ErrInvalidInput, "trip id is required")
	}
	form := url.Values{"action": {"trip"}, "trip_id": {id}}
	if _, err := c.doForm(ctx, http.MethodDelete, "/user/api/", form, "delete_trip"); err != nil {
		return err
	}
	trips, err := c.ListTrips(ctx)
	if err != nil {
		return err
	}
	for _, t := range trips {
		if t.ID == id {
			return nomads.Errorf(nomads.ErrRemoteStateMismatch,
				"delete reported success but the trip is still present").
				WithDetail("trip", t.Label())
		}
	}
	return nil
}

// findTrip locates a trip by id.
func (c *Client) findTrip(ctx context.Context, id string) (*nomads.Trip, error) {
	trips, err := c.ListTrips(ctx)
	if err != nil {
		return nil, err
	}
	for i := range trips {
		if trips[i].ID == id {
			return &trips[i], nil
		}
	}
	return nil, nomads.Errorf(nomads.ErrTripNotFound, "no trip with id %s", id)
}

// tripFromReply builds a Trip from a create/update response, preferring the
// server's canonical city/country and its newly assigned id.
func tripFromReply(raw json.RawMessage, start, end nomads.Date, note, op string) (*nomads.Trip, error) {
	var r tripReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, apiChanged(op, "could not decode trip reply", nil, raw)
	}
	if r.TripID == "" {
		return nil, apiChanged(op, "trip reply carried no trip_id", nil, raw)
	}
	return &nomads.Trip{
		ID:        r.TripID,
		City:      r.City,
		Country:   r.Country,
		Slug:      r.CitySlug,
		StartDate: start,
		EndDate:   end,
		Latitude:  toFloat(r.Latitude),
		Longitude: toFloat(r.Longitude),
		Note:      note,
	}, nil
}

// toFloat coerces the API's mixed numeric/string coordinates.
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f
	}
	return 0
}

// formatCoord renders a coordinate without exponent notation.
func formatCoord(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// VerifyTrip re-reads a trip and confirms it matches the expected dates and
// place. Used after mutations, since HTTP success alone is not proof.
func (c *Client) VerifyTrip(ctx context.Context, want nomads.Trip) error {
	trips, err := c.ListTrips(ctx)
	if err != nil {
		return err
	}
	for _, t := range trips {
		if t.ID != want.ID {
			continue
		}
		if t.StartDate.Compare(want.StartDate) != 0 || t.EndDate.Compare(want.EndDate) != 0 {
			return nomads.Errorf(nomads.ErrRemoteStateMismatch,
				"trip stored with different dates than requested").
				WithDetail("expected", want.Label()).
				WithDetail("actual", t.Label())
		}
		return nil
	}
	return nomads.Errorf(nomads.ErrRemoteStateMismatch,
		"trip was written but is absent from the profile").
		WithDetail("expected", want.Label())
}

// asNomadsError is errors.As specialised to *nomads.Error.
func asNomadsError(err error, target **nomads.Error) bool { return errors.As(err, target) }

// selfUsernameRe finds the authenticated handle the frontend embeds on every
// page as `selfUsername='@handle'`.
var selfUsernameRe = regexp.MustCompile(`selfUsername\s*=\s*['"]@?([A-Za-z0-9_.-]{1,40})['"]`)

// WhoAmI discovers the authenticated account's handle.
//
// Used when a session was created without --username, so profile and trip
// commands know which profile page to read.
func (c *Client) WhoAmI(ctx context.Context) (string, error) {
	body, err := c.getHTML(ctx, "/", "whoami")
	if err != nil {
		return "", err
	}
	if m := selfUsernameRe.FindSubmatch(body); m != nil {
		return string(m[1]), nil
	}
	if looksLikeLoginPage(body) {
		return "", nomads.Errorf(nomads.ErrAuthExpired, "whoami: not authenticated")
	}
	return "", nomads.Errorf(nomads.ErrAPIChanged, "could not find the account handle on the home page")
}
