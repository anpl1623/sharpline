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
package httpapi

import (
	"context"
	"net/netip"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
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

// Accounts reads the authenticated user's profile.
type Accounts interface {
	Profile(ctx context.Context, user domain.UserID) (Profile, error)
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
