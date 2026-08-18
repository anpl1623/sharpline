package synthetic

import (
	"fmt"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// The simulated universe: four invented leagues, forty invented competitors,
// twenty invented players, five simulated books.
//
// # Every name here is fictional, on purpose
//
// Not one league, club, player or book below exists. That is a requirement, not
// a stylistic choice. The contract ledger forbids "fake team names invented at
// the component layer" for surfaces that pretend to show real data, and the
// inverse obligation applies here: a generator that emitted real club names
// would produce output indistinguishable at a glance from a live feed, and a
// screenshot of it would be a claim about a real market that nobody made.
// Invented names make the simulation self-identifying wherever it surfaces — in
// the board, in a Kafka record, in a Grafana panel, in a bug report.
//
// The league SHAPES are real, because the point of the exercise is that the
// pricing pipeline meets realistic inputs: four periods and a ~228-point total
// for the basketball league, three periods and a ~6-goal total for the ice
// league, a three-way moneyline with a draw for the football league (the only
// one of the four whose moneyline admits domain.SelectionRoleDraw), and result
// dispersions that put spread prices near even money and moneylines across the
// realistic range. The numbers below are ordinary published dispersions for
// those sports; they are parameters of a simulation, not claims about any
// season.

// leagueDef is one invented league and the statistical shape of its contests.
type leagueDef struct {
	// key is the short stable identifier that composes into league, event,
	// market and selection identifiers.
	key string

	name      string
	sportSlug string
	sportName string

	// threeWay makes the moneyline admit a draw. Only the football league.
	threeWay bool

	// periods is the number of scoring periods, for the in-play clock.
	periods int

	// resultSD is the standard deviation of the final margin. It converts an
	// expected margin into P(home wins) and P(home covers).
	resultSD float64

	// totalMean and totalResultSD describe combined scoring. totalMean is where
	// the latent total process reverts to; totalResultSD is the dispersion of
	// the actual result around it.
	totalMean     float64
	totalResultSD float64

	// marginSpread is the standard deviation of the per-event expected margin
	// drawn at slate time: how unequal the fixtures are.
	marginSpread float64

	// marginLineSD and totalLineSD are the STATIONARY standard deviations of
	// the two latent processes: how far the line itself wanders over a day.
	// They are an order of magnitude below the result dispersions, which is
	// what makes a line move look like a line move rather than like noise.
	marginLineSD float64
	totalLineSD  float64

	// propStat names the player-prop quantity, and the three numbers describe
	// it the same way the total is described.
	propStat     string
	propMean     float64
	propResultSD float64
	propLineSD   float64

	// roster is the league's competitors. Ten, so a slate can pair them without
	// repeating a fixture within a slot.
	roster []string
}

// leagues is the invented league set.
//
// It is a function rather than a package-level slice because CLAUDE.md §12
// forbids global mutable state, and an exported or unexported slice var is
// mutable however carefully it is treated.
func leagues() []leagueDef {
	return []leagueDef{
		{
			key:           "sba",
			name:          "Synthetic Basketball Association",
			sportSlug:     "basketball",
			sportName:     "Basketball",
			periods:       4,
			resultSD:      11.5,
			totalMean:     228,
			totalResultSD: 20,
			marginSpread:  5.5,
			marginLineSD:  1.6,
			totalLineSD:   3.0,
			propStat:      "Points",
			propMean:      21.5,
			propResultSD:  6.4,
			propLineSD:    0.7,
			roster: []string{
				"Ashmill Lanterns", "Bexley Cross Quarrymen", "Calderwood Anvils",
				"Dunmoor Kestrels", "Ellisford Loomcats", "Farrowgate Beacons",
				"Greyhaven Nightjars", "Harrowfield Ironwrights", "Iverley Saltbacks",
				"Junsbury Cordwainers",
			},
		},
		{
			key:           "sgl",
			name:          "Synthetic Gridiron League",
			sportSlug:     "americanfootball",
			sportName:     "American Football",
			periods:       4,
			resultSD:      13.5,
			totalMean:     44.5,
			totalResultSD: 10.5,
			marginSpread:  6.0,
			marginLineSD:  1.1,
			totalLineSD:   1.6,
			propStat:      "Passing Yards",
			propMean:      244,
			propResultSD:  58,
			propLineSD:    6.0,
			roster: []string{
				"Kestrel Bay Halberds", "Larkspur Foxhounds", "Marrowden Pikemen",
				"Northwick Wardens", "Oakhurst Tinsmiths", "Pellingford Gannets",
				"Quarrytown Braziers", "Ridgeport Thistles", "Saltmarch Ferrymen",
				"Thornbury Wheelwrights",
			},
		},
		{
			key:           "sil",
			name:          "Synthetic Ice League",
			sportSlug:     "icehockey",
			sportName:     "Ice Hockey",
			periods:       3,
			resultSD:      2.25,
			totalMean:     6.1,
			totalResultSD: 2.3,
			marginSpread:  0.75,
			marginLineSD:  0.30,
			totalLineSD:   0.35,
			propStat:      "Shots on Goal",
			propMean:      3.1,
			propResultSD:  1.5,
			propLineSD:    0.2,
			roster: []string{
				"Ambervale Frostwrights", "Brackenmere Icebreakers", "Coldharbour Sparrows",
				"Dunlyn Glaciers", "Edenmoor Snowdrifters", "Fenwick Auroras",
				"Glasswater Puffins", "Hollowbeck Rooks", "Inglemoor Stormcaps",
				"Jarrowfield Sleetmen",
			},
		},
		{
			key:           "sfu",
			name:          "Synthetic Football Union",
			sportSlug:     "soccer",
			sportName:     "Football",
			threeWay:      true,
			periods:       2,
			resultSD:      1.40,
			totalMean:     2.72,
			totalResultSD: 1.55,
			marginSpread:  0.55,
			marginLineSD:  0.22,
			totalLineSD:   0.25,
			propStat:      "Shots",
			propMean:      2.4,
			propResultSD:  1.3,
			propLineSD:    0.18,
			roster: []string{
				"Alderbrook United", "Bellhaven Rovers", "Cranmere Athletic",
				"Dunwich Drifters", "Eastmarch City", "Fairholt Wanderers",
				"Greenshaw Town", "Havenport County", "Ironbridge Ironsides",
				"Josling Vale",
			},
		},
	}
}

// playerPool is the invented player set that player-prop markets name. Twenty
// names, drawn from without regard to league, because a prop's subject is a
// display string and nothing in the system resolves it to a roster.
func playerPool() []string {
	return []string{
		"Rowan Vestrey", "Milo Ashgrove", "Cass Underhill", "Teodor Bramble",
		"Niall Quintrell", "Devon Halloway", "Emrys Fairbank", "Idris Colemere",
		"Soren Whitlock", "Aurel Pendrick", "Bastien Marlowe", "Corin Ashby",
		"Dmitri Stanhope", "Eero Lindqvist", "Faris Ombeni", "Gideon Rooke",
		"Hakim Beaulieu", "Ivo Castellan", "Jonas Rearden", "Kirin Vashti",
	}
}

// bookDef is one simulated book and the three things that make it disagree with
// the others.
//
// CLAUDE.md §5 asks for book disagreement to be a property of the model. These
// are the three knobs it emerges from, and none of them is random noise added
// after the fact:
//
//   - lagSteps: the book quotes off the latent process as it was this many steps
//     ago. In quiet markets that is a small persistent difference; after a steam
//     move it is the staggered convergence phase 9's detector looks for.
//   - margin: the overround the book charges, applied by its own method.
//   - tickAmerican: the granularity it quotes at. A book on a 10-cent tick moves
//     its price a tenth as often as one on a 1-cent tick.
type bookDef struct {
	slug      string
	name      string
	reference bool

	// margin is the target overround: implied probabilities sum to 1 + margin
	// before tick flooring, which can only increase it.
	margin float64

	// power selects the power overround rather than the multiplicative one.
	// Mixed across the book set on purpose — see the package doc.
	power bool

	// lagSteps is how far behind the latent process this book's view is.
	lagSteps int

	// tickAmerican is the quoting granularity in American units.
	tickAmerican int64

	// biasSD is the standard deviation of this book's persistent per-event
	// opinion, in units of the league's expected margin.
	biasSD float64
}

// books returns the simulated book set.
//
// The in-house book uses domain.SyntheticBookSlug and is the sharp reference:
// tightest margin, finest tick, no lag, no bias. domain/book.go anticipates
// exactly this — marking a synthetic book as the reference "is permitted rather
// than rejected. Refusing it would leave the offline demo with no reference at
// all and the +EV surface permanently empty."
//
// The other four are simulated competitors, progressively softer. A +EV or
// arbitrage signal against them is a statement about this generator, which is
// why every one of them is domain.BookKindSynthetic.
func books() []bookDef {
	return []bookDef{
		{
			slug:         string(domain.SyntheticBookSlug),
			name:         "Sharpline Synthetic",
			reference:    true,
			margin:       0.020,
			power:        true,
			lagSteps:     0,
			tickAmerican: 1,
			biasSD:       0,
		},
		{
			slug:         "tallowcreek",
			name:         "Tallow Creek Book",
			margin:       0.038,
			lagSteps:     2,
			tickAmerican: 5,
			biasSD:       0.20,
		},
		{
			slug:         "ninepines",
			name:         "Nine Pines Wagers",
			margin:       0.045,
			power:        true,
			lagSteps:     4,
			tickAmerican: 5,
			biasSD:       0.30,
		},
		{
			slug:         "ashfall",
			name:         "Ashfall Book",
			margin:       0.055,
			lagSteps:     7,
			tickAmerican: 10,
			biasSD:       0.35,
		},
		{
			slug:         "lowtide",
			name:         "Lowtide Sportsbook",
			margin:       0.065,
			power:        true,
			lagSteps:     9,
			tickAmerican: 10,
			biasSD:       0.45,
		},
	}
}

// maxBookLag is the deepest lagged view any book takes, and therefore the length
// of history each latent process must retain.
func maxBookLag() int {
	m := 0
	for _, b := range books() {
		if b.lagSteps > m {
			m = b.lagSteps
		}
	}
	return m
}

// Identifier construction.
//
// Identifiers must be STABLE across polls for a given seed — the whole of change
// detection and of Kafka compaction depends on a market keeping its identity —
// and they must survive the slate rolling over at midnight. Both properties come
// from deriving every identifier from the event's OWN scheduled date rather than
// from the day the slate was built, and from indices rather than from anything
// clock-derived.

func (l leagueDef) leagueID() domain.LeagueID { return domain.LeagueID("syn-" + l.key) }
func (l leagueDef) leagueSlug() domain.Slug   { return domain.Slug("syn-" + l.key) }
func (l leagueDef) sportID() domain.SportID   { return domain.SportID(l.sportSlug) }

func (l leagueDef) competitorID(i int) domain.CompetitorID {
	return domain.CompetitorID(fmt.Sprintf("syn-%s-c%02d", l.key, i))
}

// matchEventID names a contest by its league, its scheduled date and its slot.
func (l leagueDef) matchEventID(start time.Time, slot int) domain.EventID {
	return domain.EventID(fmt.Sprintf("syn-%s-%s-%d", l.key, start.UTC().Format("20060102"), slot))
}

// futuresEventID names the league's season-title market. It carries the year so
// that a season rolls over into a genuinely new event rather than silently
// reusing the identity of the last one.
func (l leagueDef) futuresEventID(year int) domain.EventID {
	return domain.EventID(fmt.Sprintf("syn-%s-futures-%d", l.key, year))
}

func bookID(slug string) domain.BookID { return domain.BookID("syn-book-" + slug) }

// Market and selection identifiers. The suffixes are fixed strings so that a
// market's identity is a pure function of its event and its type.
const (
	marketSuffixMoneyline = "ml"
	marketSuffixSpread    = "sp"
	marketSuffixTotal     = "to"
	marketSuffixFutures   = "fu"
	marketSuffixProp      = "pp"
)

func marketID(event domain.EventID, suffix string) domain.MarketID {
	return domain.MarketID(string(event) + "-" + suffix)
}

func propMarketID(event domain.EventID, idx int) domain.MarketID {
	return domain.MarketID(fmt.Sprintf("%s-%s%d", event, marketSuffixProp, idx))
}

func selectionID(market domain.MarketID, suffix string) domain.SelectionID {
	return domain.SelectionID(string(market) + "-" + suffix)
}

func runnerSelectionID(market domain.MarketID, idx int) domain.SelectionID {
	return domain.SelectionID(fmt.Sprintf("%s-r%02d", market, idx))
}

// propDef is one player-prop market on one event.
type propDef struct {
	// idx is the market's ordinal on the event, and part of its identifier.
	idx int
	// subject is the display string a player-prop market must carry. The stat
	// is folded into it because domain.Market has no separate stat field —
	// market.go deliberately keeps the type set to CLAUDE.md §4's five names —
	// and a prop whose quantity is not stated anywhere cannot be graded.
	subject string
	// mean, resultSD and lineSD describe the quantity, exactly as the league's
	// total is described.
	mean     float64
	resultSD float64
	lineSD   float64
}

// slateEvent is one generated contest or competition, before any market is
// priced. It is a pure function of the league, the seed and the calendar.
type slateEvent struct {
	id     domain.EventID
	league leagueDef
	kind   domain.EventKind
	name   string

	home domain.Competitor
	away domain.Competitor

	start time.Time

	// marginMean and totalMean are the reversion targets of this event's two
	// latent processes: how good the fixture is and how high-scoring.
	marginMean float64
	totalMean  float64

	// props are the player-prop markets offered on this event. Empty unless the
	// contest is close to or in play; a book does not post props on a fixture
	// three days out.
	props []propDef

	// runners are the outright field, for a futures event only.
	runners []string
}

// isFutures reports whether the event is the league's season-title competition.
func (e slateEvent) isFutures() bool { return e.kind == domain.EventKindOutright }

// processCount is how many latent processes the event needs.
//
// A match carries the margin, the total, and one per prop. A futures event
// carries one strength process per runner.
func (e slateEvent) processCount() int {
	if e.isFutures() {
		return len(e.runners)
	}
	return processMatchProp + len(e.props)
}

// Latent process indices on a match event.
const (
	processMargin    = 0
	processTotal     = 1
	processMatchProp = 2
)

// dayStart truncates to UTC midnight. Truncate operates on absolute time and the
// Unix epoch begins at midnight UTC, so a 24-hour truncation lands on a UTC day
// boundary exactly.
func dayStart(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

// buildSlate returns every event the league currently offers.
//
// Days run from yesterday to opts.SlateDays-1 ahead. Yesterday is included so
// that a contest that started at 22:00 and is still in play at 00:30 does not
// vanish from the board when the calendar rolls; events that finished more than
// endedGrace ago are dropped, which is what retires them.
//
// The result is ordered, and the order is stable: day, then slot. Ordering
// matters because the raw payloads this produces are published to a
// partition-ordered topic and because a test that diffs two snapshots should be
// diffing prices, not permutations.
func (a *Adapter) buildSlate(l leagueDef, now time.Time) []slateEvent {
	today := dayStart(now)
	slots := a.opts.EventsPerLeaguePerDay
	spacing := time.Duration(int64(24*time.Hour) / int64(slots))
	offset := leagueOffset(l)

	events := make([]slateEvent, 0, slots*(a.opts.SlateDays+1)+1)
	for day := -1; day < a.opts.SlateDays; day++ {
		base := today.AddDate(0, 0, day)
		for slot := 0; slot < slots; slot++ {
			start := base.Add(time.Duration(slot)*spacing + offset)
			if now.Sub(start) > liveDuration+endedGrace {
				continue
			}
			events = append(events, a.buildMatch(l, start, slot, now))
		}
	}
	events = append(events, a.buildFutures(l, today))
	return events
}

// leagueOffset staggers each league's fixture grid.
//
// Without it every league would tip at the same instants and there would be
// stretches of the day with no live event anywhere — which would leave the
// scheduler's live window untested for hours at a time. The offsets are chosen
// so the leagues' between-fixture gaps do not overlap, so at least three of the
// four leagues have a contest in play at any instant.
func leagueOffset(l leagueDef) time.Duration {
	for i, def := range leagues() {
		if def.key == l.key {
			return time.Duration(i) * 45 * time.Minute
		}
	}
	return 0
}

// buildMatch generates one contest.
func (a *Adapter) buildMatch(l leagueDef, start time.Time, slot int, now time.Time) slateEvent {
	id := l.matchEventID(start, slot)
	key := string(id)

	homeIdx, awayIdx := pickPair(a.opts.Seed, "fixture:"+key, 0, len(l.roster))
	home, _ := domain.NewCompetitor(l.competitorID(homeIdx), l.roster[homeIdx])
	away, _ := domain.NewCompetitor(l.competitorID(awayIdx), l.roster[awayIdx])

	ev := slateEvent{
		id:         id,
		league:     l,
		kind:       domain.EventKindMatch,
		name:       l.roster[awayIdx] + " at " + l.roster[homeIdx],
		home:       home,
		away:       away,
		start:      start,
		marginMean: normalAt(a.opts.Seed, "margin-mean:"+key, 0) * l.marginSpread,
		totalMean:  l.totalMean + normalAt(a.opts.Seed, "total-mean:"+key, 0)*l.totalResultSD*0.25,
	}

	// Props are posted only on contests that are in play or close to it. A book
	// does not price a player prop three days out, and the scheduler's pregame
	// and futures windows should not be paying credits for markets that do not
	// exist.
	untilStart := start.Sub(now)
	if untilStart <= propPostingWindow && now.Sub(start) <= liveDuration {
		pool := playerPool()
		pa, pb := pickPair(a.opts.Seed, "players:"+key, 0, len(pool))
		for i, playerIdx := range []int{pa, pb} {
			ev.props = append(ev.props, propDef{
				idx:      i,
				subject:  fmt.Sprintf("%s (%s)", pool[playerIdx], l.propStat),
				mean:     l.propMean * (1 + normalAt(a.opts.Seed, fmt.Sprintf("prop-mean:%s:%d", key, i), 0)*0.18),
				resultSD: l.propResultSD,
				lineSD:   l.propLineSD,
			})
		}
	}
	return ev
}

// buildFutures generates the league's season-title competition.
//
// It is an EventKindOutright with no competitors, which is the shape
// domain/event.go describes: "a futures market ('2027 NBA Champion') hangs off
// something that is not a contest between two sides."
func (a *Adapter) buildFutures(l leagueDef, today time.Time) slateEvent {
	year := today.Year()
	id := l.futuresEventID(year)
	return slateEvent{
		id:      id,
		league:  l,
		kind:    domain.EventKindOutright,
		name:    fmt.Sprintf("%s %d Season Title", l.name, year),
		start:   today.AddDate(0, 0, futuresHorizonDays),
		runners: l.roster,
	}
}

// catalogue builds the sports, leagues and books this adapter serves.
func (a *Adapter) catalogue() (Catalogue, error) {
	var c Catalogue
	sports := map[string]bool{}
	for _, l := range leagues() {
		if !sports[l.sportSlug] {
			sports[l.sportSlug] = true
			s, err := domain.NewSport(domain.SportParams{
				ID:   l.sportID(),
				Slug: domain.Slug(l.sportSlug),
				Name: l.sportName,
			})
			if err != nil {
				return Catalogue{}, fmt.Errorf("synthetic sport %s: %w", l.sportSlug, err)
			}
			c.Sports = append(c.Sports, s)
		}
		league, err := domain.NewLeague(domain.LeagueParams{
			ID:      l.leagueID(),
			SportID: l.sportID(),
			Slug:    l.leagueSlug(),
			Name:    l.name,
		})
		if err != nil {
			return Catalogue{}, fmt.Errorf("synthetic league %s: %w", l.key, err)
		}
		c.Leagues = append(c.Leagues, league)
	}
	for _, b := range books() {
		var (
			book domain.Book
			err  error
		)
		if b.slug == string(domain.SyntheticBookSlug) {
			book, err = domain.NewSyntheticBook(bookID(b.slug), b.reference)
		} else {
			book, err = domain.NewBook(domain.BookParams{
				ID:        bookID(b.slug),
				Slug:      domain.Slug(b.slug),
				Name:      b.name,
				Kind:      domain.BookKindSynthetic,
				Reference: b.reference,
			})
		}
		if err != nil {
			return Catalogue{}, fmt.Errorf("synthetic book %s: %w", b.slug, err)
		}
		c.Books = append(c.Books, book)
	}
	return c, nil
}

// findLeague resolves a league identifier to its definition.
func findLeague(id domain.LeagueID) (leagueDef, bool) {
	for _, l := range leagues() {
		if l.leagueID() == id {
			return l, true
		}
	}
	return leagueDef{}, false
}
