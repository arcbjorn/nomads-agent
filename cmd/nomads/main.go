// Command nomads is the CLI for reading and updating a Nomads.com profile and
// travel itinerary.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/arcbjorn/nomads-agent/internal/auth"
	"github.com/arcbjorn/nomads-agent/internal/auth/browser"
	"github.com/arcbjorn/nomads-agent/internal/client"
	"github.com/arcbjorn/nomads-agent/internal/storage"
	"github.com/arcbjorn/nomads-agent/internal/sync"
	"github.com/arcbjorn/nomads-agent/internal/trips"
	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

const usage = `nomads - manage your Nomads.com profile and trips

Usage:
  nomads <command> [subcommand] [flags]

Commands:
  auth key --username <h> --key <k> Store your API key from nomads.com/settings
  auth login --link <magic link>   Redeem a magic link (enables profile + edit/delete)
  auth import --cookies "<...>"    Import a session from a logged-in browser
  auth status                      Show whether the stored session works
  auth refresh                     Renew the session from the stored link
  auth logout                      Delete the local session

  profile get                      Print the current profile
  profile update [flags]           Update profile fields

  trips list                       List all trips
  trips add [flags]                Add one trip
  trips update --id <id> [flags]   Change an existing trip
  trips delete --id <id>           Delete a trip
  trips sync <file> [flags]        Reconcile trips with a desired-state file

Global flags:
  --json      Emit JSON instead of text
  --debug     Verbose, redacted request logging
  --config    Override the config directory

Run 'nomads <command> --help' for the flags of a specific command.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		printError(err)
		os.Exit(1)
	}
}

// printError renders a semantic error with its diagnostics, never secrets.
func printError(err error) {
	var ne *nomads.Error
	if errors.As(err, &ne) {
		fmt.Fprintf(os.Stderr, "error [%s]: %s\n", ne.Code, ne.Message)
		for k, v := range ne.Details {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", k, v)
		}
		switch ne.Code {
		case nomads.ErrAuthExpired:
			fmt.Fprintln(os.Stderr, "\nTry: nomads auth refresh")
		case nomads.ErrAPIChanged:
			fmt.Fprintln(os.Stderr, "\nThe Nomads.com frontend may have changed.")
			fmt.Fprintln(os.Stderr, "See docs/api-observations.md for how to recapture the API.")
		}
		return
	}
	fmt.Fprintf(os.Stderr, "error: %s\n", auth.Redact(err.Error()))
}

// globals holds flags shared by every command.
type globals struct {
	jsonOut bool
	debug   bool
	config  string
}

func (g *globals) bind(fs *flag.FlagSet) {
	fs.BoolVar(&g.jsonOut, "json", false, "emit JSON instead of text")
	fs.BoolVar(&g.debug, "debug", false, "verbose, redacted request logging")
	fs.StringVar(&g.config, "config", "", "config directory (default ~/.config/nomads-agent)")
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "auth":
		return runAuth(ctx, rest)
	case "profile":
		return runProfile(ctx, rest)
	case "trips":
		return runTrips(ctx, rest)
	case "version":
		fmt.Println("nomads-agent 0.1.0")
		return nil
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// openStore builds the state store.
func openStore(dir string) (*storage.Store, error) { return storage.New(dir) }

// newClient builds a hybrid client from whatever credentials are configured.
//
// The documented API (key auth) is preferred wherever it can do the job; the
// first-party session fills in the operations it does not cover.
func newClient(g globals) (*client.Hybrid, error) {
	store, err := openStore(g.config)
	if err != nil {
		return nil, err
	}
	sess, err := store.LoadSession()
	if err != nil {
		return nil, err
	}
	// Environment variables win, so a key never has to be written to disk.
	username := firstNonEmpty(os.Getenv("NOMADS_USERNAME"), sess.Username)
	apiKey := firstNonEmpty(os.Getenv("NOMADS_API_KEY"), sess.APIKey)

	var official *client.Official
	if username != "" && apiKey != "" {
		official, err = client.NewOfficial(username, apiKey, client.WithOfficialLogger(debugLogger(g.debug)))
		if err != nil {
			return nil, err
		}
	}

	var private *client.Client
	if sess.HasBrowserSession() {
		private, err = client.New(sess, client.WithDebug(g.debug))
		if err != nil {
			return nil, err
		}
		if username == "" {
			u, err := private.WhoAmI(context.Background())
			if err == nil {
				username = u
				sess.Username = u
				_ = store.SaveSession(sess)
			}
		}
		private.SetUsername(username)
	}

	if official == nil && private == nil {
		return nil, nomads.Errorf(nomads.ErrAuthExpired,
			"no credentials configured. Run 'nomads auth key --username <handle> --key <key from https://nomads.com/settings)', "+
				"and/or 'nomads auth login --link <magic link>' for profile and trip editing.")
	}
	return client.NewHybrid(official, private)
}

// debugLogger builds the structured logger used when --debug is set.
func debugLogger(debug bool) *slog.Logger {
	if !debug {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// firstNonEmpty returns the first non-blank string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func runAuth(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("auth needs a subcommand: key, login, import, status, refresh, logout")
	}
	var g globals
	sub, rest := args[0], args[1:]

	switch sub {
	case "login":
		fs := flag.NewFlagSet("auth login", flag.ContinueOnError)
		g.bind(fs)
		link := fs.String("link", "", "magic login link or bare hash (required)")
		username := fs.String("username", "", "your Nomads.com handle, e.g. yourname")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *link == "" {
			return nomads.Errorf(nomads.ErrInvalidInput,
				"--link is required; use the 'log in' link Nomads.com emailed you")
		}
		store, err := openStore(g.config)
		if err != nil {
			return err
		}
		sess, err := auth.New(store).Login(ctx, *link)
		if err != nil {
			return err
		}
		if *username != "" {
			sess.Username = strings.TrimPrefix(*username, "@")
			if err := store.SaveSession(sess); err != nil {
				return err
			}
		}
		fmt.Printf("Logged in. Session stored at %s\n", store.Path())
		if sess.Username == "" {
			fmt.Println("Tip: pass --username <handle> so profile and trip commands know which profile to read.")
		}
		return nil

	case "key":
		fs := flag.NewFlagSet("auth key", flag.ContinueOnError)
		g.bind(fs)
		username := fs.String("username", "", "your Nomads.com handle, e.g. yourname (required)")
		key := fs.String("key", "", "personal key from https://nomads.com/settings (required)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *key == "" {
			return nomads.Errorf(nomads.ErrInvalidInput,
				"--key is required; copy it from https://nomads.com/settings")
		}
		store, err := openStore(g.config)
		if err != nil {
			return err
		}
		sess, err := auth.New(store).SaveAPIKey(*username, *key)
		if err != nil {
			return err
		}
		// Prove the key works before claiming success.
		o, err := client.NewOfficial(sess.Username, sess.APIKey)
		if err != nil {
			return err
		}
		if _, err := o.ListTrips(ctx); err != nil {
			return err
		}
		fmt.Printf("API key verified for @%s and stored at %s\n", sess.Username, store.Path())
		return nil

	case "import":
		fs := flag.NewFlagSet("auth import", flag.ContinueOnError)
		g.bind(fs)
		cookies := fs.String("cookies", "", "cookie string from the browser (document.cookie)")
		jsonFile := fs.String("file", "", "path to a JSON cookie export")
		stdin := fs.Bool("stdin", false, "read the cookie string from stdin")
		username := fs.String("username", "", "your Nomads.com handle (auto-detected if omitted)")
		if err := fs.Parse(rest); err != nil {
			return err
		}

		var (
			imported []storage.Cookie
			err      error
		)
		switch {
		case *stdin:
			raw, rerr := io.ReadAll(os.Stdin)
			if rerr != nil {
				return rerr
			}
			imported, err = browser.ParseCookieHeader(string(raw))
		case *jsonFile != "":
			raw, rerr := os.ReadFile(*jsonFile)
			if rerr != nil {
				return rerr
			}
			imported, err = browser.ParseCookieJSON(raw)
		case *cookies != "":
			imported, err = browser.ParseCookieHeader(*cookies)
		default:
			fmt.Println(browser.Instructions())
			return nil
		}
		if err != nil {
			return err
		}

		store, err := openStore(g.config)
		if err != nil {
			return err
		}
		sess, err := store.LoadSession()
		if err != nil {
			return err
		}
		sess.Cookies = imported
		if u := strings.TrimPrefix(strings.TrimSpace(*username), "@"); u != "" {
			sess.Username = u
		}

		// Prove the session works, and learn the handle, before storing it.
		probe, err := client.New(sess, client.WithDebug(g.debug))
		if err != nil {
			return err
		}
		if sess.Username == "" {
			u, werr := probe.WhoAmI(ctx)
			if werr != nil {
				return nomads.Errorf(nomads.ErrAuthExpired,
					"imported cookies did not authenticate; make sure you are logged in "+
						"to Nomads.com in that browser and copied the whole cookie string")
			}
			sess.Username = u
		}
		probe.SetUsername(sess.Username)
		if _, err := probe.GetProfile(ctx); err != nil {
			return err
		}
		if err := store.SaveSession(sess); err != nil {
			return err
		}
		fmt.Printf("Session imported and verified for @%s (%d cookies).\n", sess.Username, len(imported))
		fmt.Printf("Stored at %s\n", store.Path())
		return nil

	case "status":
		fs := flag.NewFlagSet("auth status", flag.ContinueOnError)
		g.bind(fs)
		if err := fs.Parse(rest); err != nil {
			return err
		}
		store, err := openStore(g.config)
		if err != nil {
			return err
		}
		sess, err := store.LoadSession()
		if err != nil {
			return err
		}
		if sess.IsEmpty() {
			fmt.Println("No credentials configured.")
			fmt.Println("  nomads auth key   --username <handle> --key <key from https://nomads.com/settings>")
			fmt.Println("  nomads auth login --link <magic link>   (for profile and trip editing)")
			return nil
		}

		username := firstNonEmpty(os.Getenv("NOMADS_USERNAME"), sess.Username)
		status := map[string]any{
			"username":        username,
			"api_key":         sess.HasAPIKey() || os.Getenv("NOMADS_API_KEY") != "",
			"browser_session": sess.HasBrowserSession(),
			"created_at":      sess.CreatedAt,
			"state_file":      store.Path(),
		}

		// Probe each configured backend with a real read.
		if h, err := newClient(g); err == nil {
			caps := h.Capabilities()
			status["capabilities"] = caps
			if _, err := h.ListTrips(ctx); err != nil {
				status["trips_read"] = "failed: " + string(nomads.CodeOf(err))
			} else {
				status["trips_read"] = "ok"
			}
			if sess.HasBrowserSession() {
				if _, err := h.GetProfile(ctx); err != nil {
					status["profile_read"] = "failed: " + string(nomads.CodeOf(err))
				} else {
					status["profile_read"] = "ok"
				}
			}
		} else {
			status["error"] = err.Error()
		}

		if g.jsonOut {
			return emitJSON(status)
		}
		fmt.Printf("Username:        %s\n", orDash(username))
		fmt.Printf("API key:         %s\n", yesNo(status["api_key"] == true))
		fmt.Printf("Browser session: %s\n", yesNo(sess.HasBrowserSession()))
		fmt.Printf("Trips read:      %v\n", orDashAny(status["trips_read"]))
		if v, ok := status["profile_read"]; ok {
			fmt.Printf("Profile read:    %v\n", v)
		}
		fmt.Printf("State file:      %s\n", store.Path())
		if caps, ok := status["capabilities"].(client.Capabilities); ok {
			fmt.Println("\nCapabilities:")
			fmt.Printf("  list/add trip:        %s\n", caps.ListTrips)
			fmt.Printf("  update/delete trip:   %s\n", caps.UpdateTrip)
			fmt.Printf("  read/update profile:  %s\n", caps.GetProfile)
		}
		if e, ok := status["error"]; ok {
			fmt.Printf("\nNote: %v\n", e)
		}
		return nil

	case "refresh":
		fs := flag.NewFlagSet("auth refresh", flag.ContinueOnError)
		g.bind(fs)
		if err := fs.Parse(rest); err != nil {
			return err
		}
		store, err := openStore(g.config)
		if err != nil {
			return err
		}
		if _, err := auth.New(store).Refresh(ctx); err != nil {
			return err
		}
		fmt.Println("Session refreshed.")
		return nil

	case "logout":
		fs := flag.NewFlagSet("auth logout", flag.ContinueOnError)
		g.bind(fs)
		if err := fs.Parse(rest); err != nil {
			return err
		}
		store, err := openStore(g.config)
		if err != nil {
			return err
		}
		if err := auth.New(store).Logout(); err != nil {
			return err
		}
		fmt.Println("Logged out; local session deleted.")
		return nil
	}
	return fmt.Errorf("unknown auth subcommand %q", sub)
}

func runProfile(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("profile needs a subcommand: get, update")
	}
	sub, rest := args[0], args[1:]
	var g globals

	switch sub {
	case "get":
		fs := flag.NewFlagSet("profile get", flag.ContinueOnError)
		g.bind(fs)
		if err := fs.Parse(rest); err != nil {
			return err
		}
		c, err := newClient(g)
		if err != nil {
			return err
		}
		p, err := c.GetProfile(ctx)
		if err != nil {
			return err
		}
		if g.jsonOut {
			return emitJSON(p)
		}
		fmt.Printf("Username:  @%s\n", p.Username)
		fmt.Printf("Bio:       %s\n", orDash(p.Bio))
		fmt.Printf("Website:   %s\n", orDash(p.Website))
		fmt.Printf("Twitter:   %s\n", orDash(p.Twitter))
		fmt.Printf("Instagram: %s\n", orDash(p.Instagram))
		fmt.Printf("YouTube:   %s\n", orDash(p.YouTube))
		fmt.Printf("TikTok:    %s\n", orDash(p.TikTok))
		fmt.Printf("Tags:      %s\n", orDash(strings.Join(p.Tags, ", ")))
		return nil

	case "update":
		fs := flag.NewFlagSet("profile update", flag.ContinueOnError)
		g.bind(fs)
		bio := fs.String("bio", "", "profile bio")
		website := fs.String("website", "", "website URL")
		twitter := fs.String("twitter", "", "Twitter/X handle")
		instagram := fs.String("instagram", "", "Instagram handle")
		youtube := fs.String("youtube", "", "YouTube URL")
		tiktok := fs.String("tiktok", "", "TikTok handle")
		tags := fs.String("tags", "", "comma-separated tags (replaces the whole set)")
		if err := fs.Parse(rest); err != nil {
			return err
		}

		// Only flags the user actually typed become patch fields, so an unset
		// flag never overwrites a remote value with "".
		var patch nomads.ProfilePatch
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		if set["bio"] {
			patch.Bio = bio
		}
		if set["website"] {
			patch.Website = website
		}
		if set["twitter"] {
			patch.Twitter = twitter
		}
		if set["instagram"] {
			patch.Instagram = instagram
		}
		if set["youtube"] {
			patch.YouTube = youtube
		}
		if set["tiktok"] {
			patch.TikTok = tiktok
		}
		if set["tags"] {
			list := nomads.NormalizeTags(strings.Split(*tags, ","))
			patch.Tags = &list
		}
		if patch.IsEmpty() {
			return nomads.Errorf(nomads.ErrInvalidInput,
				"nothing to update; pass at least one of --bio, --website, --twitter, --instagram, --youtube, --tiktok, --tags")
		}

		c, err := newClient(g)
		if err != nil {
			return err
		}
		p, err := c.UpdateProfile(ctx, patch)
		if err != nil {
			return err
		}
		if g.jsonOut {
			return emitJSON(p)
		}
		fmt.Println("Profile updated and verified.")
		return nil
	}
	return fmt.Errorf("unknown profile subcommand %q", sub)
}

func runTrips(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("trips needs a subcommand: list, add, update, delete, sync")
	}
	sub, rest := args[0], args[1:]
	var g globals

	switch sub {
	case "list":
		fs := flag.NewFlagSet("trips list", flag.ContinueOnError)
		g.bind(fs)
		if err := fs.Parse(rest); err != nil {
			return err
		}
		c, err := newClient(g)
		if err != nil {
			return err
		}
		list, err := c.ListTrips(ctx)
		if err != nil {
			return err
		}
		if g.jsonOut {
			return emitJSON(list)
		}
		if len(list) == 0 {
			fmt.Println("No trips.")
			return nil
		}
		today := nomads.DateFromTime(nowLocal())
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "FROM\tTO\tCITY\tCOUNTRY\tSTATUS\tID")
		for _, t := range list {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				t.StartDate, t.EndDate, t.City, t.Country, t.StatusOn(today), shortID(t.ID))
		}
		return w.Flush()

	case "add":
		fs := flag.NewFlagSet("trips add", flag.ContinueOnError)
		g.bind(fs)
		city := fs.String("city", "", "city name (required)")
		country := fs.String("country", "", "country name")
		from := fs.String("from", "", "start date YYYY-MM-DD (required)")
		to := fs.String("to", "", "end date YYYY-MM-DD (required)")
		note := fs.String("note", "", "optional note")
		lat := fs.Float64("lat", 0, "latitude (skips geocoding)")
		lon := fs.Float64("lon", 0, "longitude (skips geocoding)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		start, end, err := parseRange(*from, *to)
		if err != nil {
			return err
		}
		c, err := newClient(g)
		if err != nil {
			return err
		}
		trip, err := c.CreateTrip(ctx, nomads.CreateTripRequest{
			City: *city, Country: *country, StartDate: start, EndDate: end,
			Note: *note, Latitude: *lat, Longitude: *lon,
		})
		if err != nil {
			return err
		}
		if err := c.VerifyTrip(ctx, *trip); err != nil {
			return err
		}
		if g.jsonOut {
			return emitJSON(trip)
		}
		fmt.Printf("Added and verified: %s\n", trip.Label())
		return nil

	case "update":
		fs := flag.NewFlagSet("trips update", flag.ContinueOnError)
		g.bind(fs)
		id := fs.String("id", "", "trip id (required)")
		city := fs.String("city", "", "new city")
		country := fs.String("country", "", "new country")
		from := fs.String("from", "", "new start date YYYY-MM-DD")
		to := fs.String("to", "", "new end date YYYY-MM-DD")
		note := fs.String("note", "", "new note")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *id == "" {
			return nomads.Errorf(nomads.ErrInvalidInput, "--id is required")
		}
		var patch nomads.TripPatch
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		if set["city"] {
			patch.City = city
		}
		if set["country"] {
			patch.Country = country
		}
		if set["note"] {
			patch.Note = note
		}
		if set["from"] {
			d, err := nomads.ParseDate(*from)
			if err != nil {
				return nomads.Wrap(err, nomads.ErrInvalidInput, "--from: %s", err)
			}
			patch.StartDate = &d
		}
		if set["to"] {
			d, err := nomads.ParseDate(*to)
			if err != nil {
				return nomads.Wrap(err, nomads.ErrInvalidInput, "--to: %s", err)
			}
			patch.EndDate = &d
		}
		if patch.IsEmpty() {
			return nomads.Errorf(nomads.ErrInvalidInput, "nothing to update")
		}
		c, err := newClient(g)
		if err != nil {
			return err
		}
		trip, err := c.UpdateTrip(ctx, *id, patch)
		if err != nil {
			return err
		}
		if err := c.VerifyTrip(ctx, *trip); err != nil {
			return err
		}
		if g.jsonOut {
			return emitJSON(trip)
		}
		fmt.Printf("Updated and verified: %s\n", trip.Label())
		fmt.Printf("Note: Nomads.com assigns a new id on edit; this trip is now %s\n", shortID(trip.ID))
		return nil

	case "delete":
		fs := flag.NewFlagSet("trips delete", flag.ContinueOnError)
		g.bind(fs)
		id := fs.String("id", "", "trip id (required)")
		yes := fs.Bool("yes", false, "skip the confirmation prompt")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *id == "" {
			return nomads.Errorf(nomads.ErrInvalidInput, "--id is required")
		}
		c, err := newClient(g)
		if err != nil {
			return err
		}
		if !*yes {
			list, err := c.ListTrips(ctx)
			if err != nil {
				return err
			}
			var label string
			for _, t := range list {
				if t.ID == *id {
					label = t.Label()
				}
			}
			if label == "" {
				return nomads.Errorf(nomads.ErrTripNotFound, "no trip with id %s", *id)
			}
			fmt.Printf("Delete %s? [y/N] ", label)
			var answer string
			_, _ = fmt.Scanln(&answer)
			if !strings.EqualFold(strings.TrimSpace(answer), "y") {
				fmt.Println("Cancelled.")
				return nil
			}
		}
		if err := c.DeleteTrip(ctx, *id); err != nil {
			return err
		}
		fmt.Println("Deleted and verified.")
		return nil

	case "sync":
		fs := flag.NewFlagSet("trips sync", flag.ContinueOnError)
		g.bind(fs)
		dryRun := fs.Bool("dry-run", false, "print the plan without changing anything")
		deleteMissing := fs.Bool("delete-missing", false,
			"delete remote trips absent from the file (destructive)")
		horizon := fs.String("delete-after", "",
			"with --delete-missing, only delete trips starting on or after this date")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() < 1 {
			return nomads.Errorf(nomads.ErrInvalidInput, "usage: nomads trips sync <file> [flags]")
		}
		desired, err := trips.Load(fs.Arg(0))
		if err != nil {
			return err
		}
		if err := sync.ValidateDesired(desired); err != nil {
			return err
		}

		opts := sync.Options{DeleteMissing: *deleteMissing}
		if *horizon != "" {
			d, err := nomads.ParseDate(*horizon)
			if err != nil {
				return nomads.Wrap(err, nomads.ErrInvalidInput, "--delete-after: %s", err)
			}
			opts.DeleteHorizon = &d
		}

		c, err := newClient(g)
		if err != nil {
			return err
		}
		current, err := c.ListTrips(ctx)
		if err != nil {
			return err
		}
		plan := sync.PlanTripSync(current, desired, opts)

		if *dryRun {
			if g.jsonOut {
				return emitJSON(plan)
			}
			fmt.Print(sync.Format(plan))
			fmt.Printf("\n%d change(s) would be applied. Re-run without --dry-run to apply.\n",
				plan.MutationCount())
			return nil
		}

		if !g.jsonOut {
			fmt.Print(sync.Format(plan))
		}
		if plan.IsNoop() {
			if g.jsonOut {
				return emitJSON(sync.Result{})
			}
			fmt.Println("\nAlready in sync; nothing to do.")
			return nil
		}

		res, err := sync.Apply(ctx, c, plan)
		if err != nil {
			return err
		}
		var verifyErr error
		if !res.HasErrors() {
			verifyErr = sync.Verify(ctx, c, desired)
		}
		if g.jsonOut {
			if err := emitJSON(res); err != nil {
				return err
			}
			return verifyErr
		}
		fmt.Printf("\nApplied: %d created, %d updated, %d deleted.\n",
			len(res.Created), len(res.Updated), len(res.Deleted))
		for _, e := range res.Errors {
			fmt.Fprintf(os.Stderr, "  %s failed [%s]: %s (%s)\n", e.Action, e.Code, e.Target, e.Error)
		}
		if verifyErr == nil && !res.HasErrors() {
			fmt.Println("Verified: remote state matches the desired trips.")
		}
		if res.HasErrors() {
			return fmt.Errorf("%d action(s) failed", len(res.Errors))
		}
		return verifyErr
	}
	return fmt.Errorf("unknown trips subcommand %q", sub)
}

func parseRange(from, to string) (nomads.Date, nomads.Date, error) {
	if from == "" || to == "" {
		return nomads.Date{}, nomads.Date{}, nomads.Errorf(nomads.ErrInvalidInput,
			"--from and --to are required (YYYY-MM-DD)")
	}
	start, err := nomads.ParseDate(from)
	if err != nil {
		return nomads.Date{}, nomads.Date{}, nomads.Wrap(err, nomads.ErrInvalidInput, "--from: %s", err)
	}
	end, err := nomads.ParseDate(to)
	if err != nil {
		return nomads.Date{}, nomads.Date{}, nomads.Wrap(err, nomads.ErrInvalidInput, "--to: %s", err)
	}
	return start, end, nil
}

func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func yesNo(b bool) string {
	if b {
		return "configured"
	}
	return "not configured"
}

func orDashAny(v any) any {
	if v == nil {
		return "-"
	}
	return v
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// shortID abbreviates the long hex trip id for display.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12] + "..."
	}
	return id
}
