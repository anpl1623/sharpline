package kafka

import (
	"errors"
	"strings"
	"testing"
)

// The topic registry is the Go-side half of a two-sided contract: the other half
// is deploy/terraform/modules/kafka-topics, and the two are joined by nothing but
// agreement. Nothing at compile time notices when a name here stops matching a
// name there, and the symptom of a mismatch is UNKNOWN_TOPIC_OR_PARTITION on the
// first produce of a service that has already declared itself ready — because the
// broker runs with auto-creation disabled on purpose (CLAUDE.md §9).
//
// So these tests are pedantic about literals. TestTopicNamesAreTheFrozenLiterals
// in particular exists to fail on a rename, loudly, next to a comment explaining
// why a rename is not a refactor.

// TestTopicNamesAreTheFrozenLiterals pins every declared name, plus the odds.raw.
// prefix, against the strings Terraform declares.
//
// Renaming one is a breaking change that orphans a compacted log: the old topic
// keeps every market's current line, the new one starts empty, and every client
// that resyncs afterwards sees an empty board. There is no migration for it short
// of a copy job. If this test fails, the change is not a refactor.
func TestTopicNamesAreTheFrozenLiterals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"odds.normalized", TopicOddsNormalized, "odds.normalized"},
		{"price.computed", TopicPriceComputed, "price.computed"},
		{"wager.events", TopicWagerEvents, "wager.events"},
		{"odds.raw. prefix", TopicOddsRawPrefix, "odds.raw."},
		// Phase 9. Three of these are named in CLAUDE.md §3's event-flow
		// diagram (`signals.steam | signals.arb | signals.clv`); signals.ev is
		// the documented addition. Same rule as the four above -- a rename here
		// is a breaking change, not a refactor, because a consumer group's
		// committed offsets are keyed by topic name.
		{"signals.ev", TopicSignalsEV, "signals.ev"},
		{"signals.arb", TopicSignalsArb, "signals.arb"},
		{"signals.steam", TopicSignalsSteam, "signals.steam"},
		{"signals.clv", TopicSignalsCLV, "signals.clv"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.got != tc.want {
				t.Fatalf("topic name = %q, want %q; renaming a topic orphans its compacted log", tc.got, tc.want)
			}
		})
	}
}

// TestRetentionStringAndValid checks that Retention prints Kafka's own
// cleanup.policy spelling and that only the two real postures validate.
//
// The spelling matters beyond cosmetics: an operator comparing a log line here
// against `--describe` output in kafka-ui is comparing two strings, and "compact"
// vs "compacted" is enough to make that comparison a manual translation step.
func TestRetentionStringAndValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		retention Retention
		wantStr   string
		wantValid bool
	}{
		{"delete", RetentionDelete, "delete", true},
		{"compact", RetentionCompact, "compact", true},
		{"the zero value is never valid", RetentionUnknown, "unknown", false},
		{"an out-of-range value", Retention(99), "unknown", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.retention.String(); got != tc.wantStr {
				t.Errorf("String() = %q, want %q", got, tc.wantStr)
			}
			if got := tc.retention.Valid(); got != tc.wantValid {
				t.Errorf("Valid() = %v, want %v", got, tc.wantValid)
			}
		})
	}
}

// TestKeyKindString checks the key-kind spelling, which appears in the
// ErrWrongKeyKind message a developer will read at 2am.
func TestKeyKindString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind KeyKind
		want string
	}{
		{"market", KeyKindMarketID, "market_id"},
		{"event", KeyKindEventID, "event_id"},
		{"wager", KeyKindWagerID, "wager_id"},
		{"the zero value", KeyKindUnknown, "unknown"},
		{"out of range", KeyKind(99), "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.kind.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRegistryTopics asserts each declared topic's three load-bearing
// properties: its name, whether it is compacted, and what it is keyed by.
//
// Every one of these is read at runtime to decide something irreversible.
// Compacted() authorises a tombstone. KeyKind() is what makes Delivery refuse to
// hand back the wrong sort of identifier. Getting either wrong is a silent
// failure, so they are asserted literally rather than derived.
func TestRegistryTopics(t *testing.T) {
	t.Parallel()

	oddsRaw, err := OddsRaw("synthetic")
	if err != nil {
		t.Fatalf("OddsRaw(synthetic): %v", err)
	}

	tests := []struct {
		name          string
		topic         Topic
		wantName      string
		wantRetention Retention
		wantCompacted bool
		wantKey       KeyKind
		wantString    string
	}{
		{
			// CLAUDE.md §3: "a compacted topic keyed by market_id IS the
			// current-line snapshot".
			name:          "odds.normalized is compacted and keyed by market",
			topic:         OddsNormalized(),
			wantName:      "odds.normalized",
			wantRetention: RetentionCompact,
			wantCompacted: true,
			wantKey:       KeyKindMarketID,
			wantString:    "odds.normalized[compact,key=market_id]",
		},
		{
			name:          "price.computed is compacted and keyed by market",
			topic:         PriceComputed(),
			wantName:      "price.computed",
			wantRetention: RetentionCompact,
			wantCompacted: true,
			wantKey:       KeyKindMarketID,
			wantString:    "price.computed[compact,key=market_id]",
		},
		{
			// NOT compacted. The whole value of an audit trail is that
			// superseded entries survive; compacting wager.events would delete
			// a wager's placement the moment its settlement was written.
			name:          "wager.events is retention-based and keyed by wager",
			topic:         WagerEvents(),
			wantName:      "wager.events",
			wantRetention: RetentionDelete,
			wantCompacted: false,
			wantKey:       KeyKindWagerID,
			wantString:    "wager.events[delete,key=wager_id]",
		},
		{
			// NOT compacted, and that is the decision the whole signals family
			// turns on. Compaction keeps the latest record per key, which only
			// means something when the latest record SUPERSEDES the earlier
			// ones. "The latest +EV finding for market X" supersedes nothing:
			// the previous finding is a different event that also happened.
			name:          "signals.ev is retention-based and keyed by market",
			topic:         SignalsEV(),
			wantName:      "signals.ev",
			wantRetention: RetentionDelete,
			wantCompacted: false,
			wantKey:       KeyKindMarketID,
			wantString:    "signals.ev[delete,key=market_id]",
		},
		{
			name:          "signals.arb is retention-based and keyed by market",
			topic:         SignalsArb(),
			wantName:      "signals.arb",
			wantRetention: RetentionDelete,
			wantCompacted: false,
			wantKey:       KeyKindMarketID,
			wantString:    "signals.arb[delete,key=market_id]",
		},
		{
			// CLAUDE.md §3: "hopping window over line-movement velocity, keyed
			// by market, across books." The bus key is the market even though
			// migration 00009 keys the TABLE by (market, selection, window),
			// because steam is directional -- the finer key belongs where the
			// row is stored, not where ordering is bought.
			name:          "signals.steam is retention-based and keyed by market",
			topic:         SignalsSteam(),
			wantName:      "signals.steam",
			wantRetention: RetentionDelete,
			wantCompacted: false,
			wantKey:       KeyKindMarketID,
			wantString:    "signals.steam[delete,key=market_id]",
		},
		{
			// The one signals topic keyed by WAGER. odds/clv.go: "the settle
			// service writes one per graded leg" -- a CLV record is a fact about
			// a wager, so keying it by wager_id co-partitions it with
			// wager.events and keeps a wager's placement, settlement and CLV
			// ordered against each other.
			name:          "signals.clv is retention-based and keyed by wager",
			topic:         SignalsCLV(),
			wantName:      "signals.clv",
			wantRetention: RetentionDelete,
			wantCompacted: false,
			wantKey:       KeyKindWagerID,
			wantString:    "signals.clv[delete,key=wager_id]",
		},
		{
			// Keyed by EVENT, not market — the provider returns one payload per
			// event carrying every market on it, so the event is the unit of a
			// raw record and per-event ordering is what the normalizer needs.
			name:          "odds.raw.{provider} is retention-based and keyed by event",
			topic:         oddsRaw,
			wantName:      "odds.raw.synthetic",
			wantRetention: RetentionDelete,
			wantCompacted: false,
			wantKey:       KeyKindEventID,
			wantString:    "odds.raw.synthetic[delete,key=event_id]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.topic.Name(); got != tc.wantName {
				t.Errorf("Name() = %q, want %q", got, tc.wantName)
			}
			if got := tc.topic.Retention(); got != tc.wantRetention {
				t.Errorf("Retention() = %v, want %v", got, tc.wantRetention)
			}
			if got := tc.topic.Compacted(); got != tc.wantCompacted {
				t.Errorf("Compacted() = %v, want %v", got, tc.wantCompacted)
			}
			if got := tc.topic.KeyKind(); got != tc.wantKey {
				t.Errorf("KeyKind() = %v, want %v", got, tc.wantKey)
			}
			if tc.topic.IsZero() {
				t.Error("IsZero() = true for a registry topic")
			}
			if got := tc.topic.String(); got != tc.wantString {
				t.Errorf("String() = %q, want %q", got, tc.wantString)
			}
			if !tc.topic.Retention().Valid() {
				t.Errorf("Retention().Valid() = false for %s", tc.topic.Name())
			}
		})
	}
}

// TestZeroTopicIsInert checks that the zero Topic reports itself as such and
// carries no authority.
//
// It matters because Topic's fields are unexported with no setter, so the zero
// value is the ONLY forgeable Topic — and Compacted() on it must be false, since
// Compacted() is what authorises a tombstone.
func TestZeroTopicIsInert(t *testing.T) {
	t.Parallel()

	var zero Topic
	if !zero.IsZero() {
		t.Error("IsZero() = false for the zero Topic")
	}
	if zero.Name() != "" {
		t.Errorf("Name() = %q, want empty", zero.Name())
	}
	if zero.Compacted() {
		t.Error("Compacted() = true for the zero Topic; that would authorise a tombstone on a forged topic")
	}
	if zero.Retention() != RetentionUnknown {
		t.Errorf("Retention() = %v, want RetentionUnknown", zero.Retention())
	}
	if zero.KeyKind() != KeyKindUnknown {
		t.Errorf("KeyKind() = %v, want KeyKindUnknown", zero.KeyKind())
	}
	if got, want := zero.String(), "kafka.Topic(zero)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestNewProvider exercises the provider-slug charset, which is deliberately
// NARROWER than domain.Slug's.
//
// The narrowing is the contract with Terraform's raw_providers validation
// (`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`). A slug this package accepted and Terraform
// rejected would produce a service publishing to a topic that does not exist and
// cannot be auto-created.
func TestNewProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		slug  string
		valid bool
		why   string
	}{
		// The two slugs actually in use (ADR 003 / the contract ledger).
		{name: "synthetic", slug: "synthetic", valid: true},
		{name: "the-odds-api", slug: "the-odds-api", valid: true},

		{name: "a single letter", slug: "a", valid: true},
		{name: "a single digit", slug: "7", valid: true},
		{name: "digits and letters", slug: "book2", valid: true},
		{name: "several internal hyphens", slug: "a-b-c-d", valid: true},

		{name: "empty", slug: "", why: "empty"},
		{name: "uppercase", slug: "Synthetic", why: "Terraform's charset is lowercase only"},
		{
			name: "an underscore",
			slug: "the_odds_api",
			why: "mixing '.' and '_' in one Kafka topic name is a JMX metric-collision hazard; " +
				"Terraform rejects it and so must this",
		},
		{name: "a dot", slug: "odds.api", why: "a dot would add a topic-name component"},
		{name: "a leading hyphen", slug: "-synthetic", why: "produces odds.raw.-synthetic"},
		{name: "a trailing hyphen", slug: "synthetic-", why: "produces odds.raw.synthetic-"},
		{name: "a lone hyphen", slug: "-", why: "leading and trailing at once"},
		{name: "a space", slug: "the odds api", why: "whitespace"},
		{name: "a slash", slug: "odds/api", why: "not in Kafka's topic charset either"},
		{name: "a colon", slug: "odds:api", why: "would break event:{id} channel parsing downstream"},

		// The last component of odds.raw.{provider} has to leave room for the
		// nine-byte prefix inside Kafka's 249-byte limit.
		{name: "exactly at the length limit", slug: strings.Repeat("a", maxTopicNameLen-len(TopicOddsRawPrefix)), valid: true},
		{name: "one byte over the length limit", slug: strings.Repeat("a", maxTopicNameLen-len(TopicOddsRawPrefix)+1), why: "the full topic name would exceed 249 bytes"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := NewProvider(tc.slug)
			if tc.valid {
				if err != nil {
					t.Fatalf("NewProvider(%q) = error %v, want ok", tc.slug, err)
				}
				if got.String() != tc.slug {
					t.Errorf("String() = %q, want %q", got.String(), tc.slug)
				}
				return
			}

			if err == nil {
				t.Fatalf("NewProvider(%q) = ok, want error (%s)", tc.slug, tc.why)
			}
			if !errors.Is(err, ErrInvalidProvider) {
				t.Errorf("error = %v, want it to wrap ErrInvalidProvider", err)
			}
			if got != "" {
				t.Errorf("Provider = %q on the error path, want the zero value", got)
			}
		})
	}
}

// TestOddsRawBuildsAValidTopicName checks the full name, and that a provider
// rejected by NewProvider is also rejected here rather than producing a Topic
// with a name the broker will refuse.
func TestOddsRawBuildsAValidTopicName(t *testing.T) {
	t.Parallel()

	t.Run("a valid provider", func(t *testing.T) {
		t.Parallel()
		topic, err := OddsRaw("the-odds-api")
		if err != nil {
			t.Fatalf("OddsRaw: %v", err)
		}
		if got, want := topic.Name(), "odds.raw.the-odds-api"; got != want {
			t.Errorf("Name() = %q, want %q", got, want)
		}
		if err := validateTopicName(topic.Name()); err != nil {
			t.Errorf("the constructed name does not pass Kafka's own rules: %v", err)
		}
	})

	t.Run("at the length boundary the name is exactly 249 bytes", func(t *testing.T) {
		t.Parallel()
		topic, err := OddsRaw(Provider(strings.Repeat("a", maxTopicNameLen-len(TopicOddsRawPrefix))))
		if err != nil {
			t.Fatalf("OddsRaw: %v", err)
		}
		if got := len(topic.Name()); got != maxTopicNameLen {
			t.Fatalf("len(Name()) = %d, want %d", got, maxTopicNameLen)
		}
		if err := validateTopicName(topic.Name()); err != nil {
			t.Errorf("a name at exactly the limit must be legal: %v", err)
		}
	})

	t.Run("an invalid provider yields the zero Topic", func(t *testing.T) {
		t.Parallel()
		topic, err := OddsRaw("Not Valid")
		if err == nil {
			t.Fatal("OddsRaw = ok, want error")
		}
		if !errors.Is(err, ErrInvalidProvider) {
			t.Errorf("error = %v, want it to wrap ErrInvalidProvider", err)
		}
		if !topic.IsZero() {
			t.Errorf("Topic = %v, want the zero Topic on the error path", topic)
		}
	})
}

// TestTopicsListsTheNamedTopicsOnly checks that Topics() enumerates exactly the
// seven topics whose existence is not a deployment decision, in pipeline order.
//
// odds.raw.* is deliberately absent: which providers exist is held in Terraform's
// raw_providers, and a Go-side list of them would be a second copy that drifts.
//
// The ORDER is asserted, not just the membership. Callers enumerate this list for
// health checks and operator bookmarks, and pipeline order -- market stream, the
// signals derived from it, wager stream, the CLV derived from that -- is the one
// a human reading the output wants.
func TestTopicsListsTheNamedTopicsOnly(t *testing.T) {
	t.Parallel()

	got := Topics()
	want := []string{
		TopicOddsNormalized,
		TopicPriceComputed,
		TopicSignalsEV,
		TopicSignalsArb,
		TopicSignalsSteam,
		TopicWagerEvents,
		TopicSignalsCLV,
	}
	if len(got) != len(want) {
		t.Fatalf("Topics() returned %d topics, want %d: %v", len(got), len(want), got)
	}
	for i, topic := range got {
		if topic.Name() != want[i] {
			t.Errorf("Topics()[%d] = %q, want %q", i, topic.Name(), want[i])
		}
		if topic.IsZero() {
			t.Errorf("Topics()[%d] is the zero Topic", i)
		}
	}
}

// TestLookupTopicRoundTripsTheRegistry checks that a name resolves back to the
// same entry the constructor produced, and that an unrecognised name reports
// false rather than a plausible-looking zero Topic.
//
// The false answer is load-bearing: Delivery.requireKeyKind uses it to stay
// PERMISSIVE about topics this registry does not declare — the integration tests'
// throwaway topics, and phase 12's signals.* family.
func TestLookupTopicRoundTripsTheRegistry(t *testing.T) {
	t.Parallel()

	t.Run("registry topics resolve to themselves", func(t *testing.T) {
		t.Parallel()

		oddsRaw, err := OddsRaw("synthetic")
		if err != nil {
			t.Fatalf("OddsRaw: %v", err)
		}
		for _, want := range append(Topics(), oddsRaw) {
			got, ok := LookupTopic(want.Name())
			if !ok {
				t.Errorf("LookupTopic(%q) = not found", want.Name())
				continue
			}
			if got != want {
				t.Errorf("LookupTopic(%q) = %v, want %v", want.Name(), got, want)
			}
		}
	})

	tests := []struct {
		name  string
		topic string
		why   string
	}{
		{"an unrelated name", "some.other.topic", "not declared by this system"},
		{"empty", "", "not a topic at all"},
		{"the bare prefix", TopicOddsRawPrefix, "the provider component is empty"},
		{"a near miss", "odds.normalised", "British spelling; the frozen literal is odds.normalized"},
		{"a raw topic with an illegal provider", "odds.raw.The_Odds_API", "Terraform would reject that provider"},
		{"a raw topic with a trailing hyphen", "odds.raw.synthetic-", "a trailing hyphen is not a legal provider"},
		// signals.steam USED to be the standing example here of a topic this
		// registry deliberately did not enumerate. Phase 9 declares it, so the
		// example moves to a name from the family that is still hypothetical --
		// which keeps the permissive path covered without pretending a declared
		// topic is unknown.
		{"an undeclared member of the signals family", "signals.middles", "middles are not a phase-9 alert; see migration 00009"},
		{"a near miss in the signals family", "signals.arbitrage", "the frozen literal is signals.arb"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := LookupTopic(tc.topic)
			if ok {
				t.Fatalf("LookupTopic(%q) = %v, found; want not found (%s)", tc.topic, got, tc.why)
			}
			if !got.IsZero() {
				t.Errorf("LookupTopic(%q) returned %v, want the zero Topic", tc.topic, got)
			}
		})
	}
}

// TestExternalTopicIsUnknownButUsable checks the wrapper for topics outside the
// registry.
//
// The two properties that matter: it is NOT zero (it has a name, so it can be
// subscribed to), and its retention is UNKNOWN — which is what keeps an
// unrecognised topic out of the compaction-only code paths.
func TestExternalTopicIsUnknownButUsable(t *testing.T) {
	t.Parallel()

	topic := externalTopic("sharpline-it-throwaway")
	if topic.IsZero() {
		t.Fatal("IsZero() = true; an external topic has a name and must be usable")
	}
	if got, want := topic.Name(), "sharpline-it-throwaway"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
	if topic.Retention() != RetentionUnknown {
		t.Errorf("Retention() = %v, want RetentionUnknown", topic.Retention())
	}
	if topic.Compacted() {
		t.Error("Compacted() = true for an external topic; a tombstone must not be authorised on a topic whose cleanup policy this process cannot know")
	}
	if topic.KeyKind() != KeyKindUnknown {
		t.Errorf("KeyKind() = %v, want KeyKindUnknown", topic.KeyKind())
	}
}

// TestValidateTopicName applies Kafka's own rules.
//
// The point of validating locally at all is that a subscription typo otherwise
// surfaces as a metadata error several seconds into a poll loop, attributed to
// whatever the consumer happened to be doing.
func TestValidateTopicName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		topic string
		valid bool
		why   string
	}{
		{name: "a registry name", topic: TopicOddsNormalized, valid: true},
		{name: "underscores are legal in a topic name", topic: "sharpline_it_1", valid: true},
		{name: "hyphens", topic: "sharpline-it-1", valid: true},
		{name: "leading and trailing hyphens are legal for a topic", topic: "-x-", valid: true},
		{name: "mixed case is legal for a topic", topic: "SharplineIT", valid: true},
		{name: "a single dot-containing name", topic: "a.b", valid: true},
		{name: "exactly 249 bytes", topic: strings.Repeat("t", maxTopicNameLen), valid: true},

		{name: "empty", topic: "", why: "no name"},
		{name: "250 bytes", topic: strings.Repeat("t", maxTopicNameLen+1), why: "Kafka's limit is 249"},
		{name: "a single dot", topic: ".", why: "Kafka reserves it — it would collide with a directory entry in the log dir"},
		{name: "a double dot", topic: "..", why: "same reason"},
		{name: "a slash", topic: "odds/raw", why: "the topic name becomes a log directory name"},
		{name: "a colon", topic: "odds:raw", why: "outside [a-zA-Z0-9._-]"},
		{name: "a space", topic: "odds raw", why: "whitespace"},
		{name: "a plus", topic: "odds+raw", why: "outside the charset"},
		{name: "a non-ASCII byte", topic: "odds.raw.ünter", why: "outside the charset"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateTopicName(tc.topic)
			if tc.valid {
				if err != nil {
					t.Fatalf("validateTopicName(%q) = %v, want ok", tc.topic, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateTopicName(%q) = ok, want error (%s)", tc.topic, tc.why)
			}
			if !errors.Is(err, ErrInvalidTopic) {
				t.Errorf("error = %v, want it to wrap ErrInvalidTopic", err)
			}
		})
	}
}

// TestEveryDeclaredTopicNameIsLegal is the cheap cross-check that closes the
// loop: a name added to the registry that Kafka itself would refuse must fail
// here rather than at the first produce in a deployed cluster.
func TestEveryDeclaredTopicNameIsLegal(t *testing.T) {
	t.Parallel()

	names := []string{
		TopicOddsNormalized, TopicPriceComputed, TopicWagerEvents,
		TopicSignalsEV, TopicSignalsArb, TopicSignalsSteam, TopicSignalsCLV,
	}
	for _, provider := range []Provider{"synthetic", "the-odds-api"} {
		topic, err := OddsRaw(provider)
		if err != nil {
			t.Fatalf("OddsRaw(%q): %v", provider, err)
		}
		names = append(names, topic.Name())
	}

	for _, name := range names {
		if err := validateTopicName(name); err != nil {
			t.Errorf("declared topic %q is not a legal Kafka topic name: %v", name, err)
		}
	}
}
