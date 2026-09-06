# Nomads.com API observations

Two very different interfaces are involved. Prefer the first.

1. **The official API** (`https://nomads.com/api`, `https://nomads.com/mcp`) —
   documented, key-authenticated, announced at `https://nomads.com/llms.txt`.
   Observed 2026-09-06.
2. **The first-party web endpoints** (`/user/api`) — undocumented, session-
   authenticated, reverse-engineered from the site's own JavaScript. Used only
   for the operations the official API does not expose.

## Capability split

| Operation | Official API | First-party endpoints |
| --- | --- | --- |
| List trips | `GET /api/trips` | profile HTML |
| Add trip | `POST /api/trips` | `PUT /user/api/` |
| **Update trip** | *not available* | `PUT /user/api/` |
| **Delete trip** | *not available* | `DELETE /user/api/` |
| **Read profile** | *not available* | profile HTML |
| **Update profile** | *not available* | `POST /user/api` |
| **Tags** | *not available* | `PUT /user/api/` |

`internal/client/hybrid.go` routes each call: the official API wherever it can
do the job, the private client only for the rest.

---

# Part 1 — The official API

Self-describing: `GET https://nomads.com/api` returns its own endpoint list.

```
Base:   https://nomads.com/api
Auth:   username + personal key from https://nomads.com/settings
Limits: 100 results per search, 60 requests/hour per IP
Docs:   https://nomads.com/llms.txt
MCP:    https://nomads.com/mcp  (Streamable HTTP, no auth for city search)
```

### `GET /api/trips`

```
GET /api/trips?username=<handle>&key=<personal key>
```

Returns the member's own trip history. The key travels in the query string, so
the URL must never be logged verbatim — `auth.RedactURL` handles this.

### `POST /api/trips`

```json
{
  "username": "<handle>",
  "key": "<personal key>",
  "date_start": "2030-04-10",
  "date_end": "2030-04-18",
  "city_slug": "lisbon-portugal"
}
```

The place is given as **either** `city_slug` (from `search_cities`/`get_city`)
**or** `latitude` + `longitude`, which are snapped to the nearest Nomads.com
city within 80km. An optional `city` name may accompany coordinates for
reference.

### Errors

Returned as a JSON envelope, **sometimes with HTTP 200**, so the body must be
inspected rather than the status alone:

```json
{"error": "invalid_username_or_key", "detail": "Get your key at https://nomads.com/settings"}
{"error": "missing_params", "detail": "username and key are both required"}
```

Mapped to `AUTH_EXPIRED` and `INVALID_INPUT` respectively.

### Verified absent: update, delete and profile

Probed 2026-09-06. The official API's trip support is **append-only**; there is
no way to edit or remove a trip, and nothing for profile fields. This was
established by testing rather than inferred from the docs.

**Other HTTP verbs on `/api/trips` are not separate operations.** `PUT`,
`PATCH` and `DELETE` all return HTTP 200, which looks like support, but the
same handler answers every verb:

| Request | Response |
| --- | --- |
| `PUT`/`PATCH`/`DELETE` + credentials in the JSON body | `missing_params` |
| `PUT`/`DELETE` + credentials in the query string | `invalid_username_or_key` |
| `GET` + credentials in the query string (documented read) | `invalid_username_or_key` |

The body is not parsed for non-`POST` verbs, and the credential check is
identical to `GET`. Nothing verb-specific exists behind them.

**No hidden MCP tools.** `tools/list` returns exactly five. Calling any of
`update_trip`, `delete_trip`, `remove_trip`, `edit_trip`, `get_profile`,
`update_profile`, `set_bio` or `sync_trips` returns *unknown tool*.
`resources/list` and `prompts/list` return `-32601 Method not found`, so the
server is tools-only.

**No undocumented REST paths.** `/api/profile`, `/api/user`, `/api/me`,
`/api/trip`, `/api/trips/delete`, `/api/bio` and `/api/tags` all fall through
to the `/api` index page rather than resolving.

`llms.txt` agrees: it contains no occurrence of "update", "delete", "edit" or
"profile".

This is what the hybrid client in `internal/client/hybrid.go` routes around.

### Capability overlap with this project

Of the nine MCP tools this project exposes, **two overlap** the official API and
**seven have no official equivalent**:

| Our tool | Official equivalent | Notes |
| --- | --- | --- |
| `nomads_list_trips` | `get_trips` | overlaps — we call their API for it |
| `nomads_add_trip` | `add_trip` | overlaps — we call their API for it |
| `nomads_update_trip` | none | no endpoint exists |
| `nomads_delete_trip` | none | no endpoint exists |
| `nomads_get_profile` | none | no endpoint exists |
| `nomads_update_profile` | none | no endpoint exists |
| `nomads_plan_trip_sync` | none | higher-level: non-mutating diff |
| `nomads_sync_trips` | none | higher-level: idempotent reconcile |
| `nomads_capabilities` | none | reports which backend serves what |

The two overlapping tools are routing wrappers, not reimplementations: the
hybrid client calls the official API and falls back to the first-party
endpoints only on an auth or availability failure.

Their `search_cities`, `get_city` and `list_meetups` are **not** duplicated
here — run both servers side by side.

### The official MCP server

`https://nomads.com/mcp` speaks MCP over Streamable HTTP with no authentication
for the public tools: `search_cities`, `get_city`, `list_meetups`, and the
key-authenticated `get_trips` / `add_trip`. It is a fine complement to this
project, which adds the editing and profile operations it lacks.

---

# Part 2 — The first-party web endpoints

> Undocumented. Reverse-engineered from the site's own JavaScript on
> 2026-09-06, and used only where the official API has no equivalent. These can
> change without notice; `NOMADS_API_CHANGED` is raised when they do.

## Sources

| File | Role |
| --- | --- |
| `https://nomads.com/profile.js` | Trip CRUD, tags, settings |
| inline `<script>` on `/@username` | Profile edit modal |
| `https://nomads.com/global.js` | `get_user_info`, social graph |

## Transport

The action is a **form field, not a path**, and the HTTP verb carries meaning:

```
Endpoint:     https://nomads.com/user/api/   (trailing slash: trips, tags, settings)
              https://nomads.com/user/api    (no slash: profile fields)
Content-Type: application/x-www-form-urlencoded
Auth:         session cookies only — no CSRF token
```

| Verb | Meaning |
| --- | --- |
| `PUT` | create **and** update (trips, tags, settings) |
| `DELETE` | delete (trips) |
| `POST` | profile field writes |

## Authentication

Login is by emailed magic link:

```
GET /user/api?action=login_by_email&hash=<40-hex-secret>
```

The `hash` is a **permanent credential** — treat it exactly like a password.

### The link cannot be redeemed programmatically

Observed 2026-09-06: the same URL that authenticates successfully in a browser
returns

```
Link is corrupted! Try opening it in a different email app or request a new
login link.
```

when fetched by curl or Go, while still setting a fresh `PHPSESSID`. This was
reproduced with a full set of browser headers (`User-Agent`, `Accept`,
`Accept-Language`, `Sec-Fetch-*`, `Upgrade-Insecure-Requests`) and with
compression enabled, so it is **not** header-based. The link remains valid in
the browser afterwards, so it is also not single-use.

The most likely cause is a TLS/HTTP2 fingerprint check (Cloudflare JA3-style),
which an HTTP client cannot pass without impersonating a browser stack.

**Consequence for this project:** `nomads auth login` is kept because it is the
documented flow and may start working again, but the reliable path is
`nomads auth import`, which lifts the already-authenticated cookies out of a
browser. See `internal/auth/browser`. This is exactly the "browser bootstrap"
fallback the design calls for, and it keeps Playwright out of the runtime: the
user copies a cookie string, nothing drives a browser.

### Session cookies

| Cookie | Role |
| --- | --- |
| `logged_in_hash` | the actual authentication token (required) |
| `PHPSESSID` | PHP session id |

Other cookies (`ref`, `last_tested_internet_speed`, ...) are analytics or UI
state and are deliberately **not** imported.

> On at least some accounts the authenticated `user_id` is the same hex value
> as the login hash. Never log or commit either.

## Trips

### Read

There is no first-party JSON trip list. The profile HTML carries one
`<tr class="trip">` per trip with structured data attributes:

```html
<tr class="trip ... <trip-id>"
    data-trip-id="<50-hex>"
    data-date-start="2029-02-03"  data-date-end="2029-02-11"
    data-epoch-start="1864684800" data-epoch-end="1865376000"
    data-slug="malaga-spain"
    data-latitude="36.7213"       data-longitude="-4.4213">
  <td class="name"><h2>Málaga</h2></td>
  <td class="country">Spain</td>
```

`data-date-*` are plain ISO dates and are what we parse; the epochs are UTC
midnight and are ignored to avoid timezone drift.

The editor template row also carries `class="trip"` but no `data-trip-id`, so it
must be skipped. If rows are present but none parse, that is reported as
`NOMADS_API_CHANGED` rather than "no trips" — silently returning an empty list
would make a sync plan try to recreate the user's entire history.

### Create / update — `PUT /user/api/`

```
action=trip
trip_id=            # empty creates; an existing id updates
date_start=2030-04-10
date_end=2030-04-18
note=
city=Lisbon
country=Portugal
latitude=38.7223
longitude=-9.1393
```

Two behaviours drive the client design:

1. **Coordinates are mandatory.** Omitting them fails with
   `{"success": false, "message": "No geo data sent"}`.
2. **An update REASSIGNS the trip id.** Editing returns a *different*
   `trip_id`: the backend deletes and recreates the row. A client that keeps
   the old id operates on a dead record. Verified experimentally; the client
   always adopts `reply.trip_id`.

`changed_to_nearest` means the server snapped the coordinates to a known city,
so the returned `city`/`country` are canonical and may differ from the input.

### Delete — `DELETE /user/api/`

```
action=trip
trip_id=<id>
```

**`success: true` does not prove a deletion happened** — deleting an unknown or
stale id also returns `true`. Verified experimentally. Deletion is therefore
confirmed by re-reading the trip list.

### Geocoding (client-side, third party)

`profile.js` resolves coordinates through Photon (komoot, OSM-based — free, no
key), and this project uses the same service so results match:

```
GET https://photon.komoot.io/api/?q=<city>&lang=en&limit=10
```

`features[].geometry.coordinates` is `[lon, lat]`. No cookie is ever sent to it.

## Profile fields — `POST /user/api`

Each field is an **independent action**, and the frontend only sends fields
whose value changed — which maps exactly onto `ProfilePatch`'s nil-means-
unchanged semantics.

| Field | action | parameter |
| --- | --- | --- |
| Bio | `change_bio` | `bio` |
| Username | `change_username` | `username` |
| Website | `set_website` | `website_url` |
| Twitter/X | `set_twitter` | `twitter_username` |
| Instagram | `set_instagram` | `instagram_username` |
| YouTube | `set_youtube` | `youtube_url` |
| TikTok | `set_tiktok` | `tiktok_username` |
| Date of birth | `change_date_of_birth` | `date_of_birth` |
| Nationality | `change_nationality` | `country_code` |
| Residency | `change_residency` | `country_code` |
| Income | `change_income` | `income` |
| Employment | `change_employment_type` | `employment_type` |
| Works from | `change_works_from` | `works_from` |

Response: `{"success": true, "new_bio": "..."}`.

**Validation:** a blank bio is rejected with `{"success": false, "message":
"Can't be blank"}` → `PROFILE_VALIDATION_FAILED`. Note a bio cannot be reset to
empty once set.

Values are read back from the edit-modal inputs (`.edit-bio`, `.edit-website`,
`.edit-twitter`, `.edit-instagram`, `.edit-youtube`, `.edit-tiktok`). These
render only for the profile's owner, which doubles as the session check.

## Tags — `PUT /user/api/`

```
action=add_user_tag     # or remove_user_tag
key=<category>_<key>    # e.g. "work_Web Dev"
```

320 tags across categories. The active set is read from
`.match-settings .tags span.active` via `data-category` and `data-key`.

## Other observed actions (not used)

`copy-trip`, `split_trip`, `follow_user`, `unfollow_user`, `like_user`,
`favorite-city`, `like_city`, `match_settings`, `set_profile_photo`,
`rotate_profile_photo`, `setting`, `log_user_activity`.

---

## Recapturing the API if it changes

1. Open an authenticated profile at `https://nomads.com/@<handle>`.
2. In DevTools, watch the Network tab (filter: Fetch/XHR) while editing a bio,
   toggling one tag, and adding, editing and deleting a trip.
3. Compare the form fields against the tables above.
4. The handlers live in `profile.js` and in an inline `<script>` on the profile
   page; grep them for `/user/api` to find every call site.
5. Update this file and `internal/client/nomads_http.go` together.

Also re-check `https://nomads.com/api` and `https://nomads.com/llms.txt`: an
operation that needed the private client may since have become official.
