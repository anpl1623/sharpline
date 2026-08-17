package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// The Kafka half of the integration tier.
//
// CLAUDE.md §10 does not merely permit a real broker, it explains why one is
// mandatory: integration tests use "real Postgres/Redis/Kafka — no mocked
// databases, and no mocked broker either, BECAUSE THE INTERESTING BUGS LIVE IN
// CONSUMER-GROUP REBALANCING AND OFFSET HANDLING." Every one of those bugs is a
// property of the broker's group protocol and its log cleaner. A fake broker
// reproduces the API and none of the behaviour, so a suite built on one would be
// green and worthless.
//
// # What is under test is internal/platform/kafka, not franz-go
//
// This file's helpers build the SHIPPED types — OddsProducer, AuditProducer,
// Consumer, Snapshotter — and the tests drive those. That is a deliberate
// correction: the first version of this suite predated producer.go and
// consumer.go and therefore drove raw kgo clients with its own envelope,
// tombstone and trace-propagation helpers. It proved that Kafka works. It did
// not prove that the code which will actually run in production works, and the
// coverage report said so — the entire wrapper was at 34% with every function in
// producer.go and consumer.go at zero.
//
// Raw kgo and kadm survive here for exactly two jobs, neither of which the
// shipped package does: creating and describing topics (Terraform's job in
// production), and one deliberate CONTROL ARM that shows what closing a producer
// without flushing costs.
//
// # One broker, and how isolation is obtained on it
//
// A single KRaft container is started lazily on first use and shared, because a
// broker boot is ~5s and the alternative is paying it per test.
//
// Isolation has two shapes, because the shipped producer can only write to the
// topics the registry declares:
//
//   - CONSUMER-side tests get a private topic. `odds.raw.{provider}` is a
//     FAMILY, and NewProvider accepts any lowercase-alphanumeric slug, so
//     newRawTopic mints `odds.raw.it-000123` and publishes to it through the
//     real PublishRaw. A test's records and its consumer group are then its own.
//   - COMPACTED-topic tests must use the declared names, because
//     PublishNormalized writes to odds.normalized and there is no parameter that
//     changes that — which is the type-safety property this package exists to
//     have. Those tests therefore share odds.normalized / price.computed and
//     isolate by KEY: every key is minted from uniqueID, and every assertion is
//     scoped to the keys the test itself wrote.
//
// The earlier version of this file refused to create the declared names at all,
// on the grounds that "a test that shares Terraform's topics pollutes a live
// snapshot". That reasoning is about the COMPOSE cluster, and it still holds
// there. This broker is an ephemeral container that Terraform never touches and
// Ryuk reaps at the end of the session; there is no snapshot to pollute, and
// refusing the names would mean the shipped publish path stayed untested.
//
// # NO MOCK DATA
//
// Every record here is produced BY a test and asserted on by the same test.
// Nothing is pre-published, nothing is seeded, and no canned payload stands in
// for ingested data. An empty topic after `make up` is CORRECT; these tests
// never touch that cluster at all.
//
// # Failure, not skip
//
// These tests FAIL when the broker cannot be started. A silently skipped
// integration test reports green while proving nothing.

// kafkaImage is the compose stack's `kafka` image, pinned by digest, copied from
// the contract ledger. Kafka 4.x is KRaft-only, so there is no ZooKeeper here or
// anywhere. Testing against a different broker version than the one that will run
// would defeat the point of using a real one.
const kafkaImage = "apache/kafka:latest@sha256:77e3df9054047a88b520d0cc46e16696d3b22022e1d580aeccd2632df6532837"

// kafkaClusterID matches deploy/compose/compose.yaml's default. It is arbitrary
// but must be a valid base64 UUID, and reusing the compose value keeps one fewer
// magic string in the tree.
const kafkaClusterID = "5L6g3nShT-eMCtK--X86sw"

// starterScript is the path the post-start hook writes the resolved advertised
// listeners to. See startKafka for why this dance is necessary.
const starterScript = "/tmp/sharpline-kafka-start"

// brokerReadyDeadline bounds the application-level readiness loop that runs after
// the container reports its port open. The container wait strategy proves a
// socket exists; this proves the KRaft controller has elected and metadata is
// answerable, which is a different and later event.
const brokerReadyDeadline = 90 * time.Second

// itService is the Service name every bus client in this suite is built with. It
// becomes the Kafka client id and the `producer` field of every envelope, so it
// is what a failing assertion about provenance names.
const itService = "sharpline-it"

// cluster is one running Kafka container and how to reach it.
type cluster struct {
	container testcontainers.Container

	// bootstrap is host:port through the container's PUBLISHED port on the host,
	// reached via host.docker.internal — the same wiring the Postgres fixture
	// uses, and the reason TESTCONTAINERS_HOST_OVERRIDE is set in the Makefile.
	bootstrap string
}

// The shared broker, started at most once.
//
// It is deliberately NOT started from TestMain: TestMain belongs to the Postgres
// fixture and every test in this package would then pay for a Kafka boot,
// including the two dozen that only touch the database. sync.Once gives the same
// single-start guarantee without that coupling.
//
// Teardown is Ryuk's job. The Makefile runs with TESTCONTAINERS_RYUK_DISABLED
// defaulted to false, so the reaper container removes this broker when the test
// session's connection drops — which is the only mechanism available to a
// lazily-started shared fixture that no single test owns.
var (
	kafkaOnce    sync.Once
	sharedKafka  *cluster
	sharedKafkaE error
)

// kafkaCluster returns the package's Kafka broker, failing the calling test if it
// could not be started.
//
// It FAILS rather than skips. The whole point of the container mandate is that a
// contributor with nothing but Docker reaches a working system; a suite that
// quietly skips when Docker is unreachable would report green on a machine where
// nothing works.
func kafkaCluster(t *testing.T) *cluster {
	t.Helper()

	kafkaOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), containerStartDeadline+brokerReadyDeadline)
		defer cancel()
		sharedKafka, sharedKafkaE = startKafka(ctx)
	})

	if sharedKafkaE != nil {
		t.Fatalf("the shared Kafka broker is unavailable: %v", sharedKafkaE)
	}
	return sharedKafka
}

// startKafka boots a single-node KRaft broker and proves it answers metadata.
//
// # Why the advertised listeners need a post-start hook
//
// A Kafka client bootstraps against one address and is then TOLD, by the broker,
// which address to use for every subsequent request — that is what
// advertised.listeners is. The tests reach the broker through its published port
// on the host, and that port is EPHEMERAL: it is not known until the container is
// running. So the broker cannot be started with a correct advertised address, and
// a broker started with an incorrect one accepts the bootstrap connection and then
// redirects every real request to somewhere unreachable, which presents as a
// mysterious timeout rather than as a configuration error.
//
// The fix is the same one the upstream testcontainers Kafka module uses: hold the
// container in a shell loop, resolve the mapped port once it is running, write the
// resolved value into a file the loop is waiting for, and let it exec the image's
// own entrypoint. testcontainers runs user-defined post-start hooks BEFORE the
// default readiness hook, which is what makes the ordering work.
//
// # Two listeners, not one
//
// EXTERNAL carries client traffic and is advertised at the host address. BROKER
// carries inter-broker and coordinator traffic and is advertised at localhost
// inside the container, so that traffic never leaves the container and cannot be
// affected by the host port mapping.
func startKafka(ctx context.Context) (*cluster, error) {
	req := testcontainers.ContainerRequest{
		Image:        kafkaImage,
		ExposedPorts: []string{"9093/tcp"},
		Env: map[string]string{
			"KAFKA_NODE_ID":                        "1",
			"KAFKA_PROCESS_ROLES":                  "broker,controller",
			"KAFKA_LISTENERS":                      "BROKER://0.0.0.0:9092,CONTROLLER://0.0.0.0:9094,EXTERNAL://0.0.0.0:9093",
			"KAFKA_LISTENER_SECURITY_PROTOCOL_MAP": "BROKER:PLAINTEXT,CONTROLLER:PLAINTEXT,EXTERNAL:PLAINTEXT",
			"KAFKA_INTER_BROKER_LISTENER_NAME":     "BROKER",
			"KAFKA_CONTROLLER_LISTENER_NAMES":      "CONTROLLER",
			"KAFKA_CONTROLLER_QUORUM_VOTERS":       "1@localhost:9094",
			"KAFKA_LOG_DIRS":                       "/tmp/kraft-combined-logs",
			"CLUSTER_ID":                           kafkaClusterID,

			// Single broker: every replication factor is 1 and every min.isr is
			// 1. Anything else makes topic creation or every acks=all produce
			// fail, exactly as deploy/terraform/envs/local documents.
			"KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR":                 "1",
			"KAFKA_OFFSETS_TOPIC_NUM_PARTITIONS":                     "1",
			"KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR":         "1",
			"KAFKA_TRANSACTION_STATE_LOG_MIN_ISR":                    "1",
			"KAFKA_SHARE_COORDINATOR_STATE_TOPIC_REPLICATION_FACTOR": "1",
			"KAFKA_SHARE_COORDINATOR_STATE_TOPIC_MIN_ISR":            "1",

			// PARITY WITH PRODUCTION, and load-bearing. The compose broker runs
			// with auto-creation off (CLAUDE.md §9: topics are "created by
			// Terraform, not by hand"). A test broker that auto-created topics
			// would hide the one failure mode this setting exists to expose —
			// a producer publishing to a name Terraform never declared.
			"KAFKA_AUTO_CREATE_TOPICS_ENABLE": "false",
			"KAFKA_NUM_PARTITIONS":            "3",

			// ---- test-harness speed knobs ----------------------------------
			// These change HOW FAST the broker does things, never WHAT it does.
			// No semantic setting is relaxed: cleanup policy, retention and the
			// compaction thresholds are all topic-level and come from the
			// Terraform catalogue.
			//
			// The initial rebalance delay exists to let a whole consumer fleet
			// join before assigning; at 3s per rebalance it would dominate the
			// group tests and prove nothing.
			"KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS": "0",
			// Allows a consumer to ask for a short session timeout so a member
			// that stops heartbeating is evicted in seconds rather than in the
			// 45s default.
			"KAFKA_GROUP_MIN_SESSION_TIMEOUT_MS": "1000",
			// The log cleaner sleeps this long between passes when it finds
			// nothing to do. The default is 15s, which would make every
			// compaction assertion a minute-long wait.
			"KAFKA_LOG_CLEANER_BACKOFF_MS": "200",
			"KAFKA_LOG_CLEANER_ENABLE":     "true",
			"KAFKA_LOG_CLEANER_THREADS":    "1",

			// The deploy target is a 2 OCPU / 12 GB box shared with Postgres and
			// Redis (contract ledger). A test broker that grabs a default heap
			// while a TimescaleDB container is already running is how this VM
			// reached 100% memory before.
			"KAFKA_HEAP_OPTS": "-Xmx512m -Xms256m",
		},

		// The image's entrypoint is /__cacert_entrypoint.sh, which execs its
		// arguments. Replacing only the command therefore keeps the image's own
		// certificate setup and swaps just what it finally runs.
		Cmd: []string{
			"sh", "-c",
			"while [ ! -f " + starterScript + " ]; do sleep 0.1; done; " +
				". " + starterScript + "; exec /etc/kafka/docker/run",
		},

		LifecycleHooks: []testcontainers.ContainerLifecycleHooks{{
			PostStarts: []testcontainers.ContainerHook{writeAdvertisedListeners},
		}},

		WaitingFor: wait.ForListeningPort("9093/tcp").
			WithStartupTimeout(containerStartDeadline),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("start kafka container: %w", err)
	}

	bootstrap, err := brokerAddress(ctx, container)
	if err != nil {
		terminateContainer(container)
		return nil, err
	}

	c := &cluster{container: container, bootstrap: bootstrap}
	if err := c.awaitMetadata(ctx); err != nil {
		terminateContainer(container)
		return nil, err
	}
	return c, nil
}

// writeAdvertisedListeners is the post-start hook. It resolves the published
// address and releases the shell loop the container is parked in.
func writeAdvertisedListeners(ctx context.Context, c testcontainers.Container) error {
	address, err := brokerAddress(ctx, c)
	if err != nil {
		return err
	}

	line := fmt.Sprintf(
		"export KAFKA_ADVERTISED_LISTENERS='BROKER://localhost:9092,EXTERNAL://%s'\n", address)

	code, reader, err := c.Exec(ctx, []string{"sh", "-c", "printf %s " + shellQuote(line) + " > " + starterScript})
	if err != nil {
		return fmt.Errorf("write the kafka starter script: %w", err)
	}
	if code != 0 {
		out, _ := readAll(reader)
		return fmt.Errorf("writing the kafka starter script exited %d: %s", code, out)
	}
	return nil
}

// brokerAddress resolves the container's published EXTERNAL listener as
// host:port, from the perspective of the test container.
//
// The phase-2 handoff records that a container's ephemeral published port is
// REALLOCATED on every stop/start on this daemon, so this is always re-resolved
// and never cached beyond the life of the container it describes.
func brokerAddress(ctx context.Context, c testcontainers.Container) (string, error) {
	host, err := c.Host(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve kafka host: %w", err)
	}
	port, err := c.MappedPort(ctx, "9093/tcp")
	if err != nil {
		return "", fmt.Errorf("resolve kafka port: %w", err)
	}
	return host + ":" + port.Port(), nil
}

// awaitMetadata proves the broker answers, not merely that a socket is open.
//
// A KRaft listener binds before the controller has a quorum, so a port-open
// container is routinely a broker that answers metadata requests with an empty
// cluster. This is the same distinction internal/platform/kafka's awaitReady
// exists for, and the retry decision is made by that package's own classifier —
// so this loop doubles as a check that IsTransientClusterError recognises a
// genuinely starting broker rather than only the errors a unit test can fabricate.
func (c *cluster) awaitMetadata(ctx context.Context) error {
	deadline := time.Now().Add(brokerReadyDeadline)

	cl, err := kgo.NewClient(
		kgo.SeedBrokers(c.bootstrap),
		kgo.ClientID("sharpline-it-readiness"),
		kgo.DialTimeout(5*time.Second),
	)
	if err != nil {
		return fmt.Errorf("build the readiness client: %w", err)
	}
	defer cl.Close()

	var last error
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := cl.Ping(pingCtx)
		cancel()
		if err == nil {
			return nil
		}
		last = err

		if !kafka.IsTransientClusterError(err) {
			return fmt.Errorf("kafka at %s is unreachable and the failure is not transient "+
				"(attempt %d): %w", c.bootstrap, attempt, err)
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), last)
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("kafka at %s did not answer metadata within %s: %w",
		c.bootstrap, brokerReadyDeadline, last)
}

// terminateContainer removes a container, ignoring the error because there is
// nothing useful to do with it on a teardown path.
func terminateContainer(c testcontainers.Container) {
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = c.Terminate(ctx)
}

// shellQuote wraps s in single quotes for `sh -c`, escaping any it contains.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// readAll drains an exec's combined output.
func readAll(r interface{ Read([]byte) (int, error) }) (string, error) {
	if r == nil {
		return "", nil
	}
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String(), nil
		}
	}
}

// -----------------------------------------------------------------------------
// Admin plumbing — raw kadm, because topic creation is Terraform's job
// -----------------------------------------------------------------------------

// newKafkaClient builds a bare franz-go client for the ADMIN operations the
// shipped package does not perform: creating topics, describing their configs,
// listing offsets and reading group lag.
//
// It is deliberately not used to produce or consume anything a test asserts on.
// The one exception is the control arm in TestCloseFlushesWhatWasAcceptedButNotYetWritten,
// which says so at its call site.
func newKafkaClient(t *testing.T, extra ...kgo.Opt) *kgo.Client {
	t.Helper()

	opts := []kgo.Opt{
		kgo.SeedBrokers(kafkaCluster(t).bootstrap),
		kgo.ClientID("sharpline-it-admin"),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.DialTimeout(kafka.DefaultDialTimeout),
		kgo.MetadataMaxAge(kafka.DefaultMetadataMaxAge),
		kgo.MetadataMinAge(kafka.DefaultMetadataMinAge),
	}
	opts = append(opts, extra...)

	cl, err := kgo.NewClient(opts...)
	if err != nil {
		t.Fatalf("build kafka client: %v", err)
	}
	t.Cleanup(cl.Close)
	return cl
}

// newKafkaAdmin returns a kadm client over a fresh connection.
func newKafkaAdmin(t *testing.T) *kadm.Client {
	t.Helper()
	return kadm.NewClient(newKafkaClient(t))
}

// createKafkaTopic creates one topic and waits for every partition to report a
// leader.
//
// deleteAfter is false for the declared topics, which are shared by every test
// in the session and are reaped with the container.
func createKafkaTopic(t *testing.T, name string, partitions int32, config map[string]string, deleteAfter bool) {
	t.Helper()

	configs := make(map[string]*string, len(config))
	for k, v := range config {
		configs[k] = &v
	}

	adm := newKafkaAdmin(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	resp, err := adm.CreateTopic(ctx, partitions, 1, configs, name)
	if err != nil {
		t.Fatalf("create topic %s: %v", name, err)
	}
	if resp.Err != nil {
		t.Fatalf("create topic %s: %v", name, resp.Err)
	}

	if deleteAfter {
		// Deletion matters even on a throwaway broker: a compacted topic's log
		// cleaner keeps working after the test that created it finishes, and on a
		// 1-thread cleaner a pile of abandoned compacted topics is real contention
		// for the tests still running.
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _ = adm.DeleteTopics(ctx, name)
		})
	}

	// Topic creation is asynchronous from a producer's point of view: the
	// controller has accepted it, but the leader's metadata may not have
	// propagated to the client yet. Producing before then fails with
	// UNKNOWN_TOPIC_OR_PARTITION, which is indistinguishable from the real
	// misconfiguration it is supposed to signal.
	awaitTopicVisible(t, name, partitions)
}

// awaitTopicVisible blocks until every partition of the topic reports a leader.
func awaitTopicVisible(t *testing.T, topic string, partitions int32) {
	t.Helper()

	adm := newKafkaAdmin(t)
	deadline := time.Now().Add(30 * time.Second)

	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		details, err := adm.ListTopics(ctx, topic)
		cancel()
		if err != nil {
			last = err
		} else if d, ok := details[topic]; ok && d.Err == nil && int32(len(d.Partitions)) == partitions {
			ready := true
			for _, p := range d.Partitions {
				if p.Err != nil || p.Leader < 0 {
					ready = false
					break
				}
			}
			if ready {
				return
			}
			last = fmt.Errorf("partitions present but not all led")
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("topic %s did not become visible with %d led partitions: %v", topic, partitions, last)
}

// -----------------------------------------------------------------------------
// The declared topics, on the throwaway broker
// -----------------------------------------------------------------------------

// oddsNormalizedTestPartitions is 1 rather than the catalogue's 6, and the
// reason is compaction rather than convenience.
//
// The log cleaner works per PARTITION and never touches the ACTIVE segment, so
// proving a segment closes and then gets cleaned requires appending to the
// partition under test after its segment.ms has elapsed. With six partitions a
// roll record lands on one of them and the other five never close, so the
// assertion would be a coin toss dressed up as a test. Compaction is a
// per-partition property, so one partition proves it exactly as strongly — and
// per-market ordering, which is what the catalogue's 6 is arguing about, is
// satisfied trivially by one.
const oddsNormalizedTestPartitions = 1

// compactionSpeedups are the three timing knobs odds.normalized is created with
// on this broker, and NOTHING else is changed.
//
// The catalogue's real values put a segment roll an hour away and a minimum
// compaction lag a minute away, both deliberately: segment.ms=1h bounds how much
// dead tail a bootstrapping pod reads, and min.compaction.lag.ms=60s exists so
// that a consumer a few seconds behind cannot have an intermediate line movement
// deleted out from under it. Neither is a defect, neither is weakened in
// production, and neither can be waited out inside a test.
//
//	segment.ms              1h  -> 1s    (so a segment closes and becomes cleanable)
//	min.compaction.lag.ms   60s -> 0     (so a just-written record is eligible)
//	max.compaction.lag.ms   1h  -> 1s    (so cleaning is forced, not throughput-driven)
//
// min.cleanable.dirty.ratio keeps the catalogue's 0.1 on purpose: it is not a
// blocker at this scale and leaving it proves so. cleanup.policy,
// delete.retention.ms, compression and the size caps are the catalogue's.
//
// price.computed is created with the catalogue's values VERBATIM and is
// therefore the topic on which the cleaner provably has not run — which is what
// TestUncompactedTailStillNeedsTheFold needs in order to mean anything.
var compactionSpeedups = map[string]string{
	"segment.ms":            "1000",
	"min.compaction.lag.ms": "0",
	"max.compaction.lag.ms": "1000",
}

var declaredOnce sync.Once

// declaredKafkaTopics creates odds.normalized, price.computed and wager.events
// on the shared broker, once per session.
//
// They exist because the shipped producer's publish methods are bound to them at
// COMPILE TIME — PublishNormalized takes a domain.MarketID and writes to
// odds.normalized, and there is no parameter that changes either half. That is
// the guarantee producer.go was written to provide, and testing the real publish
// path therefore means creating the real names on this throwaway broker.
//
// Tests that share them isolate by key, never by topic.
func declaredKafkaTopics(t *testing.T) {
	t.Helper()

	kafkaCluster(t) // fail here, not inside the Once, if the broker is down

	declaredOnce.Do(func() {
		createKafkaTopic(t, kafka.TopicOddsNormalized, oddsNormalizedTestPartitions,
			copyConfig(terraformTopicConfig(t, kafka.TopicOddsNormalized),
				copyConfig(compactionSpeedups, map[string]string{"cleanup.policy": "compact"})),
			false)

		createKafkaTopic(t, kafka.TopicPriceComputed, 6,
			copyConfig(terraformTopicConfig(t, kafka.TopicPriceComputed),
				map[string]string{"cleanup.policy": "compact"}),
			false)

		createKafkaTopic(t, kafka.TopicWagerEvents, 3,
			copyConfig(terraformTopicConfig(t, kafka.TopicWagerEvents),
				map[string]string{"cleanup.policy": "delete"}),
			false)
	})
}

// newRawTopic mints a PRIVATE topic for one test and returns the provider slug
// that reaches it through the shipped producer.
//
// `odds.raw.{provider}` is a family, and kafka.NewProvider accepts any
// lowercase-alphanumeric slug with internal hyphens — the same charset
// Terraform's raw_providers validation enforces. uniqueID produces exactly that
// shape, so `odds.raw.it-000123` is a name the registry resolves to a
// retention-based topic keyed by EventID, reachable only through PublishRaw and
// owned by exactly one test.
//
// That is what makes every consumer-group assertion in this suite exact: a test
// that shared a topic could not claim "the group delivered N distinct records".
func newRawTopic(t *testing.T, partitions int32, overrides ...map[string]string) (kafka.Provider, string) {
	t.Helper()

	provider, err := kafka.NewProvider(uniqueID("it"))
	if err != nil {
		t.Fatalf("the generated provider slug is not one the registry accepts: %v", err)
	}
	topic, err := kafka.OddsRaw(provider)
	if err != nil {
		t.Fatalf("build the raw topic for %q: %v", provider, err)
	}

	config := copyConfig(terraformTopicConfig(t, "odds.raw"), map[string]string{"cleanup.policy": "delete"})
	for _, o := range overrides {
		config = copyConfig(config, o)
	}
	createKafkaTopic(t, topic.Name(), partitions, config, true)
	return provider, topic.Name()
}

// -----------------------------------------------------------------------------
// Building the shipped clients
// -----------------------------------------------------------------------------

// logSink captures a bus client's structured log so a test can assert on what it
// reported. It is concurrency-safe because franz-go's promises and the
// consumer's rebalance callbacks log from their own goroutines.
type logSink struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// busOptions is the ClientOptions every shipped client in this suite is built
// from, plus the registry and log its metrics and lines land in.
//
// Metrics get a PRIVATE registry per test: the collectors are process-wide
// counters, so a shared one would make "this operation incremented the error
// counter once" unassertable under t.Parallel().
type busOptions struct {
	kafka.ClientOptions

	registry *prometheus.Registry
	log      *logSink
}

func newBusOptions(t *testing.T) busOptions {
	t.Helper()

	reg := prometheus.NewRegistry()
	metrics, err := kafka.NewMetrics(reg)
	if err != nil {
		t.Fatalf("build the kafka metrics: %v", err)
	}

	sink := &logSink{}
	logger := slog.New(slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return busOptions{
		ClientOptions: kafka.ClientOptions{
			Brokers: []string{kafkaCluster(t).bootstrap},
			Service: itService,
			Logger:  logger,
			Metrics: metrics,
			// franz-go's own INFO logging is a line per metadata refresh and per
			// heartbeat outcome. It is forwarded at slog DEBUG, which this sink
			// records, so it is turned down to WARN to keep an assertion on the
			// package's own lines from searching a haystack.
			FranzLogLevel: kgo.LogLevelWarn,
		},
		registry: reg,
		log:      sink,
	}
}

// busMetric reads one series from a test's PRIVATE registry.
//
// It delegates to the package's existing counterValue, which handles a counter
// and a gauge alike and reports whether the series exists; a series that has
// never been touched reads as zero, which is the same thing for every assertion
// here.
//
// The registry being private per test is what makes these assertions possible at
// all: the collectors are process-wide, so a shared registry would make "this
// operation incremented the error counter once" unassertable under t.Parallel().
func busMetric(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	v, _ := counterValue(t, reg, name, labels)
	return v
}

// newOddsProducer opens the shipped low-latency producer against the shared
// broker.
//
// The startup probe is left ON. It is the behaviour a cmd/ entrypoint gets, and
// a test that skipped it would not notice a constructor that stopped proving
// connectivity.
func newOddsProducer(t *testing.T, bus busOptions, mutate ...func(*kafka.ProducerOptions)) *kafka.OddsProducer {
	t.Helper()

	opts := kafka.ProducerOptions{ClientOptions: bus.ClientOptions}
	for _, m := range mutate {
		m(&opts)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	p, err := kafka.NewOddsProducer(ctx, opts)
	if err != nil {
		t.Fatalf("NewOddsProducer: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// newAuditProducer opens the shipped never-give-up producer for wager.events.
func newAuditProducer(t *testing.T, bus busOptions, mutate ...func(*kafka.ProducerOptions)) *kafka.AuditProducer {
	t.Helper()

	opts := kafka.ProducerOptions{ClientOptions: bus.ClientOptions}
	for _, m := range mutate {
		m(&opts)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	p, err := kafka.NewAuditProducer(ctx, opts)
	if err != nil {
		t.Fatalf("NewAuditProducer: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// newSnapshotter opens the shipped snapshot reader.
func newSnapshotter(t *testing.T, bus busOptions, topic string, mutate ...func(*kafka.SnapshotOptions)) *kafka.Snapshotter {
	t.Helper()

	opts := kafka.SnapshotOptions{ClientOptions: bus.ClientOptions, Topic: topic}
	for _, m := range mutate {
		m(&opts)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	s, err := kafka.NewSnapshotter(ctx, opts)
	if err != nil {
		t.Fatalf("NewSnapshotter(%s): %v", topic, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// member is one running kafka.Consumer: the shipped type, its Run goroutine, and
// the handle a test uses to stop it and read what Run returned.
type member struct {
	*kafka.Consumer

	name string
	stop context.CancelFunc
	done chan error
}

// startMember opens a Consumer and runs it.
//
// Run BLOCKS, exactly as a cmd/ entrypoint uses it, so it goes in a goroutine
// and its return value is kept: for the error-policy tests the value Run returns
// IS the assertion.
func startMember(t *testing.T, bus busOptions, name string, mutate func(*kafka.ConsumerOptions), h kafka.Handler) *member {
	t.Helper()

	opts := kafka.ConsumerOptions{ClientOptions: bus.ClientOptions}
	mutate(&opts)

	// Run's context outlives t.Context() deliberately: a test that stops a member
	// and then asserts on what the group did must be able to control the stop
	// itself rather than have it happen during cleanup.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	openCtx, openCancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer openCancel()

	c, err := kafka.NewConsumer(openCtx, opts)
	if err != nil {
		cancel()
		t.Fatalf("NewConsumer(%s): %v", name, err)
	}

	m := &member{Consumer: c, name: name, stop: cancel, done: make(chan error, 1)}
	go func() { m.done <- c.Run(ctx, h) }()

	t.Cleanup(func() { _ = m.shutdown(t) })
	return m
}

// shutdown stops the member, waits for Run, and returns what Run returned.
//
// Cancel first, then Close: Run returns on the cancelled context and performs
// its final commit, and Close then sends LeaveGroup so the coordinator
// rebalances immediately instead of waiting out the session timeout.
func (m *member) shutdown(t *testing.T) error {
	t.Helper()

	m.stop()
	var runErr error
	select {
	case runErr = <-m.done:
		m.done <- runErr // keep it readable by a later close
	case <-time.After(60 * time.Second):
		t.Fatalf("%s: Run did not return within 60s of its context being cancelled", m.name)
	}
	if err := m.Close(); err != nil {
		t.Errorf("%s: Close: %v", m.name, err)
	}
	return runErr
}

// awaitRunError waits for Run to return and hands back its value.
func (m *member) awaitRunError(t *testing.T, within time.Duration) error {
	t.Helper()

	select {
	case err := <-m.done:
		m.done <- err
		return err
	case <-time.After(within):
		t.Fatalf("%s: Run had not returned after %s", m.name, within)
		return nil
	}
}

// awaitTrue polls until cond holds, failing with what after the deadline.
//
// Polling on an OUTCOME rather than sleeping for a duration is what keeps these
// tests deterministic: a rebalance, a commit and a compaction pass are all
// asynchronous, and a sleep long enough to be reliable is a sleep long enough to
// dominate the suite.
func awaitTrue(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: still false after %s", what, within)
}

// -----------------------------------------------------------------------------
// The Terraform catalogue, mirrored
// -----------------------------------------------------------------------------

// terraformCatalogue is the path to the file that OWNS the topic configuration.
// CLAUDE.md §9: Kafka topic configuration "gets created once with a CLI flag,
// forgotten, and then silently differs between laptop and cluster. Declaring it
// removes that failure mode entirely."
const terraformCatalogue = "../../deploy/terraform/modules/kafka-topics/variables.tf"

// terraformTopicConfig extracts one topic's `config = { ... }` block from the
// Terraform catalogue.
//
// Reading the file rather than copying the numbers into Go recreates nothing:
// copying would reproduce the exact drift this whole arrangement exists to
// prevent, a test asserting compaction works against values the cluster is no
// longer deployed with. Reading means the test is wrong the moment the catalogue
// changes, which is the correct time to find out.
//
// A regex rather than an HCL parser: adding an HCL dependency to the Go module to
// read four numbers would be a worse trade than a parser that fails loudly when
// the file's shape changes. It asserts what it found, so a silent partial match
// is not possible.
//
// "odds.raw" is spelled without a provider because the catalogue declares the
// family once, as `var.raw_topic`, rather than per provider.
func terraformTopicConfig(t *testing.T, topic string) map[string]string {
	t.Helper()

	raw, err := os.ReadFile(terraformCatalogue)
	if err != nil {
		t.Fatalf("read the Terraform topic catalogue at %s: %v", terraformCatalogue, err)
	}
	text := string(raw)

	var rest string
	if topic == "odds.raw" {
		// The raw family is a single `default = {` block on var.raw_topic rather
		// than a keyed entry in var.topics.
		start := strings.Index(text, `variable "raw_topic"`)
		if start < 0 {
			t.Fatalf("the Terraform catalogue no longer declares var.raw_topic; this test and %s have drifted apart",
				terraformCatalogue)
		}
		rest = text[start:]
		if next := strings.Index(rest, `variable "topics"`); next > 0 {
			rest = rest[:next]
		}
	} else {
		start := strings.Index(text, `"`+topic+`" = {`)
		if start < 0 {
			t.Fatalf("the Terraform catalogue no longer declares %q; this test and %s have drifted apart",
				topic, terraformCatalogue)
		}
		// The block ends at the next topic declaration or at the end of the file.
		rest = text[start+len(topic)+6:]
		if next := regexp.MustCompile(`\n\s*"[a-z.]+" = \{`).FindStringIndex(rest); next != nil {
			rest = rest[:next[0]]
		}
	}

	config := map[string]string{}
	for _, m := range regexp.MustCompile(`"([a-z.]+)"\s*=\s*"([^"]*)"`).FindAllStringSubmatch(rest, -1) {
		key, value := m[1], m[2]
		// `cleanup_policy` and friends use underscores; only the dotted Kafka
		// config keys belong in the map handed to the broker.
		if strings.Contains(key, ".") {
			config[key] = value
		}
	}
	if len(config) == 0 {
		t.Fatalf("extracted no config keys for %q from %s; the file's shape has changed",
			topic, terraformCatalogue)
	}
	return config
}

// mustInt reads a numeric topic config value.
func mustInt(t *testing.T, config map[string]string, key string) int64 {
	t.Helper()
	raw, ok := config[key]
	if !ok {
		t.Fatalf("topic config has no %q; keys present: %v", key, sortedKeys(config))
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("topic config %q = %q, which is not an integer: %v", key, raw, err)
	}
	return n
}

// mustFloat reads a fractional topic config value.
func mustFloat(t *testing.T, config map[string]string, key string) float64 {
	t.Helper()
	raw, ok := config[key]
	if !ok {
		t.Fatalf("topic config has no %q; keys present: %v", key, sortedKeys(config))
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("topic config %q = %q, which is not a number: %v", key, raw, err)
	}
	return f
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// A tiny insertion sort keeps this file free of a sort import for one
	// diagnostic message.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// copyConfig returns a mutable copy so a test can override a timing knob without
// mutating the catalogue-derived map another parallel test is reading.
func copyConfig(src map[string]string, overrides map[string]string) map[string]string {
	out := make(map[string]string, len(src)+len(overrides))
	for k, v := range src {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}
