package client

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/arcbjorn/nomads-agent/internal/auth"
	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

// PhotonEndpoint is the geocoder the Nomads.com frontend itself uses. It is
// OSM-based, free and needs no API key, so resolving coordinates locally
// reproduces exactly what the website would have sent.
const PhotonEndpoint = "https://photon.komoot.io/api/"

// Place is a resolved location.
type Place struct {
	City      string
	Country   string
	Latitude  float64
	Longitude float64
}

// Geocoder resolves a city name to coordinates.
type Geocoder interface {
	Geocode(ctx context.Context, city, country string) (*Place, error)
}

// PhotonGeocoder resolves cities through Photon.
type PhotonGeocoder struct {
	http     *http.Client
	log      *slog.Logger
	endpoint string
}

// NewPhotonGeocoder builds a geocoder sharing the caller's HTTP client.
func NewPhotonGeocoder(h *http.Client, log *slog.Logger) *PhotonGeocoder {
	return &PhotonGeocoder{http: h, log: log, endpoint: PhotonEndpoint}
}

// WithEndpoint overrides the geocoding endpoint, for tests.
func (g *PhotonGeocoder) WithEndpoint(u string) *PhotonGeocoder {
	g.endpoint = u
	return g
}

// photonResponse is the GeoJSON subset we need.
type photonResponse struct {
	Features []struct {
		Geometry struct {
			Coordinates []float64 `json:"coordinates"` // [lon, lat]
		} `json:"geometry"`
		Properties struct {
			Name    string `json:"name"`
			City    string `json:"city"`
			Country string `json:"country"`
			OSMKey  string `json:"osm_key"`
			OSMVal  string `json:"osm_value"`
			Type    string `json:"type"`
		} `json:"properties"`
	} `json:"features"`
}

// Geocode resolves city/country to coordinates, preferring populated places and
// a matching country.
func (g *PhotonGeocoder) Geocode(ctx context.Context, city, country string) (*Place, error) {
	city = strings.TrimSpace(city)
	if city == "" {
		return nil, nomads.Errorf(nomads.ErrInvalidInput, "city is required for geocoding")
	}
	query := city
	if c := strings.TrimSpace(country); c != "" {
		query = city + ", " + c
	}

	u := g.endpoint + "?" + url.Values{
		"q":     {query},
		"lang":  {"en"},
		"limit": {"10"},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nomads.Wrap(err, nomads.ErrInvalidInput, "build geocode request")
	}
	req.Header.Set("User-Agent", auth.UserAgent)
	req.Header.Set("Accept", "application/json")

	// The geocoder is third-party: never send it a cookie.
	client := &http.Client{Timeout: g.http.Timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nomads.Wrap(err, nomads.ErrGeocodeFailed,
			"could not reach the geocoder for %q", city)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nomads.Errorf(nomads.ErrGeocodeFailed,
			"geocoder returned HTTP %d for %q", resp.StatusCode, city)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, nomads.Wrap(err, nomads.ErrGeocodeFailed, "reading geocoder response")
	}
	var pr photonResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, nomads.Wrap(err, nomads.ErrGeocodeFailed, "geocoder returned unexpected data")
	}
	if len(pr.Features) == 0 {
		return nil, nomads.Errorf(nomads.ErrGeocodeFailed,
			"no coordinates found for %q; pass --lat and --lon explicitly", query).
			WithDetail("query", query)
	}

	wantCountry := nomads.CanonicalName(country)
	wantCity := nomads.CanonicalName(city)

	best := -1
	bestScore := -1
	for i, f := range pr.Features {
		if len(f.Geometry.Coordinates) < 2 {
			continue
		}
		p := f.Properties
		score := 0
		// Prefer a real settlement over a street or POI.
		if p.OSMKey == "place" {
			score += 4
		}
		switch p.Type {
		case "city":
			score += 3
		case "district", "locality", "town", "village":
			score += 2
		}
		if wantCountry != "" && nomads.CanonicalName(p.Country) == wantCountry {
			score += 5
		}
		name := p.Name
		if name == "" {
			name = p.City
		}
		if nomads.CanonicalName(name) == wantCity {
			score += 3
		}
		if score > bestScore {
			bestScore, best = score, i
		}
	}
	if best < 0 {
		return nil, nomads.Errorf(nomads.ErrGeocodeFailed,
			"geocoder returned no usable coordinates for %q", query)
	}

	f := pr.Features[best]
	name := f.Properties.Name
	if name == "" {
		name = f.Properties.City
	}
	if name == "" {
		name = city
	}
	resolvedCountry := f.Properties.Country
	if strings.TrimSpace(country) != "" {
		// Trust the caller's country spelling when they gave one.
		resolvedCountry = country
	}
	g.log.Debug("geocoded city", "query", query, "resolved", name, "country", resolvedCountry)

	return &Place{
		City:      name,
		Country:   resolvedCountry,
		Longitude: f.Geometry.Coordinates[0],
		Latitude:  f.Geometry.Coordinates[1],
	}, nil
}

// StaticGeocoder resolves from a fixed table. Used in tests and as an override.
type StaticGeocoder struct{ Places map[string]Place }

// Geocode looks up the city in the static table.
func (s *StaticGeocoder) Geocode(_ context.Context, city, country string) (*Place, error) {
	if p, ok := s.Places[nomads.CanonicalPlace(city, country)]; ok {
		return &p, nil
	}
	if p, ok := s.Places[nomads.CanonicalName(city)]; ok {
		return &p, nil
	}
	return nil, nomads.Errorf(nomads.ErrGeocodeFailed, "no static coordinates for %q", city)
}
