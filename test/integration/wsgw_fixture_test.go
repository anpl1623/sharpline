package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/ingest/provider/synthetic"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
	"github.com/anpl1623/sharpline/internal/platform/redis"
	"github.com/anpl1623/sharpline/internal/pricing"
	"github.com/anpl1623/sharpline/internal/wsgw"
	"github.com/anpl1623/sharpline/internal/wsgw/redispresence"
	"github.com/anpl1623/sharpline/pkg/client"
)

// The WebSocket gateway's integration fixture: a real Kafka, a real Redis, two
// real hub replicas, and market documents that came out of the real pipeline.
//
// # Where the fixture data comes from
//
// Nothing in this file writes a price, a market document or a JSON payload by
// hand. Every ComputedMarket a client receives here was produced by this chain,
// with no step stubbed:
//
//	internal/ingest/provider/synthetic   the stochastic market maker, seeded, on
//	                                     a clock this fixture advances, producing
//	                                     provider.RawPayload bytes
//	kafka.OddsProducer.PublishRaw        onto a PRIVATE odds.raw.{provider} topic
//	internal/ingest/normalizer           the real Normalizer, running as a real
//	                                     kafka.Consumer, mapping and change-
//	                                     detecting onto odds.normalized
//	kafka.Snapshotter                    reads its output back off the compacted
//	                                     topic — the same read the pricer's warm
//	                                     start performs
//	internal/pricing.Engine.Price        the real devig, fair value, EV, Kelly,
//	                                     arbitrage and middles pass
//	kafka.OddsProducer.PublishPrice      onto price.computed, with the same
//	                                     envelope pricing.Service publishes
//
// The one thing this fixture does that pricing.Service would otherwise do is
// call the engine and the producer itself, rather than running the Service's
// consume loop. That is deliberate and it is the only way the tests below can
// exist: several of them turn on publishing an exact record at an exact moment —
// a second generation of one market, a tombstone, four hundred deltas at a
// stalled reader — and a consume loop driven by another topic cannot be asked
// for that. The engine call and the publish are copied from
// pricing.Service.HandleMessage line for line, including the envelope's Type,
// ID and ObservedAt, so what lands on price.computed is what the pricer lands.
//
// # Isolation on shared topics
//
// price.computed and odds.normalized are the DECLARED names, because
// PublishPrice and PublishNormalized are bound to them at compile time — the
// type-safety property internal/platform/kafka exists to have. So isolation is
// by KEY, exactly as kafka_fixture_test.go describes: every slate mints a unique
// provider slug, and normalizer's identity scheme namespaces every derived
// event id, market id and league slug under it. Two tests therefore cannot share
// a channel, and every assertion below is scoped to channels only its own slate
// can reach.
//
// A hub folds the WHOLE compacted topic, so it also sees the records other tests
// wrote. Those fail pricing.ComputedMarket.Validate or carry an envelope type
// this build does not read, and internal/wsgw counts and skips them — which is
// the behaviour its own errors.go argues for, and which this suite therefore
// exercises for free.
//
// # Failure, not skip
//
// These tests FAIL when Docker is unreachable. A silently skipped integration
// test reports green while proving nothing.

// -----------------------------------------------------------------------------
// The Redis container
// -----------------------------------------------------------------------------

// wsRedisImage is deploy/compose/compose.yaml's `redis` service, pinned by the
// same digest that internal/wsgw/redispresence's own integration test uses.
// Testing against a different build than the one that will run is the failure
// this pin exists to prevent.
const wsRedisImage = "redis:7-alpine@sha256:e7723ff73d963f5cc6d9c4643ea3d989527a402a319239054e9472a7fb9219a2"

// The shared Redis, started at most once and only when something asks for it.
//
// Deliberately NOT started from TestMain, for the reason kafkaCluster gives
// about the broker: TestMain belongs to the Postgres fixture, and every test in
// this package would otherwise pay for a Redis boot including the two dozen that
// only touch the database.
var (
	wsRedisOnce sync.Once
	wsRedisAddr string
	wsRedisErr  error
)

// wsRedisAddress returns the shared Redis endpoint, failing the calling test if
// it could not be started.
func wsRedisAddress(t *testing.T) string {
	t.Helper()

	wsRedisOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), containerStartDeadline)
		defer cancel()

		req := testcontainers.ContainerRequest{
			Image:        wsRedisImage,
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor: wait.ForLog("Ready to accept connections").
				WithStartupTimeout(containerStartDeadline),
		}
		container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: req,
			Started:          true,
		})
		if err != nil {
			wsRedisErr = fmt.Errorf("start redis: %w", err)
			return
		}
		host, err := container.Host(ctx)
		if err != nil {
			wsRedisErr = fmt.Errorf("redis container host: %w", err)
			return
		}
		port, err := container.MappedPort(ctx, "6379/tcp")
		if err != nil {
			wsRedisErr = fmt.Errorf("redis container port: %w", err)
			return
		}
		wsRedisAddr = host + ":" + port.Port()
	})

	if wsRedisErr != nil {
		t.Fatalf("the shared Redis is unavailable, so nothing in this file can run: %v", wsRedisErr)
	}
	return wsRedisAddr
}

// wsRedisClient opens a client of this replica's own against the shared server.
//
// One client per gateway node rather than one shared by both, because the thing
// under test is two POD-shaped things reaching one Redis: a shared connection
// pool would quietly make the two replicas one process in the only dimension
// that matters.
func wsRedisClient(t *testing.T, name string) *redis.Client {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	c, err := redis.Connect(ctx, redis.Options{
		Addr:    wsRedisAddress(t),
		Service: name,
		Logger:  testLogger(t),
	})
	if err != nil {
		t.Fatalf("connect redis as %s: %v", name, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// -----------------------------------------------------------------------------
// The slate: one test's private stretch of the real pipeline
// -----------------------------------------------------------------------------

// wsSlateOptions tunes how much board a slate produces. The defaults are small
// on purpose: the assertions below are about routing, sequencing and
// backpressure, and a full five-day slate of four leagues would spend the whole
// budget of this suite producing markets nothing asserts on.
type wsSlateOptions struct {
	// SlateDays and EventsPerDay size the synthetic fixture grid.
	SlateDays    int
	EventsPerDay int
	// Markets are the market types fetched. Fewer types means fewer markets per
	// event, which is what keeps a snapshot assertion legible.
	Markets []domain.MarketType
}

// slate is one test's private stretch of the pipeline, from the market maker to
// price.computed.
type slate struct {
	provider kafka.Provider
	rawTopic string
	prefix   string

	adapter *synthetic.Adapter
	scope   provider.Scope
	engine  *pricing.Engine

	bus      busOptions
	producer *kafka.OddsProducer

	// mu guards now, which the fetch reads and the test advances.
	mu  sync.Mutex
	now time.Time
}

// pricedMarket is one market that travelled the whole chain, with both the
// document that goes on the wire and the normalizer record it was computed from.
type pricedMarket struct {
	ID       domain.MarketID
	Computed pricing.ComputedMarket
	Source   normalizer.NormalizedMarket
}

// The three channels this market is published on, in internal/wsgw's own order.
func (m pricedMarket) marketChannel() string { return "market:" + m.Computed.Market.ID }
func (m pricedMarket) eventChannel() string  { return "event:" + m.Computed.Event.ID }
func (m pricedMarket) leagueChannel() string { return "league:" + m.Computed.League.Slug }

// newSlate wires the ingest half of the pipeline for one test and leaves it
// running.
//
// The synthetic adapter's clock is INJECTED and starts at the wall clock. Both
// halves of that matter: injected, because the market maker advances with time
// rather than with polls, so a second generation of a market requires moving the
// clock and not polling twice; and starting at now, because every price's
// observation instant becomes a staleness observation, and a fixture anchored in
// 2001 would put the headline SLO's histogram in a bucket no real deployment can
// reach.
func newSlate(t *testing.T, bus busOptions, opts wsSlateOptions) *slate {
	t.Helper()

	declaredKafkaTopics(t)

	// Two days forward and two fixtures a day is the smallest grid that is
	// guaranteed to have contests on it at every hour of the wall clock. The
	// generator drops a fixture once it is more than liveDuration+endedGrace old,
	// so a one-day, one-fixture grid produces an EMPTY board for most of the day
	// — which reads as a broken fixture rather than as a configuration too small
	// to work.
	if opts.SlateDays == 0 {
		opts.SlateDays = 2
	}
	if opts.EventsPerDay == 0 {
		opts.EventsPerDay = 2
	}
	if len(opts.Markets) == 0 {
		opts.Markets = []domain.MarketType{domain.MarketTypeMoneyline, domain.MarketTypeTotal}
	}

	prov, rawTopic := newRawTopic(t, 1)

	s := &slate{
		provider: prov,
		rawTopic: rawTopic,
		// Every identifier the normalizer derives is namespaced by the provider
		// slug (internal/ingest/normalizer/identity.go), so this prefix selects
		// exactly this test's records out of a shared compacted topic.
		prefix: prov.String() + ".",
		bus:    bus,
		now:    time.Now().UTC(),
	}

	adapter, err := synthetic.New(synthetic.Options{
		// A fixed seed, so a failure here is reproducible: the same universe,
		// the same latent paths and the same steam moves on every run.
		Seed:                  20260819,
		Clock:                 s.clock,
		SlateDays:             opts.SlateDays,
		EventsPerLeaguePerDay: opts.EventsPerDay,
	})
	if err != nil {
		t.Fatalf("build the synthetic provider: %v", err)
	}
	s.adapter = adapter

	catCtx, catCancel := context.WithTimeout(t.Context(), 30*time.Second)
	catalogue, err := adapter.Catalogue(catCtx)
	catCancel()
	if err != nil {
		t.Fatalf("read the synthetic catalogue: %v", err)
	}
	if len(catalogue.Leagues) == 0 {
		t.Fatal("the synthetic catalogue offers no leagues")
	}
	s.scope = provider.Scope{League: catalogue.Leagues[0].ID(), Markets: opts.Markets}

	engine, err := pricing.NewEngine(pricing.Options{})
	if err != nil {
		t.Fatalf("build the pricing engine: %v", err)
	}
	s.engine = engine

	s.producer = newOddsProducer(t, bus)

	// The REAL normalizer, running as a REAL consumer group on this slate's own
	// raw topic. Its group is unique to the test, so its offsets and its
	// rebalances are its own.
	norm, err := normalizer.New(normalizer.Options{
		Provider: prov,
		Decoder:  mustNeutralDecoder(t, prov),
		Producer: s.producer,
		// The warm start reads odds.normalized. It is REQUIRED by the
		// normalizer and it is not weakened here: change detection must survive
		// a restart or the first poll after every deploy republishes the whole
		// board.
		Snapshotter: newSnapshotter(t, bus, kafka.TopicOddsNormalized),
		Logger:      testLogger(t),
		// The provider slug namespaces the derived SLUGS as well as the derived
		// identifiers, so this test's league channel cannot collide with
		// another's on the shared topic.
		SlugNamespace: prov.String(),
		Clock:         s.clock,
	})
	if err != nil {
		t.Fatalf("build the normalizer: %v", err)
	}

	startMember(t, bus, "normalizer-"+prov.String(), func(o *kafka.ConsumerOptions) {
		o.Group = uniqueID("wsgw-norm")
		o.Topics = []string{rawTopic}
		// A record this normalizer cannot map is skipped rather than fatal: it
		// is what a cmd/ entrypoint configures, and stopping the group would
		// wedge the fixture on one unmappable event.
		o.ErrorPolicy = kafka.ErrorPolicySkip
		o.DisableLagExport = true
	}, norm)

	return s
}

// clock is the adapter's and the normalizer's only source of time.
func (s *slate) clock() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

// advance moves the simulated present forward. The market maker's state is a
// function of time rather than of polls, so this — and only this — is what makes
// the next fetch a genuine line movement instead of the same board again.
func (s *slate) advance(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = s.now.Add(d)
}

// mustNeutralDecoder builds the syntax layer for the synthetic adapter's
// payloads. It is the shipped decoder, not a test one: the synthetic adapter
// emits the neutral shape precisely so the real normalizer can read it.
func mustNeutralDecoder(t *testing.T, prov kafka.Provider) *normalizer.NeutralDecoder {
	t.Helper()
	d, err := normalizer.NewNeutralDecoder(prov)
	if err != nil {
		t.Fatalf("build the neutral decoder for %s: %v", prov, err)
	}
	return d
}

// poll fetches the current board and publishes every event's raw payload,
// exactly as internal/ingest does.
func (s *slate) poll(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	snapshot, err := s.adapter.Fetch(ctx, s.scope)
	if err != nil {
		t.Fatalf("fetch the synthetic board: %v", err)
	}
	if len(snapshot.Events) == 0 {
		t.Fatal("the synthetic board is empty; the fixture grid produced no events")
	}

	for _, ev := range snapshot.Events {
		if ev.Raw.IsZero() {
			continue
		}
		// The envelope is ingest.go's, field for field. ObservedAt is the
		// PROVIDER's instant and never the moment the bytes were received: it is
		// the subtrahend in every staleness measurement downstream, and stamping
		// it with a clock reading here would make the SLO report perfect health
		// for data that has none.
		if err := s.producer.PublishRaw(ctx, s.provider, ev.Event.ID(), kafka.Message{
			Type:       normalizer.RawMessageType,
			ID:         ev.Event.ID().String(),
			ObservedAt: ev.Raw.ObservedAt,
			Payload:    json.RawMessage(ev.Raw.Body),
		}); err != nil {
			t.Fatalf("publish the raw payload for %s: %v", ev.Event.ID(), err)
		}
	}
}

// normalized reads this slate's records back off odds.normalized.
//
// It is a Snapshotter read — the same read the pricer's warm start performs —
// rather than a second consumer group, because what is wanted is the CURRENT
// state of each key and not the history: the topic is compacted and
// last-write-wins per key is exactly the question being asked.
func (s *slate) normalized(t *testing.T) map[string]normalizer.NormalizedMarket {
	t.Helper()

	snap := newSnapshotter(t, s.bus, kafka.TopicOddsNormalized)
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	out := make(map[string]normalizer.NormalizedMarket)
	_, err := snap.Read(ctx, func(_ context.Context, d *kafka.Delivery) error {
		if d.Tombstone || !strings.HasPrefix(d.Key, s.prefix) {
			return nil
		}
		if d.Envelope.Type != normalizer.MessageType {
			return nil
		}
		var rec normalizer.NormalizedMarket
		if err := d.Unmarshal(&rec); err != nil {
			// Another test's record under this prefix is impossible, so this can
			// only be a genuine decode failure and it is worth failing on.
			return fmt.Errorf("decode %s: %w", d.Key, err)
		}
		out[d.Key] = rec
		return nil
	})
	if err != nil {
		t.Fatalf("read odds.normalized: %v", err)
	}
	return out
}

// generation polls, waits for the normalizer to publish, and prices what came
// out.
//
// `after` is the previous generation keyed by market id, or nil for the first.
// A record is accepted only when its observation instant is strictly newer than
// the one held before, which is what makes "the line moved" a property of the
// data rather than of the timing: the normalizer suppresses a market whose
// fingerprint has not changed, so a generation that produced no movement
// produces no record and this loop keeps waiting rather than returning the
// previous board a second time.
func (s *slate) generation(t *testing.T, want int, after map[string]pricedMarket) []pricedMarket {
	t.Helper()

	s.poll(t)

	var fresh map[string]normalizer.NormalizedMarket
	awaitTrue(t, 120*time.Second,
		fmt.Sprintf("the normalizer published at least %d moved markets under %s", want, s.prefix),
		func() bool {
			fresh = nil
			candidates := make(map[string]normalizer.NormalizedMarket)
			for key, rec := range s.normalized(t) {
				if prev, held := after[key]; held && !rec.ObservedAt.After(prev.Source.ObservedAt) {
					continue
				}
				candidates[key] = rec
			}
			if len(candidates) < want {
				return false
			}
			fresh = candidates
			return true
		})

	priced := make([]pricedMarket, 0, len(fresh))
	for key, rec := range fresh {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		computed, err := s.engine.Price(ctx, rec)
		cancel()
		if err != nil {
			// An engine failure is permanent by contract — nothing in the odds
			// mathematics does I/O — so a market this engine will not price is
			// one this fixture drops rather than retries.
			t.Logf("the engine declined to price %s: %v", key, err)
			continue
		}
		id, err := computed.MarketID()
		if err != nil {
			t.Fatalf("the priced document for %s has no usable market id: %v", key, err)
		}
		priced = append(priced, pricedMarket{ID: id, Computed: computed, Source: rec})
	}
	if len(priced) < want {
		t.Fatalf("the engine priced %d markets, want at least %d", len(priced), want)
	}

	// A stable order, so a failure message names the same market on every run.
	sort.Slice(priced, func(i, j int) bool { return priced[i].ID < priced[j].ID })
	return priced
}

// publish puts one priced market on price.computed with the envelope
// pricing.Service publishes.
//
// SYNCHRONOUS, as the Service's own publish is: returning before the
// acknowledgement would let a test assert on a record that never reached the
// broker.
func (s *slate) publish(t *testing.T, m pricedMarket) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	if err := s.producer.PublishPrice(ctx, m.ID, kafka.Message{
		Type:       pricing.MessageType,
		ID:         m.Computed.SourceFingerprint,
		ObservedAt: m.Computed.ObservedAt,
		Payload:    m.Computed,
	}); err != nil {
		t.Fatalf("publish the priced market %s: %v", m.ID, err)
	}
}

// tombstone deletes one market from the compacted topic, the way
// pricing.Service propagates a deletion from odds.normalized.
func (s *slate) tombstone(t *testing.T, m pricedMarket) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	if err := s.producer.TombstonePrice(ctx, m.ID, kafka.Tombstone{
		Reason:      "integration test: source market was tombstoned on odds.normalized",
		Acknowledge: kafka.AcknowledgeDeletesKeyFromSnapshot,
		ObservedAt:  m.Computed.ObservedAt.Add(time.Second),
	}); err != nil {
		t.Fatalf("tombstone the priced market %s: %v", m.ID, err)
	}
}

// -----------------------------------------------------------------------------
// Gateway nodes
// -----------------------------------------------------------------------------

// wsTestSigningKey is the HMAC secret the fixture's token issuer uses.
//
// It is a test constant, not a credential: it signs tokens for a broker and a
// Redis that exist for the length of one test session. It is at least
// auth.MinSigningKeyLen bytes because the real issuer refuses anything shorter,
// and using the real issuer is the point — there must not be a second token
// verifier in this repository, so the gateway under test is handed the same
// pinned-algorithm verifier cmd/api adapts.
const wsTestSigningKey = "integration-test-signing-key-do-not-use-anywhere-else-0123456789"

// gatewayNode is one `stream` replica: a bus follower, a hub, a Redis presence
// store and an HTTP listener, wired the way cmd/stream wires them.
type gatewayNode struct {
	replica  string
	hub      *wsgw.Hub
	follower *kafka.Follower
	server   *httptest.Server
	registry *prometheus.Registry
	issuer   *auth.TokenIssuer
}

// gatewayOptions tunes one node.
type gatewayOptions struct {
	// SendQueueCapacity is the per-connection bounded queue. Zero means the
	// package default.
	SendQueueCapacity int
	// Redis, when false, builds the node with no durable presence store, which
	// is the degraded configuration internal/wsgw documents as still correct.
	Redis bool
}

// newGatewayNode brings up one replica against the shared broker and the shared
// Redis, and waits until it holds the complete slate.
//
// It waits on [wsgw.Hub.Check] rather than on a sleep, because that is the
// readiness gate the Kubernetes probe uses: a replica whose fold is incomplete
// would answer a snapshot with a board that is silently missing markets, which
// internal/wsgw argues is worse than answering nothing.
func newGatewayNode(t *testing.T, bus busOptions, opts gatewayOptions) *gatewayNode {
	t.Helper()

	declaredKafkaTopics(t)

	replica := uniqueID("stream")
	registry := prometheus.NewRegistry()

	metrics, err := wsgw.NewMetrics(registry)
	if err != nil {
		t.Fatalf("build the wsgw metrics: %v", err)
	}

	issuer, err := auth.NewTokenIssuer(auth.TokenIssuerOptions{SigningKey: []byte(wsTestSigningKey)})
	if err != nil {
		t.Fatalf("build the token issuer: %v", err)
	}

	openCtx, openCancel := context.WithTimeout(t.Context(), 90*time.Second)
	follower, err := kafka.NewFollower(openCtx, kafka.FollowerOptions{
		ClientOptions: bus.ClientOptions,
		Topic:         kafka.TopicPriceComputed,
		// After catch-up a record this build cannot fold is skipped. The hub
		// never returns an error anyway; this makes the follower's own decode
		// failures non-fatal too, which is what a shared topic carrying other
		// tests' records requires.
		ErrorPolicy:    kafka.ErrorPolicySkip,
		CatchUpTimeout: 120 * time.Second,
	})
	openCancel()
	if err != nil {
		t.Fatalf("open the price.computed follower: %v", err)
	}

	var presence wsgw.Presence
	if opts.Redis {
		store, perr := redispresence.New(redispresence.Options{
			Client:   wsRedisClient(t, "wsgw-it-"+replica),
			Logger:   testLogger(t),
			Replica:  replica,
			Registry: registry,
		})
		if perr != nil {
			t.Fatalf("build the redis presence store: %v", perr)
		}
		presence = store
	}

	hub, err := wsgw.NewHub(wsgw.HubOptions{
		Options: wsgw.Options{
			Logger:  testLogger(t),
			Metrics: metrics,
			// The SAME pinned-algorithm verifier cmd/api adapts, in one line,
			// exactly as cmd/api's newAuthenticator does it. A second verifier
			// anywhere in the tree would be a second place for algorithm
			// confusion to be subtly wrong, and the second one is always the one
			// nobody reviews.
			Verifier: wsgw.TokenVerifierFunc(func(_ context.Context, token string) (wsgw.Identity, error) {
				claims, verr := issuer.Verify(auth.NewSecret(token))
				if verr != nil {
					return wsgw.Identity{}, verr
				}
				return wsgw.Identity{
					UserID:    claims.Subject,
					SessionID: claims.SessionID,
					ExpiresAt: claims.ExpiresAt,
				}, nil
			}),
			ReplicaID:         replica,
			SendQueueCapacity: opts.SendQueueCapacity,
		},
		Source:   follower,
		Presence: presence,
	})
	if err != nil {
		t.Fatalf("build the hub: %v", err)
	}

	runCtx, stopRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- hub.Run(runCtx) }()

	srv, err := wsgw.NewServer(wsgw.ServerOptions{Hub: hub})
	if err != nil {
		t.Fatalf("build the upgrade handler: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle(wsgw.Route, srv)
	listener := httptest.NewServer(mux)

	node := &gatewayNode{
		replica:  replica,
		hub:      hub,
		follower: follower,
		server:   listener,
		registry: registry,
		issuer:   issuer,
	}

	t.Cleanup(func() {
		listener.Close()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = hub.Shutdown(shutdownCtx)
		cancel()

		stopRun()
		select {
		case <-runDone:
		case <-time.After(60 * time.Second):
			t.Errorf("%s: Hub.Run did not return within 60s of its context being cancelled", replica)
		}
		_ = follower.Close()
	})

	awaitTrue(t, 150*time.Second, replica+" caught up with price.computed", func() bool {
		return hub.Check(context.Background()) == nil
	})
	return node
}

// wsURL is the address a client dials.
func (n *gatewayNode) wsURL() string {
	return "ws" + strings.TrimPrefix(n.server.URL, "http") + wsgw.Route
}

// token mints an access token for a user and a session, through the real issuer.
//
// The session id is what a durable subscription set is keyed by: two connections
// presenting tokens from the same login family land on the same Redis key, on
// whichever replica they reach, which is the whole of CLAUDE.md §9's
// affinity-free routing.
func (n *gatewayNode) token(t *testing.T, user domain.UserID, session string) string {
	t.Helper()
	secret, _, err := n.issuer.Issue(user, session)
	if err != nil {
		t.Fatalf("issue an access token: %v", err)
	}
	return secret.Expose()
}

// -----------------------------------------------------------------------------
// Clients
// -----------------------------------------------------------------------------

// dialStream opens a pkg/client stream against one node.
//
// The SDK is used rather than a raw socket deliberately: it is the OTHER
// deliverable of this phase, and driving it against the real gateway is what
// checks that its re-declaration of the wire protocol has not drifted from
// internal/wsgw's.
func dialStream(t *testing.T, node *gatewayNode, opts client.StreamOptions) *client.Stream {
	t.Helper()

	sdk, err := client.New(client.Options{BaseURL: node.server.URL})
	if err != nil {
		t.Fatalf("build the SDK client: %v", err)
	}
	if opts.URL == "" {
		opts.URL = node.wsURL()
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	s, err := sdk.Stream(ctx, opts)
	if err != nil {
		t.Fatalf("open a stream against %s: %v", node.replica, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// awaitStreamEvent reads events until one of the wanted kind arrives, failing
// with what it saw instead.
func awaitStreamEvent(t *testing.T, s *client.Stream, want client.StreamEventKind, within time.Duration) client.StreamEvent {
	t.Helper()

	deadline := time.Now().Add(within)
	var seen []string
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithDeadline(t.Context(), deadline)
		ev, err := s.Next(ctx)
		cancel()
		if err != nil {
			t.Fatalf("waiting for a %s event (saw %v): %v", want, seen, err)
		}
		if ev.Kind == want {
			return ev
		}
		seen = append(seen, string(ev.Kind))
	}
	t.Fatalf("no %s event arrived within %s (saw %v)", want, within, seen)
	return client.StreamEvent{}
}

// awaitSnapshot reads until the snapshot for one channel arrives.
func awaitSnapshot(t *testing.T, s *client.Stream, channel string, within time.Duration) client.StreamEvent {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithDeadline(t.Context(), deadline)
		ev, err := s.Next(ctx)
		cancel()
		if err != nil {
			t.Fatalf("waiting for the %s snapshot: %v", channel, err)
		}
		if ev.Kind == client.StreamEventSnapshot && ev.Channel == channel {
			return ev
		}
	}
	t.Fatalf("no snapshot for %s arrived within %s", channel, within)
	return client.StreamEvent{}
}

// snapshotMarketIDs decodes the market ids out of a snapshot's documents.
//
// It decodes ONLY the identifier, from the document the gateway carried through
// byte for byte. The point of the assertion is that the right markets arrived,
// and re-deriving anything else from these bytes would be this test doing the
// second mapping internal/wsgw refuses to do.
func snapshotMarketIDs(t *testing.T, ev client.StreamEvent) []string {
	t.Helper()

	ids := make([]string, 0, len(ev.Markets))
	for _, raw := range ev.Markets {
		var doc struct {
			Market struct {
				ID string `json:"id"`
			} `json:"market"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("a snapshot document is not a computed market: %v", err)
		}
		ids = append(ids, doc.Market.ID)
	}
	sort.Strings(ids)
	return ids
}

// marketIDsOf renders a set of priced markets as sorted identifiers.
func marketIDsOf(markets []pricedMarket) []string {
	ids := make([]string, 0, len(markets))
	for _, m := range markets {
		ids = append(ids, m.ID.String())
	}
	sort.Strings(ids)
	return ids
}

// marketsOnEvent selects the markets belonging to one event.
func marketsOnEvent(markets []pricedMarket, eventID string) []pricedMarket {
	out := make([]pricedMarket, 0, len(markets))
	for _, m := range markets {
		if m.Computed.Event.ID == eventID {
			out = append(out, m)
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// Metric assertions
// -----------------------------------------------------------------------------

// histogramCount returns the observation count of a histogram with at least the
// given labels, and whether the series exists.
//
// counterValue in metrics_test.go handles a counter and a gauge; a histogram is
// a third shape and the headline SLO is one, so this exists rather than the
// assertion being weakened to something counterValue can already read.
func histogramCount(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) (uint64, bool) {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	var total uint64
	found := false
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if !labelsMatch(metric, labels) {
				continue
			}
			h := metric.GetHistogram()
			if h == nil {
				t.Fatalf("%s is not a histogram", name)
			}
			total += h.GetSampleCount()
			found = true
		}
	}
	return total, found
}

// wsCounter reads one counter off a node's private registry. A series that has
// never been touched reads as zero, which is the same thing for every assertion
// here.
func wsCounter(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	v, _ := counterValue(t, reg, name, labels)
	return v
}

// describeHistograms renders every histogram series on a registry, so a failed
// SLO assertion says what IS there.
func describeHistograms(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		return fmt.Sprintf("  (gather failed: %v)", err)
	}
	var lines []string
	for _, family := range families {
		if family.GetType() != dto.MetricType_HISTOGRAM {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			lines = append(lines, fmt.Sprintf("  %s%s count=%d",
				family.GetName(), formatLabels(labels), metric.GetHistogram().GetSampleCount()))
		}
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return "  (no histograms)"
	}
	return strings.Join(lines, "\n")
}

// generationFor waits until ONE named market has moved, and returns its priced
// form.
//
// It re-polls with the clock advanced rather than waiting, because the market
// maker's state is a function of time: a market that has not moved will not move
// on its own, and the normalizer suppresses a republication of an unchanged
// fingerprint, so waiting longer would wait for ever.
func (s *slate) generationFor(t *testing.T, id domain.MarketID, after map[string]pricedMarket) pricedMarket {
	t.Helper()

	deadline := time.Now().Add(4 * time.Minute)
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		for _, m := range s.generation(t, 1, after) {
			if m.ID == id {
				return m
			}
		}
		t.Logf("market %s had not moved after %d polls; advancing the simulated clock", id, attempt)
		s.advance(90 * time.Second)
	}
	t.Fatalf("market %s never moved", id)
	return pricedMarket{}
}

// awaitMarkets blocks until this replica's slate holds exactly `want` markets on
// a channel.
//
// It reads the hub's own store rather than a client's snapshot, because it
// answers a different question: "has the fold caught up" is about the replica,
// and asking a client would conflate it with "did the frame arrive". [wsgw.Hub.Store]
// exists for exactly this and for the operational surface.
func (n *gatewayNode) awaitMarkets(t *testing.T, channel string, want int) {
	t.Helper()

	ch, err := wsgw.ParseChannel(channel)
	if err != nil {
		t.Fatalf("%q is not a channel this build can parse: %v", channel, err)
	}
	awaitTrue(t, 90*time.Second,
		fmt.Sprintf("%s holds %d markets on %s", n.replica, want, channel),
		func() bool { return len(n.hub.Store().Snapshot(ch)) == want })
}

// assertComputedProvenance decodes a document the gateway delivered and requires
// it to be a valid pricing.ComputedMarket from the expected provider.
//
// This is the end-to-end statement that nothing in this suite fabricated a
// price: the bytes a client received parse as the pricer's own document, satisfy
// the document's own validator, and name the provider whose market maker
// produced them.
func assertComputedProvenance(t *testing.T, raw json.RawMessage, wantProvider string) {
	t.Helper()

	var doc pricing.ComputedMarket
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("a delivered document is not a computed market: %v", err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("a delivered document does not satisfy pricing.ComputedMarket.Validate: %v", err)
	}
	if doc.Provider != wantProvider {
		t.Errorf("delivered document names provider %q, want %q", doc.Provider, wantProvider)
	}
	if doc.SourceFingerprint == "" {
		t.Error("delivered document carries no source fingerprint; it is not attributable to a " +
			"normalizer record")
	}
	if len(doc.Books) == 0 {
		t.Error("delivered document quotes no book")
	}
}

// quoteCount is how many staleness observations one record produces: one per
// quote carrying an observation instant, which is the rule
// internal/wsgw/metrics.go applies.
func quoteCount(m pricedMarket) int {
	n := 0
	for _, b := range m.Computed.Books {
		for _, q := range b.Quotes {
			if !q.ObservedAt.IsZero() {
				n++
			}
		}
	}
	return n
}

// containsChannel reports whether a restored channel list names one channel.
func containsChannel(list []string, want string) bool {
	for _, c := range list {
		if c == want {
			return true
		}
	}
	return false
}

// busiestEvent returns the event id carrying the most markets, so a snapshot
// assertion has more than one document to be exact about.
func busiestEvent(markets []pricedMarket) string {
	counts := make(map[string]int, len(markets))
	for _, m := range markets {
		counts[m.Computed.Event.ID]++
	}
	best, bestN := "", -1
	for id, n := range counts {
		// Ties break on the identifier, so a failure names the same event twice
		// in a row.
		if n > bestN || (n == bestN && id < best) {
			best, bestN = id, n
		}
	}
	return best
}
