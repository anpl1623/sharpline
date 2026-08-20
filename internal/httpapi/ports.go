// Ports: the seams internal/httpapi reaches the rest of the system through, and
// the neutral read model that crosses them.
//
// Every interface here is declared BY THE CONSUMER (CLAUDE.md §12), which is
// this package, and each is as narrow as the handlers that call it. Nothing in
// internal/platform/postgres, internal/platform/redis or internal/auth declares
// an interface for this package to depend on, and none of them imports it.
//
// # Why the read model is not the sqlc row and not the wire type
//
// Three shapes exist for the same fact and collapsing any two of them is a
// mistake this file exists to prevent:
//
//   - The sqlc row (internal/platform/postgres/gen) is the TABLE's shape. It
//     carries pgtype.Text, pgtype.Int4 and raw enum strings, and it changes
//     whenever a column does. Handing it to a handler makes a column rename a
//     breaking API change — which is exactly why sqlc.yaml sets
//     `emit_json_tags: false`.
//
//   - The generated wire type (internal/httpapi/gen) is the CONTRACT's shape,
//     owned by openapi.yaml. Making the store return it would put the database
//     adapter in charge of the published API.
//
//   - The read model below is the QUESTION's shape: parsed domain enums, real
//     Go optionals, and nothing the wire does not need. It is the only one of
//     the three that both sides can agree on without either owning the other.
//
// The translation cost is one mapping function per type, in exchange for a
// schema change and an API change being independently reviewable. On a project
// whose stated deliverable is the API contract, that is the right trade.
//
// # Where each port reads from, and why
//
//	Catalogue   Postgres. The source of truth for sports, leagues, books,
//	            events, markets and selections. Never cached: the catalogue
//	            changes at ingest's polling cadence, not at tick rate, and a
//	            stale league list is a bug nobody would think to look for.
//
//	Prices      Postgres (`prices` hypertable), optionally fronted by
//	            [PriceCache]. CLAUDE.md §3 puts the current-line snapshot in
//	            Redis and is explicit that Redis is "never the source of
//	            truth", so a cache miss falls through and the answer is
//	            identical either way. The compacted `price.computed` topic
//	            carries the pricer's DERIVED view (fair value, EV, arbitrage);
//	            it is the WebSocket gateway's input and phase 9's, not this
//	            endpoint's — a REST board that read a Kafka snapshot would
//	            need the whole compacted topic resident in every api replica to
//	            answer one event's page.
//
//	Ledger      Postgres, through the `account_balances` VIEW. The balance is
//	            a fold over ledger_entries and there is no balance column
//	            anywhere in the schema (CLAUDE.md §4).
//
//	Limits      Postgres, append-only history.
//
//	Sessions    internal/auth, which owns argon2id, the JWT issuer, the TOTP
//	            keyring and refresh-token rotation. This package does the HTTP
//	            around it and none of the cryptography.
//
//	Audit       Postgres (`audit_log` hypertable), written INSIDE the
//	            transaction that performs the change it records.
//
//	Wagers      Postgres. The customer's own placed tickets, read back from
//	            `wagers` and `legs`. Never cached and never derived from the
//	            bus: a wager is the record of a promise, and the only correct
//	            source for one is the row that recorded it.
//
//	Betting     internal/betting, which owns the placement transaction —
//	            self-exclusion, responsible-gaming limits, price-move
//	            detection, the balance fold and the double-entry stake
//	            movement, all under one lock. This package does the HTTP
//	            around it and NONE of the money.
//
//	CashOutQuotes / CashOuts
//	            internal/betting prices an early close; whatever EXECUTES one
//	            is a state transition on a placed ticket and belongs with the
//	            other transitions. The two are separate ports so a deployment
//	            can serve the quote without being able to take it.
//
//	TicketPricer
//	            internal/betting again. Ticket pricing is not a function of
//	            the leg prices in general, so the quote endpoint borrows the
//	            SAME pricer the placement service uses rather than
//	            multiplying decimals itself.
//
// # Why internal/betting's own types cross the Betting seam, when nothing else's do
//
// [Betting], [CashOutQuotes] and [TicketPricer] are declared here, by the
// consumer, exactly like every other port — but their parameters are
// internal/betting's value types rather than a read model of this package's
// own, and that is deliberate rather than a lapse.
//
// The rule those three break is "the read model is the QUESTION's shape", and
// the rule exists to stop a PRODUCER owning this package's vocabulary. Here the
// producer is not a database adapter whose shape follows a column list; it is
// the domain service whose whole purpose is the vocabulary — a betting.Slip IS
// the question, expressed in domain value types with no pgtype, no wire tag and
// no HTTP anywhere in it. A parallel httpapi.Slip would be a field-for-field
// copy whose only job would be to be copied back, and the copy is where a field
// eventually goes missing.
//
// The property the rule protects is kept by other means: because the interfaces
// are declared HERE and narrowly, *betting.Service satisfies them without
// knowing this package exists, and a test satisfies them with a struct literal.
// internal/betting does not import internal/httpapi and never will.
package httpapi

import (
	"context"
	"net/netip"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/betting"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// -----------------------------------------------------------------------------
// Read model
// -----------------------------------------------------------------------------

// Sport is one sport in the catalogue.
type Sport struct {
	ID   domain.SportID
	Slug domain.Slug
	Name string
}

// League is one competition under a sport.
type League struct {
	ID      domain.LeagueID
	SportID domain.SportID
	Slug    domain.Slug
	Name    string
}

// Book is one sportsbook whose lines are ingested.
type Book struct {
	ID   domain.BookID
	Slug domain.Slug
	Name string
	Kind domain.BookKind

	// Reference marks the sharp book the pricer devigs against. Every EV number
	// in the system is relative to it, so a surface that renders EV has to be
	// able to name it.
	Reference bool
}

// Event is one contest, as the board and the detail page need it.
type Event struct {
	ID       domain.EventID
	LeagueID domain.LeagueID
	Kind     domain.EventKind
	Name     string

	HomeCompetitorID   *domain.CompetitorID
	HomeCompetitorName string
	AwayCompetitorID   *domain.CompetitorID
	AwayCompetitorName string

	ScheduledStart time.Time
	Status         domain.EventStatus

	// Clock is present only while the event has one. A scheduled event has no
	// clock at all, which is a different fact from "a clock reading zero" and is
	// why this is a pointer rather than a zero-valued struct.
	Clock *GameClock
	Score *Score

	// ObservedAt is the provider's instant for this event's state.
	ObservedAt time.Time
}

// GameClock is live clock state.
type GameClock struct {
	Period  *int32
	Elapsed *time.Duration
	Running bool
}

// Score is the live score of a fixture.
type Score struct {
	Home int32
	Away int32
}

// Market is one question about an event.
type Market struct {
	ID      domain.MarketID
	EventID domain.EventID
	Type    domain.MarketType

	// Line is the handicap or total, absent on a market that carries none.
	Line *float64

	// Subject is the player a prop market is about, empty on every other type.
	Subject string

	Status     domain.MarketStatus
	ObservedAt time.Time
}

// Selection is one answer to a market.
type Selection struct {
	ID         domain.SelectionID
	MarketID   domain.MarketID
	MarketType domain.MarketType
	Role       domain.SelectionRole
	Name       string
}

// Quote is one book's current price on one selection.
//
// It is called Quote rather than Price because domain.Price is an immutable
// historical row and this is a snapshot of the newest one; naming both Price
// invites the confusion that makes a board render a stale line.
type Quote struct {
	SelectionID domain.SelectionID
	BookID      domain.BookID
	Odds        odds.Decimal
	Line        *float64

	// ObservedAt is the provider's own instant; IngestedAt is when this system
	// received it. Their difference is the provider-attributable share of the
	// staleness SLO, and neither is interchangeable with the other.
	ObservedAt time.Time
	IngestedAt time.Time
}

// HistoryPoint is one point of a line-movement series.
//
// On a raw series Open, High, Low and Close are the same stored quote and
// Samples is 1, so a client renders one shape either way.
type HistoryPoint struct {
	At      time.Time
	Open    odds.Decimal
	High    odds.Decimal
	Low     odds.Decimal
	Close   odds.Decimal
	Line    *float64
	Samples int32
}

// Balance is one derived account balance.
type Balance struct {
	Kind domain.AccountKind

	// Amount is minor units. It is DERIVED from ledger_entries on every read;
	// nothing stores it (CLAUDE.md §4).
	Amount domain.Money

	// Entries is how many ledger entries were folded. Zero means the account has
	// never moved, which is a different fact from "moved and nets to zero" and
	// is the only thing that distinguishes them.
	Entries int64
}

// Profile is the authenticated user's whole persisted identity.
//
// There is no name, address, date of birth, document, country or jurisdiction
// field. Migration 00005 has no column for any of them and CLAUDE.md §0 forbids
// adding one: this is not a licensed sportsbook.
type Profile struct {
	ID        domain.UserID
	Email     string
	Status    auth.UserStatus
	CreatedAt time.Time

	// TOTPConfirmed is true only for a CONFIRMED enrolment. An unconfirmed row
	// is not a second factor — treating it as one locks out a user whose QR
	// scan failed.
	TOTPConfirmed bool

	// TOTPPending is true when an enrolment has been started and not confirmed.
	TOTPPending bool
}

// Limit is one self-imposed responsible-gaming limit.
type Limit struct {
	ID     string
	UserID domain.UserID
	Kind   auth.LimitKind
	Period auth.LimitPeriod

	// Amount is set on the money kinds (grant, stake, loss) and nil on session.
	Amount *domain.Money
	// Duration is set on session and nil on the money kinds.
	Duration *time.Duration

	RequestedAt   time.Time
	EffectiveFrom time.Time
}

// Session is the result of a successful authentication or rotation.
//
// AccessToken and RefreshToken are the only two secrets this package ever holds,
// they are held for exactly as long as it takes to serialise one response, and
// neither is ever logged, traced, span-attributed or written to the audit log.
type Session struct {
	AccessToken      string
	AccessExpiresIn  time.Duration
	RefreshToken     string
	RefreshExpiresAt time.Time
	Profile          Profile
}

// Enrolment is a started, unconfirmed TOTP enrolment.
type Enrolment struct {
	// ProvisioningURI is `otpauth://totp/...` and EMBEDS THE SHARED SECRET. It
	// is returned to the user once and must never reach a log, a span or a
	// metric.
	ProvisioningURI string
	ExpiresAt       time.Time
}

// -----------------------------------------------------------------------------
// Cursor-paginated queries
// -----------------------------------------------------------------------------

// EventPageQuery asks for one keyset page of events.
//
// After is nil on the first page. When it is set, StartingBefore MUST be the
// same value the cursor was minted under — the cursor codec binds the window
// into the cursor and the handler rejects a mismatch, so a caller cannot
// silently page through a different set than the one it started in.
type EventPageQuery struct {
	// LeagueID scopes the page to one league. Zero means every league, which is
	// served by a different index and therefore a different statement.
	LeagueID domain.LeagueID

	StartingBefore time.Time
	After          *EventKey

	// Limit is the page size. The store fetches Limit+1 rows to answer HasMore
	// without a second count query, and returns at most Limit.
	Limit int32
}

// EventKey is the total ordering every event page is sorted and cursored by.
//
// ScheduledStart alone is NOT total — two fixtures kicking off at the same
// instant are returned in an arbitrary order — so a cursor on it alone cannot
// say which row it points after. ID is a primary key, so the pair is total.
type EventKey struct {
	ScheduledStart time.Time
	ID             domain.EventID
}

// EventPage is one page of events plus whether another exists.
type EventPage struct {
	Events  []Event
	HasMore bool
}

// SearchQuery asks for one keyset page of search hits.
//
// Prefix is the raw user input. The store is responsible for escaping LIKE
// metacharacters in it; a caller must not pre-escape, because doing it twice
// makes a literal backslash unsearchable.
type SearchQuery struct {
	Prefix string
	After  *EventKey
	Limit  int32
}

// SearchPage is one page of search hits.
type SearchPage struct {
	Events  []Event
	HasMore bool
}

// HistoryQuery asks for a line-movement series.
//
// From and To are REQUIRED and the window is half-open [From, To). `prices` is
// partitioned on observed_at and has no retention policy, so an unbounded read
// consults an index on every chunk that has ever existed; there is deliberately
// no way to express one here.
type HistoryQuery struct {
	SelectionID domain.SelectionID
	BookID      domain.BookID
	From        time.Time
	To          time.Time

	// Bucket is the downsampling width. Zero means raw — every stored quote.
	Bucket time.Duration

	// MaxPoints bounds the result. The handler rejects a window that would
	// exceed it rather than truncating, because a truncated chart lies about
	// where the line ended.
	MaxPoints int32
}

// -----------------------------------------------------------------------------
// Ports
// -----------------------------------------------------------------------------

// Catalogue reads the entities the board and the detail page are built from.
type Catalogue interface {
	Sports(ctx context.Context) ([]Sport, error)
	LeaguesInSport(ctx context.Context, sport domain.SportID) ([]League, error)
	LeagueBySlug(ctx context.Context, slug domain.Slug) (League, error)
	Books(ctx context.Context) ([]Book, error)

	EventPage(ctx context.Context, q EventPageQuery) (EventPage, error)
	SearchEvents(ctx context.Context, q SearchQuery) (SearchPage, error)

	// EventWithBreadcrumb returns the event together with the league and sport it
	// sits under. ONE query with two joins, not three primary-key lookups: the
	// detail page always renders the breadcrumb, so three round trips would be
	// three where one does.
	EventWithBreadcrumb(ctx context.Context, id domain.EventID) (Event, League, Sport, error)

	Market(ctx context.Context, id domain.MarketID) (Market, error)
	Selection(ctx context.Context, id domain.SelectionID) (Selection, error)

	// MarketsForEvents is the board's market tree for a whole page of events.
	// Separate from MarketsForEvent because a page of fifty events served one
	// query at a time is fifty round trips whose per-call overhead dominates a
	// query that is otherwise a bounded index scan.
	MarketsForEvents(ctx context.Context, ids []domain.EventID) ([]Market, error)

	// SelectionsForMarkets takes a set because both callers have one: the detail
	// page holds every market of an event and the board holds the main markets
	// of every event on screen. One round trip, not one per market.
	SelectionsForMarkets(ctx context.Context, ids []domain.MarketID) ([]Selection, error)
}

// Prices reads current lines and line history.
type Prices interface {
	// CurrentQuotes returns the newest quote from each book on each selection,
	// restricted to quotes observed after `since`.
	//
	// `since` is a STALENESS HORIZON as well as a chunk filter: a quote older
	// than it is not a current line, it is history. It is a required argument
	// and not a default inside the store, so the caller states what it means by
	// "current" rather than inheriting somebody else's answer.
	CurrentQuotes(ctx context.Context, selections []domain.SelectionID, since time.Time) ([]Quote, error)

	History(ctx context.Context, q HistoryQuery) ([]HistoryPoint, error)
}

// PriceCache is the optional Redis snapshot in front of [Prices].
//
// CLAUDE.md §3 assigns Redis the "current-line snapshot cache" and states it is
// "never the source of truth". Every method here is therefore allowed to fail
// or to answer nothing, and the caller falls through to Postgres and gets an
// identical answer. A nil PriceCache disables caching entirely, which is what a
// unit test wants and what a cold start does.
type PriceCache interface {
	// Quotes returns the cached quotes it has and the selections it did not.
	// It never reports an error for a miss; an error means the cache itself is
	// unhealthy and the caller ignores it and reads through.
	Quotes(ctx context.Context, selections []domain.SelectionID) (found []Quote, missing []domain.SelectionID, err error)

	// Store writes quotes back. Best effort: a failure is logged and dropped,
	// never surfaced to the client, because a cache write failing has no effect
	// on the correctness of the response already computed.
	Store(ctx context.Context, quotes []Quote, ttl time.Duration) error
}

// Ledger derives balances.
//
// There is no Credit, Debit or SetBalance here and there never will be: money
// moves only through a double-entry transaction, which is internal/betting's
// (phase 8), and the API's account surface is read-only over it.
type Ledger interface {
	// Balances returns the user's cash and escrow balances. An account with no
	// entries is returned with a zero amount and zero entries rather than
	// omitted, so a caller cannot mistake "never funded" for "not found".
	Balances(ctx context.Context, user domain.UserID) ([]Balance, error)
}

// Accounts reads the authenticated user's profile, and applies the one change
// a customer may make to it.
type Accounts interface {
	Profile(ctx context.Context, user domain.UserID) (Profile, error)

	// SelfExclude moves the caller to `self_excluded` and writes the audit
	// entry, in ONE transaction.
	//
	// # It only ever narrows, and that is structural rather than conventional
	//
	// The statement behind it can reach `self_excluded` and `closed` and no
	// other status, and refuses a source that is already one of those. So this
	// method cannot lift a self-exclusion, cannot lift an operator's suspension
	// and cannot reinstate a closed account — no matter what an implementation
	// passes and no matter what a bug upstream computes. Reinstatement is an
	// operator action with a different actor and a different authorisation, and
	// it gets its own statement when the admin console is built. It must not be
	// added to this one.
	//
	// # A no-op is a success, not an error
	//
	// A customer who is already self-excluded has the outcome they asked for.
	// The implementation returns the profile as it stands and no error, and
	// writes NO audit row — nothing changed, and the trail records changes.
	// Reporting a 409 or a 500 for "you are already protected" would be a
	// failure message on the one endpoint where a customer is least able to
	// deal with one.
	//
	// A user id that does not exist is [ErrNotFound], which the handler answers
	// the same way [Accounts.Profile] does: the token verified but names nobody,
	// which is a server-side inconsistency rather than the caller's fault.
	SelfExclude(ctx context.Context, req SelfExclusion) (Profile, error)
}

// SelfExclusion is a customer's request to stop themselves wagering.
//
// It carries no status field. The destination is not the caller's to choose —
// this endpoint means exactly one thing — and a field would invite an
// implementation to pass 'active' and discover the database refusing it, rather
// than the API making the wrong request unrepresentable.
type SelfExclusion struct {
	UserID domain.UserID

	// Audit is the provenance stamped onto the audit entry written in the same
	// transaction, exactly as on [SetLimit].
	Audit AuditContext
}

// Limits reads and writes self-imposed responsible-gaming limits.
type Limits interface {
	Current(ctx context.Context, user domain.UserID) ([]Limit, error)

	// Set records a new limit and supersedes the one it replaces, in ONE
	// transaction that also writes the audit entry.
	//
	// The cooling-off asymmetry — a tightening binds immediately, a loosening
	// binds later — is policy and is decided by the implementation, which is
	// the only place that can read the current row and the new one under the
	// same lock. The handler reports whatever EffectiveFrom comes back rather
	// than computing it, so there is exactly one implementation of the rule.
	Set(ctx context.Context, req SetLimit) (Limit, error)
}

// SetLimit is a request to change one self-imposed limit.
//
// Exactly one of Amount and Duration is set, decided by Kind: session takes a
// duration, every other kind takes an amount. Both nil REMOVES the limit, which
// is a loosening and serves the cooling-off period like any other.
type SetLimit struct {
	UserID   domain.UserID
	Kind     auth.LimitKind
	Period   auth.LimitPeriod
	Amount   *domain.Money
	Duration *time.Duration

	// Audit is the provenance stamped onto the audit entry written in the same
	// transaction.
	Audit AuditContext
}

// Sessions is the authentication surface. internal/auth owns every secret and
// every comparison; this package owns only the HTTP around them.
//
// THE CONTRACT ON ERRORS IS PART OF THE SECURITY MODEL. Authenticate must
// return auth.ErrCredentials for an unknown address AND for a wrong password,
// having done the same amount of work in both cases, so the two are
// indistinguishable in body, status and timing. Redeem must return
// auth.ErrTokenUnknown for an unknown token AND, after revoking the family,
// for a reused one — a thief must not learn from the response that they tripped
// a detector.
type Sessions interface {
	Register(ctx context.Context, email, password string, ac AuditContext) (Session, error)
	Authenticate(ctx context.Context, email, password, totpCode string, ac AuditContext) (Session, error)

	// Redeem rotates a refresh token: it invalidates the presented token and
	// issues its successor in one transaction. Presenting an already-redeemed
	// token revokes the WHOLE FAMILY.
	Redeem(ctx context.Context, refreshToken string, ac AuditContext) (Session, error)

	// Revoke ends the presented token's family. Idempotent, and it reports no
	// error for an unknown token: distinguishing "already dead" from "never
	// existed" is an enumeration oracle and no caller can act on it.
	Revoke(ctx context.Context, refreshToken string, ac AuditContext) error

	// There is deliberately NO Verify method here. Access-token verification is
	// internal/httpapi/middleware's (Authenticate), built by the composition
	// root over internal/auth's TokenIssuer — which pins the signing algorithm
	// and IGNORES the `alg` header on the presented token, so `alg: none` and
	// algorithm confusion are unrepresentable rather than merely rejected.
	// Declaring it here too would put a second verifier in the tree, and a
	// second verifier is a second place for it to be subtly wrong.

	BeginTOTP(ctx context.Context, user domain.UserID, ac AuditContext) (Enrolment, error)
	ConfirmTOTP(ctx context.Context, user domain.UserID, code string, ac AuditContext) error
	RemoveTOTP(ctx context.Context, user domain.UserID, code string, ac AuditContext) error
}

// AuditContext is the provenance every state-changing action carries into the
// audit log (CLAUDE.md §6: "audit log on every state-changing action").
//
// It holds NO SECRET and has no field one could be put in. Not a password, not
// a token, not a TOTP code, not an Authorization header — the only client-
// supplied value here is the address, which migration 00007 calls out as the
// single PII-bearing column in the schema and keeps deliberately.
type AuditContext struct {
	// RequestID correlates the audit row with the access-log line and the error
	// body the client received.
	RequestID string

	// ClientIP is resolved by the server's trusted-proxy logic, never read from
	// a header by a handler. Invalid when the peer could not be determined.
	ClientIP netip.Addr

	// TraceID and SpanID are W3C Trace Context ids in lowercase hex, which is
	// what makes the row joinable to a Jaeger trace. Empty when the request
	// carried no sampled span.
	TraceID string
	SpanID  string

	// At is the instant the action occurred, supplied by the caller rather than
	// taken from the database clock so a redelivered message re-applies the
	// original instant.
	At time.Time
}

// forBetting projects the provenance onto internal/betting's own declaration of
// the same idea, for the two money-moving requests that carry one.
//
// TWO DECLARATIONS RATHER THAN ONE SHARED TYPE, deliberately. internal/betting
// cannot import this package — the arrow points the other way — and CLAUDE.md
// §12 puts an interface at its consumer, which applies to the value types that
// cross it. Hoisting AuditContext into a third package to be shared would make
// a change to either side's provenance a change to a package both depend on,
// which is the coupling the read-model argument at the top of this file spends
// three paragraphs refusing.
//
// At IS DROPPED, and its absence on the far side is the point: internal/betting
// has its own injected clock and insists that one placement has ONE instant —
// placed_at, the limit windows, the staleness horizon and the ledger's
// occurred_at are all one value read at the top of the operation. Handing it a
// second instant from the handler would put two in a transaction whose whole
// discipline is that there is one. [Limits.Set] takes it because that store has
// no clock at all; this one does.
func (ac AuditContext) forBetting() betting.AuditContext {
	return betting.AuditContext{
		RequestID: ac.RequestID,
		ClientIP:  ac.ClientIP,
		TraceID:   ac.TraceID,
		SpanID:    ac.SpanID,
	}
}

// Audit writes the audit log.
//
// Most writes go through the transaction that performs the change (see
// [Limits.Set]); this port exists for the actions with no other transaction of
// their own to join.
type Audit interface {
	Record(ctx context.Context, e AuditEntry) error
}

// AuditEntry is one audit-log row.
type AuditEntry struct {
	Context AuditContext

	// ActorKind is "user", "system" or "operator". ActorID is never empty: for
	// a system actor it is the service name, because a row with no actor
	// answers none of the questions the table exists to answer.
	ActorKind string
	ActorID   string

	// Action is dotted `domain.verb`: "user_limit.set", "totp.enrol_confirm".
	Action string

	EntityType string
	EntityID   string

	// Outcome is "success" or "failure". A rejected action is at least as
	// interesting as an accepted one.
	Outcome string
	Reason  string

	// Before and After hold the CHANGED FIELDS ONLY — a diff, not an entity
	// dump — and are nil where nothing persisted changed. Nothing secret is
	// ever placed in either.
	Before map[string]any
	After  map[string]any
}

// Clock is the only source of "now" in this package.
//
// It is a port so tests can pin time without sleeping, and because "as_of" on
// every board page is a value the client computes staleness against — a handler
// reading time.Now() in three places would put three different instants in one
// response.
type Clock func() time.Time

// -----------------------------------------------------------------------------
// Betting: the read model
// -----------------------------------------------------------------------------

// Wager is one placed ticket as the history and detail surfaces need it.
//
// # Why this is not domain.Wager
//
// [Betting] hands back domain.Wager values, because a wager that was just
// BOOKED has necessarily passed every invariant domain.NewWager enforces — it
// was constructed by one. A wager READ BACK out of Postgres has not, and cannot
// be assumed to: the row may have been written by a build whose ticket pricer
// this one no longer has (a teaser priced from a ladder that is no longer
// configured), or by a future one, and domain.NewWager would refuse it.
//
// Refusing to RENDER a ticket the book has already accepted the customer's
// money for is the wrong failure. A history page that 500s because one old row
// no longer satisfies a constructor is worse in every way than one that shows
// the row. So the read path carries this flat shape, with the enums parsed at
// the boundary and nothing re-validated, and [wagerFromDomain] converts the
// other direction for the placement response so that one wire mapper serves
// both.
//
// Nothing here is mutable and nothing here is a live reference. In particular
// [WagerLeg.Decimal] is the price AT PLACEMENT and is never re-resolved; see
// the type's own comment.
type Wager struct {
	ID     domain.WagerID
	UserID domain.UserID
	Kind   domain.WagerKind
	Status domain.WagerStatus

	Stake domain.Money

	// Decimal is the whole TICKET's accepted price, bounded by
	// domain.MaxWagerDecimal (1e9) rather than by odds.MaxDecimalOdds (1e5): a
	// 20-leg parlay of even-money legs is 2^20 and the market-price bound would
	// wrongly reject it.
	Decimal odds.Decimal

	// Rounding is the rule stake x price was collapsed under, recorded so a
	// later repricing uses the rule the ticket was written under.
	Rounding domain.Rounding

	PotentialPayout domain.Money
	PotentialProfit domain.Money

	// TeaserPoints is set exactly on a teaser; RoundRobinID exactly on a
	// round-robin ticket. Both biconditionals are CHECK constraints in
	// migration 00006, so a row carrying one without the other cannot exist.
	TeaserPoints *float64
	RoundRobinID *domain.RoundRobinID

	// Returned and NetReturn are both set or both nil, and are non-nil exactly
	// when Status is terminal. They are the only authority on what settlement
	// paid: a partially-voided parlay returns less than PotentialPayout and a
	// cash-out returns whatever price was taken.
	Returned  *domain.Money
	NetReturn *domain.Money

	Legs []WagerLeg

	PlacedAt time.Time

	// UpdatedAt is the instant of the latest transition, from the acting
	// service's clock — not row bookkeeping. There is no SettledAt field
	// because it IS this value once Status is terminal, and a second copy of
	// one instant is a second thing to keep in agreement.
	UpdatedAt time.Time
}

// SettledAt reports the settlement instant, and whether the ticket has one.
//
// domain.Wager.SettledAt draws the line in the same place and the two must
// agree; this is that method restated over the flat read model rather than a
// second rule.
func (w Wager) SettledAt() (time.Time, bool) {
	if !w.Status.IsTerminal() {
		return time.Time{}, false
	}
	return w.UpdatedAt, true
}

// WagerLeg is one selection on a placed ticket, holding THE PRICE AT PLACEMENT.
//
// BookID, Decimal, Line and PriceObservedAt are a copied domain.Price value, not
// a reference into the price series. CLAUDE.md §4 is emphatic — "Legs hold the
// price at placement time, never a live reference" — and migration 00006 makes
// it structural: `legs` has no foreign key into the `prices` hypertable and a
// trigger freezes those columns after insert. Nothing in this package may look
// up a current price for a booked leg, and there is no field here through which
// one could arrive.
type WagerLeg struct {
	ID          domain.LegID
	EventID     domain.EventID
	MarketID    domain.MarketID
	MarketType  domain.MarketType
	SelectionID domain.SelectionID
	Role        domain.SelectionRole
	Status      domain.LegStatus

	BookID  domain.BookID
	Decimal odds.Decimal

	// Line is the line the quote was made at, from THIS SELECTION's own
	// perspective — already inverted for an away spread. TeasedLine is the
	// moved line a teaser leg grades at, kept BESIDE the real price rather than
	// replacing it, because the book never traded at the teased number and a
	// forged quote there would corrupt line history and destroy CLV.
	Line       domain.Line
	TeasedLine domain.Line

	PriceObservedAt time.Time

	// GradedAt is set exactly when Status is not pending. Per leg, because the
	// legs of a parlay grade at different times.
	GradedAt *time.Time
}

// GradingLine is the line this leg actually grades at.
//
// domain.Leg.GradingLine is the same rule; this restates it over the read model
// so the wire mapper does not decide it.
func (l WagerLeg) GradingLine() domain.Line {
	if l.TeasedLine.Present() {
		return l.TeasedLine
	}
	return l.Line
}

// wagerFromDomain projects a freshly-booked domain.Wager onto the read model.
//
// The placement path and the history path then converge on one wire mapper,
// which is what stops a wager rendering differently depending on whether the
// client just placed it or came back to it an hour later.
func wagerFromDomain(w domain.Wager) Wager {
	out := Wager{
		ID:              w.ID(),
		UserID:          w.UserID(),
		Kind:            w.Kind(),
		Status:          w.Status(),
		Stake:           w.Stake(),
		Decimal:         odds.Decimal(w.AcceptedDecimal()),
		Rounding:        w.Rounding(),
		PotentialPayout: w.PotentialPayout(),
		PotentialProfit: w.PotentialProfit(),
		PlacedAt:        w.PlacedAt(),
		UpdatedAt:       w.UpdatedAt(),
		Legs:            make([]WagerLeg, 0, w.LegCount()),
	}
	if points, ok := w.TeaserPoints(); ok {
		out.TeaserPoints = &points
	}
	if rr, ok := w.RoundRobinID(); ok {
		out.RoundRobinID = &rr
	}
	// Returned and NetReturn share one presence flag in the domain, so they are
	// read through the same `ok` rather than tested separately — the pair is
	// never half-set and this is what keeps that true on the way out.
	if returned, ok := w.Returned(); ok {
		net, _ := w.NetReturn()
		out.Returned = &returned
		out.NetReturn = &net
	}
	for _, leg := range w.Legs() {
		out.Legs = append(out.Legs, legFromDomain(leg))
	}
	return out
}

func legFromDomain(l domain.Leg) WagerLeg {
	out := WagerLeg{
		ID:              l.ID(),
		EventID:         l.EventID(),
		MarketID:        l.MarketID(),
		MarketType:      l.MarketType(),
		SelectionID:     l.SelectionID(),
		Role:            l.Role(),
		Status:          l.Status(),
		BookID:          l.Price().BookID(),
		Decimal:         odds.Decimal(l.Price().Decimal()),
		Line:            l.Price().Line(),
		TeasedLine:      l.TeasedLine(),
		PriceObservedAt: l.Price().ObservedAt(),
	}
	if at, ok := l.GradedAt(); ok {
		out.GradedAt = &at
	}
	return out
}

// WagerKey is the total ordering wager history is sorted and cursored by.
//
// PlacedAt alone is NOT total: a round robin writes N tickets at one instant, so
// a cursor naming only the instant cannot say which of them it points after. ID
// is a primary key, so the pair is total. This is the same argument [EventKey]
// makes about two fixtures kicking off together, and the round-robin case is why
// it is not merely theoretical here.
type WagerKey struct {
	PlacedAt time.Time
	ID       domain.WagerID
}

// WagerPageQuery asks for one keyset page of a customer's wagers, newest first.
type WagerPageQuery struct {
	UserID domain.UserID

	// Statuses restricts the page. Empty means every status.
	//
	// THE FILTER IS APPLIED TO THE PAGE THE STORE SCANNED, not pushed into the
	// statement, so a filtered page may hold fewer than Limit rows while
	// HasMore is still true. The two statements that serve this read take no
	// status parameter, and adding a third and fourth shape to the query file —
	// each with its own entry in the index-plan gate — for a parameter that is
	// usually absent buys less than it costs. A page that is occasionally short
	// is a far smaller defect than a page that is occasionally wrong, which is
	// what an OFFSET-based filter would give.
	Statuses []domain.WagerStatus

	After *WagerKey

	// Limit is the page size. The store reads Limit+1 rows to answer HasMore
	// without a second count query.
	Limit int32
}

// WagerPage is one page of wagers plus whether another exists.
//
// HasMore is about the SCAN, not about the filtered result: it is true whenever
// the store stopped short of exhausting the customer's history, so a client
// follows NextCursor until it is false rather than stopping at a short page.
type WagerPage struct {
	Wagers  []Wager
	HasMore bool

	// Last is the ordering key of the last row SCANNED, which is what the next
	// cursor is minted from. It is the last scanned row and not the last
	// returned one: minting from a returned row would skip every filtered-out
	// row between it and the scan's end.
	Last WagerKey
}

// -----------------------------------------------------------------------------
// Betting: the ports
// -----------------------------------------------------------------------------

// Wagers reads the authenticated customer's placed tickets.
//
// EVERY METHOD TAKES THE USER AND SCOPES BY IT. There is no "read any wager"
// method here and there must not be: the identifier of a wager appears in a URL,
// so a lookup that did not scope would need an ownership comparison at every
// call site, and the one call site that forgot would serve another customer's
// ticket. Scoping in the port makes forgetting unrepresentable.
type Wagers interface {
	// Wager returns one ticket with its legs, or [ErrNotFound].
	//
	// A wager that exists but belongs to somebody else returns [ErrNotFound] —
	// the SAME error as a wager that does not exist, produced by the same
	// branch, so nothing above can tell the two apart. That is deliberate and
	// it is the one place this API uses 404 for an authorization outcome: a 403
	// here would confirm the id exists, which is a wager-enumeration oracle
	// over every customer of the book.
	Wager(ctx context.Context, user domain.UserID, id domain.WagerID) (Wager, error)

	// WagerPage returns one keyset page, newest first.
	WagerPage(ctx context.Context, q WagerPageQuery) (WagerPage, error)
}

// Betting places wagers.
//
// One method, because placement is one transaction and this package has no
// business decomposing it. Everything that decides whether a slip becomes a
// ticket — the self-exclusion read against a locked row, the responsible-gaming
// limit sums, the balance fold, the price re-read and the price-move
// comparison, the round-robin expansion, the double-entry stake movement —
// happens inside it, in that order, for reasons internal/betting's doc.go
// argues at length. A handler that could call any of those separately could
// call them in a different order.
//
// *betting.Service satisfies this without knowing this package exists.
type Betting interface {
	Place(ctx context.Context, req betting.PlaceRequest) (betting.Placement, error)

	// Grant credits the customer's cash account with play money.
	//
	// It sits on THIS port rather than on a port of its own because it is the
	// same kind of thing as a placement and shares its machinery exactly: one
	// transaction, the users row locked first, the responsible-gaming limit
	// sums evaluated inside it, a double-entry movement, and an identifier
	// derived from (user, Idempotency-Key) so a replay collides with its own
	// primary key. A separate port would suggest a separate discipline applies,
	// and none does.
	//
	// It is the only path by which money ENTERS the system. Every other
	// movement is zero-sum between accounts that already hold a balance, so
	// without this one every balance is permanently zero.
	Grant(ctx context.Context, req betting.GrantRequest) (betting.Grant, error)
}

// CashOutQuotes prices an early close.
//
// IT DOES NOT SCOPE BY USER, and that is not an oversight in the port: quoting
// is pure pricing over a ticket, and the ticket's owner is not an input to it.
// The handler establishes ownership FIRST, through [Wagers.Wager], and quotes
// only a ticket that read back — so an unauthorised caller gets the 404 that
// read produced and never reaches a pricing call at all.
type CashOutQuotes interface {
	CashOutQuote(ctx context.Context, id domain.WagerID) (betting.CashOutQuote, error)
}

// CashOuts EXECUTES an early close: it settles the ticket at `cashed_out`,
// returns the value to the customer's cash account and releases the escrowed
// stake, in one balanced transaction.
//
// # Why this is a separate port from [CashOutQuotes], and why it is OPTIONAL
//
// Taking a cash-out is a state transition on a placed ticket, and every other
// transition — grading a leg, settling a wager, writing the payout — belongs to
// internal/settlement. internal/betting states the reason for the split in its
// own doc.go and it is not a layering preference: a component that could both
// QUOTE and TAKE a cash-out could do both in one transaction at a price of its
// own choosing, which is the shape an operator fraud takes.
//
// So this port exists, [API.Routes] mounts `POST /wagers/{id}/cashout` only when
// it is non-nil, and a deployment without it serves the quote and answers the
// spec's own 404 on the take. That is the same degradation [APIOptions.Sessions]
// takes and it is chosen for the same reason: a route that exists and returns a
// fabricated or unimplemented answer is worse than an absent one.
//
// # The audit requirement on whoever implements this
//
// A cash-out is a state-changing action, so CLAUDE.md §6 requires an audit
// entry, and [TakeCashOut] carries the provenance for it. The entry MUST be
// written inside the same transaction as the settlement, on the same pgx.Tx —
// the arrangement internal/betting's Tx.RecordAudit and this package's
// [Limits.Set] both use, and for the reason both of them state: a row that can
// commit without its money movement, or a movement that can commit without its
// row, is the gap the requirement exists to close.
//
// Unlike a placement or a grant, a cash-out MUTATES an existing ticket, so its
// entry has a real before-image — the wager's status and, at minimum, the value
// returned — and should carry one. The action is `wager.cash_out` and the
// entity is the wager. Nothing in this repository implements this port yet, so
// that is a contract on a future implementation and is written here rather than
// asserted anywhere as done.
type CashOuts interface {
	TakeCashOut(ctx context.Context, req TakeCashOut) (domain.Wager, error)
}

// TakeCashOut is a request to close a ticket early at a quoted value.
type TakeCashOut struct {
	UserID  domain.UserID
	WagerID domain.WagerID

	// IdempotencyKey is the client's declaration that two submits are one
	// request, with the same discipline placement uses: the transaction
	// identifier is derived from it, so a replay collides with the row it
	// already wrote instead of paying twice.
	IdempotencyKey string

	// AcceptedValue is the value the customer was SHOWN and agreed to. The
	// implementation re-prices while holding the wager row and refuses when the
	// number has changed — taking a cash-out at whatever the price happened to
	// be when the request landed is the same defect as booking a bet at a moved
	// line.
	AcceptedValue domain.Money

	Audit AuditContext
}

// TicketPricer prices a whole ticket from the legs it would be booked at.
//
// The slip-quote endpoint needs a ticket price and MUST NOT compute one. A
// parlay's price is not the product of its legs in general — same-game legs
// carry a correlation adjustment, a teaser's price is a posted ladder unrelated
// to the underlying prices — so multiplying decimals here would be a second
// implementation of the one number that gets frozen onto a ticket, and the two
// would eventually disagree in the direction nobody audits.
//
// The composition root passes the SAME pricer to this port and to the placement
// service, so a slip that quotes at X is placed at X or is refused for having
// moved. Passing two different pricers would make the quote a polite fiction.
//
// betting.IndependentPricer satisfies this, and refuses the two shapes it
// cannot price correctly rather than approximating them.
type TicketPricer interface {
	TicketDecimal(ctx context.Context, t betting.Ticket) (float64, error)
}
