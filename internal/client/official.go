package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/arcbjorn/nomads-agent/internal/auth"
	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

// OfficialBaseURL is the documented Nomads.com JSON API, announced at
// https://nomads.com/api and https://nomads.com/llms.txt.
const OfficialBaseURL = "https://nomads.com/api"

// OfficialRateLimit is the published quota: 60 requests/hour per IP.
const OfficialRateLimit = 60

// Official is a client for the documented Nomads.com API.
//
// Unlike the reverse-engineered client this speaks a supported, versioned
// contract authenticated by a personal key from https://nomads.com/settings.
// It is the preferred path for everything it covers, which today is reading
// trip history and appending a trip.
type Official struct {
	http     *http.Client
	baseURL  string
	username string
	key      string
	log      *slog.Logger
}

// OfficialOption configures an Official client.
type OfficialOption func(*Official)

// WithOfficialBaseURL overrides the API origin, for tests.
func WithOfficialBaseURL(u string) OfficialOption {
	return func(o *Official) { o.baseURL = strings.TrimRight(u, "/") }
}

// WithOfficialLogger sets the structured logger.
func WithOfficialLogger(l *slog.Logger) OfficialOption {
	return func(o *Official) { o.log = l }
}

// WithOfficialHTTPClient overrides the HTTP client.
func WithOfficialHTTPClient(h *http.Client) OfficialOption {
	return func(o *Official) { o.http = h }
}

// NewOfficial builds a client for the documented API.
func NewOfficial(username, key string, opts ...OfficialOption) (*Official, error) {
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
	key = strings.TrimSpace(key)
	if username == "" || key == "" {
		return nil, nomads.Errorf(nomads.ErrInvalidInput,
			"the official API needs a username and a personal key from https://nomads.com/settings")
	}
	o := &Official{
		http:     &http.Client{Timeout: defaultTimeout},
		baseURL:  OfficialBaseURL,
		username: username,
		key:      key,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, opt := range opts {
		opt(o)
	}
	return o, nil
}

// Username returns the account this client acts as.
func (o *Official) Username() string { return o.username }

// officialTrip is one entry of the documented trips response.
type officialTrip struct {
	ID        any    `json:"id"`
	TripID    any    `json:"trip_id"`
	City      string `json:"city"`
	Country   string `json:"country"`
	CitySlug  string `json:"city_slug"`
	Slug      string `json:"slug"`
	DateStart string `json:"date_start"`
	DateEnd   string `json:"date_end"`
	Latitude  any    `json:"latitude"`
	Longitude any    `json:"longitude"`
}

// officialError is the documented error envelope.
type officialError struct {
	Error  string `json:"error"`
	Detail string `json:"detail"`
}

// ListTrips reads the member's own trip history.
func (o *Official) ListTrips(ctx context.Context) ([]nomads.Trip, error) {
	q := url.Values{"username": {o.username}, "key": {o.key}}
	raw, err := o.do(ctx, http.MethodGet, "/trips?"+q.Encode(), nil, "official_list_trips")
	if err != nil {
		return nil, err
	}

	// The API has returned either a bare array or an object wrapping one; accept
	// both rather than break on a cosmetic change.
	var list []officialTrip
	if err := json.Unmarshal(raw, &list); err != nil {
		var wrapped struct {
			Trips []officialTrip `json:"trips"`
		}
		if err2 := json.Unmarshal(raw, &wrapped); err2 != nil {
			return nil, apiChangedRaw("official_list_trips", "trips response was neither an array nor {trips:[...]}", raw)
		}
		list = wrapped.Trips
	}

	out := make([]nomads.Trip, 0, len(list))
	for _, t := range list {
		start, errS := nomads.ParseDate(t.DateStart)
		end, errE := nomads.ParseDate(t.DateEnd)
		if errS != nil || errE != nil {
			// Skip malformed rows rather than abort the whole read.
			o.log.Debug("skipping trip with unparseable dates",
				"date_start", t.DateStart, "date_end", t.DateEnd)
			continue
		}
		slug := t.CitySlug
		if slug == "" {
			slug = t.Slug
		}
		out = append(out, nomads.Trip{
			ID:        firstNonEmptyID(t.TripID, t.ID),
			City:      t.City,
			Country:   t.Country,
			Slug:      slug,
			StartDate: start,
			EndDate:   end,
			Latitude:  toFloat(t.Latitude),
			Longitude: toFloat(t.Longitude),
		})
	}
	nomads.SortTrips(out)
	return out, nil
}

// CreateTrip appends a trip to the member's history.
//
// The API accepts either a city_slug or latitude+longitude, snapping
// coordinates to the nearest Nomads.com city within 80km.
func (o *Official) CreateTrip(ctx context.Context, req nomads.CreateTripRequest) (*nomads.Trip, error) {
	if req.StartDate.IsZero() || req.EndDate.IsZero() {
		return nil, nomads.Errorf(nomads.ErrInvalidInput, "both start and end dates are required")
	}
	if req.EndDate.Before(req.StartDate) {
		return nil, nomads.Errorf(nomads.ErrInvalidInput,
			"end date %s is before start date %s", req.EndDate, req.StartDate)
	}

	body := map[string]any{
		"username":   o.username,
		"key":        o.key,
		"date_start": req.StartDate.String(),
		"date_end":   req.EndDate.String(),
	}
	switch {
	case req.Slug != "":
		body["city_slug"] = req.Slug
	case req.Latitude != 0 || req.Longitude != 0:
		body["latitude"] = req.Latitude
		body["longitude"] = req.Longitude
		if req.City != "" {
			body["city"] = req.City
		}
	default:
		return nil, nomads.Errorf(nomads.ErrInvalidInput,
			"a city_slug or latitude/longitude is required to add a trip")
	}

	raw, err := o.do(ctx, http.MethodPost, "/trips", body, "official_add_trip")
	if err != nil {
		return nil, err
	}

	// The reply shape is not contractually fixed, so fall back to the request's
	// own values for anything it does not echo.
	var reply officialTrip
	_ = json.Unmarshal(raw, &reply)
	var wrapped struct {
		Trip officialTrip `json:"trip"`
	}
	if reply.DateStart == "" {
		if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Trip.DateStart != "" {
			reply = wrapped.Trip
		}
	}

	trip := nomads.Trip{
		ID:        firstNonEmptyID(reply.TripID, reply.ID),
		City:      firstNonEmpty(reply.City, req.City),
		Country:   firstNonEmpty(reply.Country, req.Country),
		Slug:      firstNonEmpty(reply.CitySlug, reply.Slug, req.Slug),
		StartDate: req.StartDate,
		EndDate:   req.EndDate,
		Latitude:  req.Latitude,
		Longitude: req.Longitude,
		Note:      req.Note,
	}
	if d, err := nomads.ParseDate(reply.DateStart); err == nil {
		trip.StartDate = d
	}
	if d, err := nomads.ParseDate(reply.DateEnd); err == nil {
		trip.EndDate = d
	}
	return &trip, nil
}

// do performs one API call and unwraps the documented error envelope.
func (o *Official) do(ctx context.Context, method, path string, body any, op string) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nomads.Wrap(err, nomads.ErrInvalidInput, "encode request")
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, o.baseURL+path, reader)
	if err != nil {
		return nil, nomads.Wrap(err, nomads.ErrInvalidInput, "build request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", auth.UserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	start := time.Now()
	resp, err := o.http.Do(req)
	if err != nil {
		return nil, nomads.Wrap(err, nomads.ErrNetwork, "%s: %s", op, auth.Redact(err.Error()))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nomads.Wrap(err, nomads.ErrNetwork, "%s: reading response", op)
	}
	// The key travels in the query string, so never log the path verbatim.
	o.log.Debug("official api request",
		"operation", op, "method", method,
		"endpoint", auth.RedactURL(path),
		"status", resp.StatusCode,
		"duration_ms", time.Since(start).Milliseconds())

	if resp.StatusCode == http.StatusTooManyRequests {
		e := nomads.Errorf(nomads.ErrRateLimited,
			"%s: rate limited (the API allows %d requests/hour per IP)", op, OfficialRateLimit)
		if d := retryAfter(resp); d > 0 {
			e.RetryAfter = d
			e.WithDetail("retry_after_seconds", int(d.Seconds()))
		}
		return nil, e
	}
	if looksLikeHTML(raw) {
		return nil, apiChangedRaw(op, "received HTML instead of JSON", raw)
	}

	// Errors come back as a JSON envelope, sometimes with HTTP 200.
	var apiErr officialError
	if err := json.Unmarshal(raw, &apiErr); err == nil && apiErr.Error != "" {
		return nil, o.classify(op, apiErr, resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return nil, nomads.Errorf(nomads.ErrNetwork, "%s: Nomads.com returned HTTP %d", op, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, apiChangedRaw(op, fmt.Sprintf("unexpected HTTP %d", resp.StatusCode), raw)
	}
	return raw, nil
}

// classify maps a documented error code onto a semantic error.
func (o *Official) classify(op string, e officialError, status int) error {
	switch e.Error {
	case "invalid_username_or_key":
		return nomads.Errorf(nomads.ErrAuthExpired,
			"%s: Nomads.com rejected the username or API key; get a new key at https://nomads.com/settings", op)
	case "missing_params":
		return nomads.Errorf(nomads.ErrInvalidInput, "%s: %s", op, e.Detail)
	case "rate_limited", "too_many_requests":
		return nomads.Errorf(nomads.ErrRateLimited,
			"%s: rate limited (the API allows %d requests/hour per IP)", op, OfficialRateLimit)
	case "not_found", "no_city_found":
		return nomads.Errorf(nomads.ErrGeocodeFailed, "%s: %s", op, e.Detail)
	}
	// An unknown code with a message is still a real, reportable failure.
	return nomads.Errorf(nomads.ErrAPIChanged, "%s: %s", op, firstNonEmpty(e.Detail, e.Error)).
		WithDetail("api_error", e.Error).
		WithDetail("status", status)
}

// apiChangedRaw reports a protocol mismatch without an *http.Response.
func apiChangedRaw(op, why string, raw []byte) error {
	return nomads.Errorf(nomads.ErrAPIChanged,
		"%s: %s — see docs/api-observations.md to recapture the API", op, why).
		WithDetail("body_snippet", auth.Redact(snippet(raw)))
}

// firstNonEmpty returns the first non-blank string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// firstNonEmptyID renders the first usable id, which may arrive as a number.
func firstNonEmptyID(vals ...any) string {
	for _, v := range vals {
		switch n := v.(type) {
		case string:
			if strings.TrimSpace(n) != "" {
				return n
			}
		case float64:
			if n != 0 {
				return fmt.Sprintf("%.0f", n)
			}
		}
	}
	return ""
}
