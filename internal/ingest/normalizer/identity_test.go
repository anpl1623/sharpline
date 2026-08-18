package normalizer

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// identityProbe is one derivation of every identifier this package produces.
//
// It is a struct rather than a list of assertions because the cross-process test
// below compares a whole probe from a CHILD PROCESS against one derived here,
// and a single JSON document is what crosses that boundary.
type identityProbe struct {
	Sport     string `json:"sport"`
	League    string `json:"league"`
	Book      string `json:"book"`
	Event     string `json:"event"`
	Market    string `json:"market"`
	PlayerMkt string `json:"player_market"`
	Selection string `json:"selection"`
	Outright  string `json:"outright"`
	SportSlug string `json:"sport_slug"`
	BookSlug  string `json:"book_slug"`
}

// deriveProbe runs every exported derivation over one fixed input set.
func deriveProbe(t *testing.T) identityProbe {
	t.Helper()
	const (
		prov      = kafka.Provider("the-odds-api")
		sportKey  = "americanfootball"
		leagueKey = "americanfootball_nfl"
		bookKey   = "draftkings"
		eventKey  = "42db668449664943833b5c04a583328a"
		subject   = "Jared Goff"
		runner    = "Detroit Lions"
	)

	sport, err := SportIDFor(prov, sportKey)
	must(t, err)
	league, err := LeagueIDFor(prov, leagueKey)
	must(t, err)
	book, err := BookIDFor(prov, bookKey)
	must(t, err)
	event, err := EventIDFor(prov, eventKey)
	must(t, err)
	market, err := MarketIDFor(event, MarketKeySpreads, "")
	must(t, err)
	player, err := MarketIDFor(event, "player_pass_tds", subject)
	must(t, err)
	selection, err := SelectionIDFor(market, domain.SelectionRoleHome, "Kansas City Chiefs")
	must(t, err)
	futures, err := MarketIDFor(event, MarketKeyOutrights, "")
	must(t, err)
	outright, err := SelectionIDFor(futures, domain.SelectionRoleOutright, runner)
	must(t, err)
	sportSlug, err := SlugFor("", sportKey)
	must(t, err)
	bookSlug, err := SlugFor("", bookKey)
	must(t, err)

	return identityProbe{
		Sport: sport.String(), League: league.String(), Book: book.String(),
		Event: event.String(), Market: market.String(), PlayerMkt: player.String(),
		Selection: selection.String(), Outright: outright.String(),
		SportSlug: sportSlug.String(), BookSlug: bookSlug.String(),
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestIdentifiersAreDeterministicWithinAProcess is the cheap half of the
// guarantee: the same inputs derive the same identifiers, repeatedly.
func TestIdentifiersAreDeterministicWithinAProcess(t *testing.T) {
	want := deriveProbe(t)
	for i := 0; i < 32; i++ {
		if got := deriveProbe(t); got != want {
			t.Fatalf("derivation %d differed:\n got %+v\nwant %+v", i, got, want)
		}
	}
}

// childEnv marks the re-executed test binary that stands in for a restart.
const childEnv = "SHARPLINE_IDENTITY_CHILD"

// TestIdentifiersSurviveARestart is the expensive half, and it is the one that
// matters.
//
// odds.normalized is COMPACTED and keyed by market_id. The broker keeps the
// latest record per key, which is what makes the topic a replayable snapshot of
// every current line — and that property is worth exactly as much as the
// stability of the key. An identifier that differed between two RUNS of the same
// binary would make compaction stop collapsing and start accumulating, silently:
// the topic still serves records, there are just now two for one market and one
// of them is frozen for ever.
//
// A same-process loop cannot catch that class. Go randomises its map hash seed
// and its map iteration order PER PROCESS, so a derivation that ranged a map, or
// that mixed a runtime hash into a component, would be perfectly stable within
// one run and different in the next. This test re-executes the test binary and
// compares the child's derivation against the parent's, which is the only way to
// see it.
func TestIdentifiersSurviveARestart(t *testing.T) {
	if payload := os.Getenv(childEnv); payload == "1" {
		// The child half: derive, print, exit. The parent reads stdout.
		enc, err := json.Marshal(deriveProbe(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Log(childMarker + string(enc))
		return
	}

	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary to re-execute: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=^TestIdentifiersSurviveARestart$", "-test.v")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process failed: %v\n%s", err, out)
	}

	line := ""
	for _, l := range strings.Split(string(out), "\n") {
		if i := strings.Index(l, childMarker); i >= 0 {
			line = strings.TrimSpace(l[i+len(childMarker):])
		}
	}
	if line == "" {
		t.Fatalf("child produced no probe:\n%s", out)
	}

	var child identityProbe
	if err := json.Unmarshal([]byte(line), &child); err != nil {
		t.Fatalf("decoding the child's probe %q: %v", line, err)
	}
	if want := deriveProbe(t); child != want {
		t.Fatalf("identifiers differ across processes — compaction on odds.normalized would "+
			"accumulate a dead key per restart:\nchild  %+v\nparent %+v", child, want)
	}
}

// childMarker prefixes the child's JSON so the parent can find it in test output.
const childMarker = "identity-probe:"

// TestMarketIdentityExcludesTheLine is the decision MarketIDFor documents, and
// it is load-bearing in both directions.
//
// A market whose line moves from -3.5 to -4 is the SAME market. Folding the line
// into the key would issue a new identifier on every move, which shatters the
// line-movement series CLAUDE.md §6 charts and §9's CLV is computed from, and
// leaves one orphaned compacted key behind per move.
func TestMarketIdentityExcludesTheLine(t *testing.T) {
	event, err := EventIDFor("synthetic", "evt-1")
	must(t, err)

	a, err := MarketIDFor(event, MarketKeySpreads, "")
	must(t, err)
	b, err := MarketIDFor(event, MarketKeySpreads, "")
	must(t, err)
	if a != b {
		t.Fatalf("MarketIDFor is not a function: %s != %s", a, b)
	}

	// Two provider keys that map to ONE domain type must NOT collapse: they are
	// genuinely different markets, and on a compacted topic collapsing them means
	// they overwrite each other for ever.
	pass, err := MarketIDFor(event, "player_pass_tds", "Jared Goff")
	must(t, err)
	rush, err := MarketIDFor(event, "player_rush_yds", "Jared Goff")
	must(t, err)
	if pass == rush {
		t.Fatal("two provider market keys collapsed onto one identifier")
	}

	// Two subjects under one key are two markets, not two selections.
	goff, err := MarketIDFor(event, "player_pass_tds", "Jared Goff")
	must(t, err)
	blough, err := MarketIDFor(event, "player_pass_tds", "David Blough")
	must(t, err)
	if goff == blough {
		t.Fatal("two player-prop subjects collapsed onto one identifier")
	}
}

// TestIdentifiersFitTheDomainBudget checks the arithmetic identity.go's file
// comment states: the LONGEST possible selection identifier must still fit
// domain.MaxIDLen, because identifiers nest.
func TestIdentifiersFitTheDomainBudget(t *testing.T) {
	prov := kafka.Provider(strings.Repeat("a", MaxProviderLen))
	if err := ValidateProviderForIdentity(prov); err != nil {
		t.Fatalf("a provider at exactly the budget was rejected: %v", err)
	}
	if err := ValidateProviderForIdentity(kafka.Provider(strings.Repeat("a", MaxProviderLen+1))); err == nil {
		t.Fatal("a provider over the budget was accepted; identifiers would silently exceed MaxIDLen")
	}

	long := strings.Repeat("z", 512)
	event, err := EventIDFor(prov, long)
	must(t, err)
	market, err := MarketIDFor(event, long, long)
	must(t, err)
	selection, err := SelectionIDFor(market, domain.SelectionRoleOutright, long)
	must(t, err)

	if got := len(selection.String()); got > domain.MaxIDLen {
		t.Fatalf("worst-case selection identifier is %d bytes, domain.MaxIDLen is %d: %s",
			got, domain.MaxIDLen, selection)
	}
}

// TestSlugNamespaceSeparatesProviders pins the collision SlugFor exists to
// prevent: leagues.slug and books.slug are UNIQUE GLOBALLY in the schema, so two
// providers using one key for one real-world league derive the same slug and
// different identifiers, and the second write violates the constraint.
func TestSlugNamespaceSeparatesProviders(t *testing.T) {
	plain, err := SlugFor("", "americanfootball_nfl")
	must(t, err)
	spaced, err := SlugFor("syn", "americanfootball_nfl")
	must(t, err)

	if plain == spaced {
		t.Fatal("the namespace did not change the slug")
	}
	if !strings.HasPrefix(spaced.String(), "syn-") {
		t.Fatalf("namespaced slug %q does not carry the namespace", spaced)
	}
	if _, err := domain.NewSlug(spaced.String()); err != nil {
		t.Fatalf("namespaced slug %q is not a valid domain slug: %v", spaced, err)
	}

	// A key that folds away to nothing becomes its hash rather than an error: a
	// slug is a display and routing concern, and failing an entire league over an
	// unfortunate name would trade a cosmetic problem for a data-loss one.
	odd, err := SlugFor("", "！！！")
	must(t, err)
	if odd.String() == "" {
		t.Fatal("a non-ASCII key produced an empty slug")
	}
	if _, err := domain.NewSlug(odd.String()); err != nil {
		t.Fatalf("folded slug %q is not a valid domain slug: %v", odd, err)
	}
}

// TestSportKeyFromLeagueKey pins the provider's own grouping convention.
func TestSportKeyFromLeagueKey(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"americanfootball_nfl", "americanfootball"},
		{"basketball_nba", "basketball"},
		{"icehockey_nhl", "icehockey"},
		{"boxing", "boxing"},
		{"_leading", "_leading"},
		{"", ""},
	} {
		if got := SportKeyFromLeagueKey(tc.in); got != tc.want {
			t.Errorf("SportKeyFromLeagueKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
