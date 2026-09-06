# nomads-agent

Read and update your [Nomads.com](https://nomads.com) profile and travel
itinerary from the command line or from an AI agent.

The design principle is **desired state**, not button clicking. You describe the
itinerary you want; the tool works out the minimal set of changes, applies them,
and verifies the result. Running the same input twice changes nothing the second
time.

```bash
nomads trips sync trips.yaml --dry-run
```

```
Nomads sync plan

CREATE
  Lisbon, Portugal
  2030-04-10 -> 2030-04-18

UPDATE
  Malaga, Spain
  2030-05-02 -> 2030-05-18
  to:
  2030-05-01 -> 2030-05-20

UNCHANGED
  Porto, Portugal

DELETE
  none

AMBIGUOUS
  none
```

## What it does

- Read your profile: bio, tags, website, social links
- Update the bio, tags and social links
- Read your trips, past and upcoming
- Add, edit and delete trips
- Reconcile your trips against a desired itinerary, idempotently
- Expose all of the above to AI agents over MCP

## Two backends, and why

Nomads.com now publishes an official API (announced at
[/api](https://nomads.com/api) and [/llms.txt](https://nomads.com/llms.txt)).
It covers reading and adding trips, but has no endpoint for editing or deleting
a trip, nor for anything on the profile.

This tool therefore uses both, preferring the supported one:

| Operation | Backend | Needs |
| --- | --- | --- |
| List trips | official API | API key |
| Add trip | official API | API key |
| Update trip | first-party endpoints | browser session |
| Delete trip | first-party endpoints | browser session |
| Read/update profile | first-party endpoints | browser session |
| Tags | first-party endpoints | browser session |

`nomads auth status` shows which of these are currently available to you.

> **The first-party endpoints are undocumented and unofficial.** They were
> reverse-engineered from the site's own JavaScript and can break without
> notice. When they do, the tool reports `NOMADS_API_CHANGED` with enough
> non-secret detail to investigate, and
> [`docs/api-observations.md`](docs/api-observations.md) explains how to
> recapture them. Nothing here is endorsed by Nomads.com; use it on your own
> account and stay within their rate limits.

## Install

Requires Go 1.22 or newer. There are no third-party dependencies.

```bash
git clone <your fork or clone URL>
cd nomads-agent
go build -o bin/nomads ./cmd/nomads
go build -o bin/nomads-mcp ./cmd/nomads-mcp
```

## Authentication

### 1. API key — reading and adding trips

Copy your personal key from <https://nomads.com/settings>:

```bash
nomads auth key --username yourhandle --key <your key>
```

The key is verified against the API before it is stored. If you would rather not
write it to disk, export it instead — environment variables take precedence:

```bash
export NOMADS_USERNAME=yourhandle
export NOMADS_API_KEY=...
```

### 2. Browser session — editing, deleting and profile changes

These have no official endpoint, so they need a real login session.

Nomads.com's magic link **cannot be redeemed by an HTTP client**: the same URL
that works in a browser returns *"Link is corrupted!"* to curl or Go, almost
certainly a TLS-fingerprint check. So the reliable path is to import the
session from a browser you are already logged into:

```bash
nomads auth import          # prints step-by-step instructions
```

In short: open <https://nomads.com> logged in, open DevTools > Console, run
`copy(document.cookie)` — which copies without displaying — then:

```bash
pbpaste | nomads auth import --stdin        # keeps it out of your shell history
```

Or from a JSON cookie export:

```bash
nomads auth import --file cookies.json
```

Only `logged_in_hash` and `PHPSESSID` are stored; everything else in the string
is discarded. The import is verified against the live profile before it is
saved. These cookies are equivalent to a password: they are written with `0600`
permissions and redacted from every log and error message.

If the magic link ever starts working for HTTP clients again, `nomads auth
login --link "<link>"` is still supported.

### Checking and renewing

```bash
nomads auth status     # which credentials work, and what they enable
nomads auth refresh    # renew the session from the stored link
nomads auth logout     # delete all local credentials
```

## CLI

```bash
nomads profile get
nomads profile update --bio "Building things and moving around Europe."
nomads profile update --tags "Web Dev,Software Dev,Sports"

nomads trips list

nomads trips add --city Lisbon --country Portugal \
  --from 2030-04-10 --to 2030-04-18

nomads trips update --id <id> --to 2030-04-25
nomads trips delete --id <id>

nomads trips sync trips.yaml --dry-run
nomads trips sync trips.yaml
nomads trips sync trips.yaml --delete-missing
```

Add `--json` to any command for machine-readable output, and `--debug` for
redacted request logging on stderr.

Only the flags you actually pass are changed. `nomads profile update --bio x`
leaves your website and social links untouched; it does not blank them.

### The itinerary file

```yaml
trips:
  - city: Lisbon
    country: Portugal
    from: 2030-04-10
    to: 2030-04-18

  - city: Malaga
    country: Spain
    from: 2030-05-01
    to: 2030-05-20
    slug: malaga-spain   # optional: pin the exact Nomads.com city
```

See [`trips.example.yaml`](trips.example.yaml). JSON is also accepted.

## Sync behaviour

The planner is pure: it does no network I/O and is fully deterministic, so the
risky decisions are exhaustively unit-tested without an account.

**Matching.** A desired trip is paired with a remote trip by, in order of
confidence: an exact place-and-dates match; the same place and start date; the
same place with overlapping dates. Position in the list is never used. City
names are compared with case and accents folded, so `malaga` matches
`Málaga` and no duplicate is created.

**Ambiguity is never guessed.** If a desired trip could correspond to more than
one remote trip, it is reported as ambiguous and *nothing is mutated*. Resolve it
by narrowing the dates or by editing that trip by id.

**Deletion is opt-in.** By default sync only creates and updates: your desired
list is usually a partial view of a longer travel history, so unmatched remote
trips are left alone. Pass `--delete-missing` to remove them, and
`--delete-after 2026-01-01` to protect everything before a date.

**Verification.** HTTP success is not treated as proof. After writing, the
tool re-reads the remote state and compares it, reporting
`REMOTE_STATE_MISMATCH` if they diverge. This matters because the delete
endpoint returns success even for an id that does not exist.

**Idempotency.** Run the same file twice and the second run makes no
mutations.

## MCP

```bash
claude mcp add nomads /path/to/bin/nomads-mcp
```

Or configure it manually:

```json
{
  "mcpServers": {
    "nomads": {
      "command": "/path/to/bin/nomads-mcp",
      "env": {
        "NOMADS_USERNAME": "yourhandle",
        "NOMADS_API_KEY": "..."
      }
    }
  }
}
```

The server speaks JSON-RPC over stdin/stdout and never opens a network port.

| Tool | Purpose |
| --- | --- |
| `nomads_get_profile` | Read the profile |
| `nomads_update_profile` | Change bio, tags, website, socials |
| `nomads_list_trips` | List all trips |
| `nomads_add_trip` | Add one trip |
| `nomads_update_trip` | Change one trip |
| `nomads_delete_trip` | Delete one trip |
| `nomads_plan_trip_sync` | Preview a reconciliation — **never mutates** |
| `nomads_sync_trips` | Apply a reconciliation |
| `nomads_capabilities` | Report which operations are available |

So an agent can act on:

> "Add Lisbon from Apr 10–18, 2030, Porto Apr 20–30, then Malaga
> from May 1–20."

by calling `nomads_plan_trip_sync` to show you the diff, then
`nomads_sync_trips` to apply it.

Errors are returned as semantic codes rather than raw HTTP details:
`AUTH_EXPIRED`, `PROFILE_VALIDATION_FAILED`, `TRIP_ALREADY_EXISTS`,
`TRIP_AMBIGUOUS`, `TRIP_NOT_FOUND`, `REMOTE_STATE_MISMATCH`, `RATE_LIMITED`,
`NOMADS_API_CHANGED`, `GEOCODE_FAILED`, `INVALID_INPUT`, `NETWORK_ERROR`.

Nomads.com also runs its own MCP server at `https://nomads.com/mcp` for city
search and meetups. It complements this one, which adds the editing and profile
operations it does not cover.

## Security

- Credentials live in `~/.config/nomads-agent/state.json`, written atomically
  with `0600` in a `0700` directory.
- No credential is ever committed, logged or printed. Cookies, API keys, login
  hashes and user ids are redacted from every log line and error message,
  including in `--debug`.
- Request bodies are not logged by default, because they carry your bio and
  itinerary.
- The MCP server is stdio-only and binds no port.
- The geocoder is a third party, so it never receives a cookie or a key.
- `.gitignore` excludes state files, `.env`, real itineraries and captured HTTP
  traffic.

## Troubleshooting

**`AUTH_EXPIRED`.** For the API key, check it at
<https://nomads.com/settings> and re-run `nomads auth key`. For the browser
session, run `nomads auth refresh`, or `nomads auth login` with a fresh magic
link. Note that logging into Nomads.com in your browser with the same link will
invalidate the session stored here.

**`RATE_LIMITED`.** The official API allows 60 requests per hour per IP. The
tool honours `Retry-After` and retries reads with backoff; it never retries a
write automatically, since it cannot tell whether the write already landed.

**`NOMADS_API_CHANGED`.** The site's markup or protocol moved. The error
carries the status, content type and a redacted body snippet.
[`docs/api-observations.md`](docs/api-observations.md) documents every endpoint
and how to recapture them from the frontend.

**`TRIP_AMBIGUOUS`.** Two or more remote trips matched one desired trip.
Nothing was changed. Narrow the dates, or edit the specific trip by id from
`nomads trips list`.

**`REMOTE_STATE_MISMATCH`.** The write reported success but the re-read
disagreed. Run `nomads trips list` to see the actual state; the mutation may
have partly applied.

**An edit changed the trip id.** That is Nomads.com's behaviour, not a bug: it
implements an edit as delete-and-recreate. The tool follows the new id and
reports it.

## Testing

```bash
go test ./...
```

Three levels, none of which touch your account by default:

- **Unit tests** for date handling, canonicalisation, matching, diffing,
  ambiguity and idempotency — pure, no network, no account.
- **Fixture tests** parsing redacted captured HTML in `fixtures/`, and stub
  servers reproducing the real protocol, including its quirks: id reassignment
  on edit, success-on-unknown-delete, and blank-bio rejection.
- **Live integration tests**, which run only with `NOMADS_INTEGRATION_TEST=1`
  and mutate nothing destructively.

```bash
NOMADS_INTEGRATION_TEST=1 go test ./... -run Integration
```

## Project layout

```
cmd/nomads/          CLI
cmd/nomads-mcp/      MCP server
internal/auth/       magic-link redemption, session storage, redaction
internal/client/     official API, first-party endpoints, hybrid routing
internal/sync/       pure planner and executor
internal/storage/    credential file, 0600
internal/trips/      itinerary file parsing
internal/mcp/        JSON-RPC server
pkg/nomads/          domain model, Date, semantic errors
docs/                API observations
fixtures/            redacted captured responses
```

`pkg/nomads` has no knowledge of the network. All remote-specific handling is
confined to `internal/client`, so a change on Nomads.com's side is contained
there.

## License

MIT
