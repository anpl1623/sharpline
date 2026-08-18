package theoddsapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
)

// Credential redaction.
//
// # Why this file exists and why it is this thorough
//
// The Odds API authenticates with the key as a QUERY PARAMETER. That makes the
// natural, idiomatic, otherwise-correct thing to do — wrap the failing request
// into an error and log it — a credential leak, because net/http's *url.Error
// formats the FULL request URL. doc.go names the three hazards; these tests are
// the proof for each of them.
//
// A leaked key is not recoverable by deleting a log line. It is recoverable by
// rotating a key, which nobody remembers to do. So the assertion here is an
// ABSENCE across every string the package can produce, not a spot check on the
// one place the author remembered.

// assertNoKey fails if the credential appears anywhere in s.
func assertNoKey(t *testing.T, where, s string) {
	t.Helper()
	if strings.Contains(s, testAPIKey) {
		t.Fatalf("API KEY LEAKED into %s:\n%s", where, s)
	}
	// Also refuse a partial leak: the tail of a key is enough to make a
	// credential guessable if the rest is known.
	if tail := testAPIKey[len(testAPIKey)-12:]; strings.Contains(s, tail) {
		t.Fatalf("API KEY TAIL LEAKED into %s:\n%s", where, s)
	}
}

// echoURLHandler answers with the FULL request URI in the body, which is the
// worst realistic provider behaviour: some gateways reflect the request in
// their error page, and that page becomes an error message and a log line.
func echoURLHandler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"message":"INVALID_KEY: request %s%s failed"}`, r.Host, r.URL.RequestURI())
	}
}

func TestRedactionErrorFromProviderBody(t *testing.T) {
	stub := newProviderStub(t)
	stub.fallback = echoURLHandler(http.StatusUnauthorized)
	h := newHarness(t, stub, nil)

	_, err := h.Adapter.Fetch(context.Background(), provider.Scope{
		League:  mustLeagueID(t, "americanfootball_nfl"),
		Markets: []domain.MarketType{domain.MarketTypeMoneyline},
	})
	if err == nil {
		t.Fatalf("Fetch succeeded against a 401")
	}

	assertNoKey(t, "the returned error", err.Error())
	if !strings.Contains(err.Error(), redactedPlaceholder) {
		t.Errorf("error does not show the key was removed; %q is what distinguishes "+
			"a redacted value from a request that forgot the key:\n%s", redactedPlaceholder, err.Error())
	}
	assertNoKey(t, "the log", h.Logs.text())
	assertNoKey(t, "the trace", h.spanText(t))

	// A 401 naming a key code is a human-fixable deployment error, not
	// something to retry for ever.
	if got, want := provider.Classify(err), provider.DispositionFatal; got != want {
		t.Errorf("disposition = %s, want %s", got, want)
	}
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Errorf("error does not unwrap to provider.ErrUnauthorized: %v", err)
	}
}

// TestRedactionErrorFromTransportFailure covers the *url.Error path, which is
// the one that leaks by default.
//
// net/http returns *url.Error from Do and its Error() interpolates the whole
// request URL. Nothing in this package formats that URL itself, so if
// sanitizeError were removed this test — and only this test — would catch it.
func TestRedactionErrorFromTransportFailure(t *testing.T) {
	// A server that is listening and then is not: the dial fails and the error
	// carries the URL that was attempted.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	stub := newProviderStub(t)
	h := newHarness(t, stub, func(c *Config) { c.BaseURL = deadURL })

	_, err := h.Adapter.Catalogue(context.Background())
	if err == nil {
		t.Fatalf("Catalogue succeeded against a closed server")
	}
	assertNoKey(t, "the transport error", err.Error())
	assertNoKey(t, "the log", h.Logs.text())
	assertNoKey(t, "the trace", h.spanText(t))

	// The identity of the wrapped cause must survive redaction, or upstream
	// timeout and DNS detection would stop working.
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Errorf("redaction destroyed the *url.Error identity; errors.As no longer finds it: %v", err)
	}
	if got, want := provider.Classify(err), provider.DispositionRetryable; got != want {
		t.Errorf("disposition = %s, want %s — a dial failure says nothing about the next poll", got, want)
	}
}

// TestRedactionSpanURLAttribute asserts the SUCCESS path, where no error is
// produced and the URL reaches a span attribute on its own.
func TestRedactionSpanURLAttribute(t *testing.T) {
	stub := newProviderStub(t)
	stub.route("/v4/sports/", json200(readGolden(t, goldenSports)))
	h := newHarness(t, stub, nil)

	if _, err := h.Adapter.Catalogue(context.Background()); err != nil {
		t.Fatalf("Catalogue: %v", err)
	}

	spans := h.spanText(t)
	assertNoKey(t, "the trace", spans)

	// The attribute must actually be present, or the assertion above passes for
	// the wrong reason.
	if !strings.Contains(spans, attrURLFull+"=") {
		t.Fatalf("no %s attribute was recorded; the redaction assertion would pass vacuously", attrURLFull)
	}
	if !strings.Contains(spans, apiKeyParam+"="+url.QueryEscape(redactedPlaceholder)) &&
		!strings.Contains(spans, apiKeyParam+"="+redactedPlaceholder) {
		t.Errorf("the %s span attribute does not carry a redacted %s parameter:\n%s",
			attrURLFull, apiKeyParam, spans)
	}
}

// TestRedactionRetryLogging drives the retry path, which is the only place this
// package logs an error, and asserts the log line is clean.
func TestRedactionRetryLogging(t *testing.T) {
	stub := newProviderStub(t)
	stub.fallback = echoURLHandler(http.StatusInternalServerError)
	h := newHarness(t, stub, nil)

	_, err := h.Adapter.Catalogue(context.Background())
	if err == nil {
		t.Fatalf("Catalogue succeeded against a 500")
	}

	logged := h.Logs.text()
	if !strings.Contains(logged, "provider request failed, retrying") {
		t.Fatalf("the retry path did not log; the redaction assertion would pass vacuously:\n%s", logged)
	}
	assertNoKey(t, "the retry log", logged)
	assertNoKey(t, "the error", err.Error())
	assertNoKey(t, "the trace", h.spanText(t))

	// DefaultMaxAttempts is 3, so a 5xx is attempted three times and logged
	// twice. Retries cost credits on a billed endpoint, which is why the count
	// is bounded by configuration rather than open-ended.
	if got, want := len(h.Stub.seen()), DefaultMaxAttempts; got != want {
		t.Errorf("issued %d attempts, want %d", got, want)
	}
	if got, want := provider.Classify(err), provider.DispositionRetryable; got != want {
		t.Errorf("disposition = %s, want %s", got, want)
	}
}

// TestRedactionCrossHostRedirect covers the third hazard doc.go names: a 3xx to
// another host would hand the key to whoever controls that host.
//
// The "attacker" is a real listening server, so a regression that removed the
// redirect policy would DELIVER the credential to it and the handler's
// t.Errorf would fire. Asserting only on the error message would let a broken
// policy pass as long as the follow-on request happened to fail.
//
// Its address is spelled `localhost` while the stub's is `127.0.0.1`. Both
// httptest servers necessarily listen on the loopback interface, so the port is
// the only thing that differs numerically — using the two different NAMES for
// it is what makes this a genuine test of the hostname comparison rather than
// of the port.
func TestRedactionCrossHostRedirect(t *testing.T) {
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the client followed a cross-host redirect and sent the apiKey %q to another host",
			r.URL.Query().Get(apiKeyParam))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(attacker.Close)

	target := strings.Replace(attacker.URL, "127.0.0.1", "localhost", 1)
	if target == attacker.URL {
		t.Fatalf("attacker server is not on 127.0.0.1 (%s); the test cannot construct a differing "+
			"hostname for the same listener", attacker.URL)
	}

	stub := newProviderStub(t)
	stub.fallback = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusFound)
	}
	h := newHarness(t, stub, nil)

	_, err := h.Adapter.Catalogue(context.Background())
	if err == nil {
		t.Fatalf("Catalogue followed a cross-host redirect without complaint")
	}
	assertNoKey(t, "the redirect error", err.Error())
	assertNoKey(t, "the log", h.Logs.text())
	assertNoKey(t, "the trace", h.spanText(t))
	if !strings.Contains(err.Error(), "cross-host redirect") {
		t.Errorf("error does not name the refused redirect: %v", err)
	}
	// net/http wraps a CheckRedirect refusal in a *url.Error whose URL is the
	// REDIRECT TARGET — a string this package did not build and does not
	// control. It still has to come out with the credential removed by
	// PARAMETER NAME, not merely by matching the literal we happen to hold.
	if !strings.Contains(err.Error(), apiKeyParam+"="+redactedPlaceholder) {
		t.Errorf("the attacker-supplied redirect URL in the error does not carry a redacted %s "+
			"parameter: %v", apiKeyParam, err)
	}
}

// TestRedactionOfAnUnfamiliarKeySpelling covers the case the by-value pass
// cannot: a URL from an untrusted source carrying the credential in a form this
// redactor was never told about.
func TestRedactionOfAnUnfamiliarKeySpelling(t *testing.T) {
	// The redactor knows one literal. The URL spells it with a percent-escaped
	// hyphen, so nothing matches by value.
	r := newRedactor(testAPIKey)
	escaped := strings.ReplaceAll(testAPIKey, "-", "%2D")
	raw := "https://evil.example.com/v4/sports/?apiKey=" + escaped

	got := r.RawURL(raw)
	if strings.Contains(got, escaped) {
		t.Fatalf("an alternately-encoded key survived redaction: %q", got)
	}
	assertNoKey(t, "an alternately-encoded URL", got)
	if !strings.Contains(got, apiKeyParam+"="+redactedPlaceholder) {
		t.Errorf("parameter-name redaction did not run: %q", got)
	}

	// An unparseable input must never come back raw.
	if got := r.RawURL("://not a url?apiKey=" + testAPIKey); strings.Contains(got, testAPIKey) {
		t.Errorf("an unparseable URL was returned with the key intact: %q", got)
	}
}

// TestRedactionRefusesTLSDowngradeRedirect covers the same-host variant of the
// hazard: a 302 from https to http on the SAME host passes a hostname check and
// still puts the apiKey on the wire in cleartext.
func TestRedactionRefusesTLSDowngradeRedirect(t *testing.T) {
	origin, err := url.Parse("https://api.the-odds-api.com/v4/sports/?apiKey=" + testAPIKey)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	downgraded, err := url.Parse("http://api.the-odds-api.com/v4/sports/?apiKey=" + testAPIKey)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	req := &http.Request{URL: downgraded}
	via := []*http.Request{{URL: origin}}
	if err := refuseCrossHostRedirect(req, via); err == nil {
		t.Fatalf("an https -> http redirect on the same host was allowed; the apiKey travels in the " +
			"query string and would go out in cleartext")
	} else {
		assertNoKey(t, "the downgrade refusal", err.Error())
	}

	// The reverse — an upgrade — must still be allowed, so a base URL
	// configured without TLS ends up encrypted rather than failing.
	up := &http.Request{URL: origin}
	if err := refuseCrossHostRedirect(up, []*http.Request{{URL: downgraded}}); err != nil {
		t.Errorf("an http -> https upgrade on the same host was refused: %v", err)
	}
}

// TestConfigLogValueNeverLeaksKey is the assertion config.go's LogValue comment
// promises by name.
func TestConfigLogValueNeverLeaksKey(t *testing.T) {
	cfg := baseConfig("https://api.the-odds-api.com").withDefaults()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("starting", slog.Any("config", cfg))

	assertNoKey(t, "a logged Config", buf.String())
	// The absence must be informative, not silent: an operator has to be able
	// to tell "no key configured" from "key configured and withheld".
	if !strings.Contains(buf.String(), `"api_key_set":true`) {
		t.Errorf("logged config does not report that a key is present:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `"api_key_len":`) {
		t.Errorf("logged config does not report the key length:\n%s", buf.String())
	}
}

// TestConfigValidateNeverEchoesKey covers the startup path: a too-short key must
// be reported by LENGTH, never by value.
func TestConfigValidateNeverEchoesKey(t *testing.T) {
	const shortKey = "changeme"
	cfg := baseConfig("https://api.the-odds-api.com")
	cfg.APIKey = shortKey
	err := cfg.withDefaults().Validate()
	if err == nil {
		t.Fatalf("an %d-byte key validated; minAPIKeyLen is %d", len(shortKey), minAPIKeyLen)
	}
	if strings.Contains(err.Error(), shortKey) {
		t.Fatalf("validation echoed the configured key value: %v", err)
	}
}

// TestRedactorProperties covers the two independent axes redact.go describes.
func TestRedactorProperties(t *testing.T) {
	r := newRedactor(testAPIKey)

	t.Run("by value in an arbitrary string", func(t *testing.T) {
		got := r.String("Get \"https://api.the-odds-api.com/v4/sports/?apiKey=" + testAPIKey + "\": eof")
		assertNoKey(t, "a redacted string", got)
		if !strings.Contains(got, redactedPlaceholder) {
			t.Errorf("redaction left no marker: %q", got)
		}
	})

	t.Run("by parameter name for a key it was never told about", func(t *testing.T) {
		// The parameter-name pass is what protects a URL assembled elsewhere,
		// a redirect target, or a key rotated at runtime.
		blind := newRedactor()
		u, err := url.Parse("https://api.the-odds-api.com/v4/sports/?apiKey=SOME-OTHER-KEY-ENTIRELY&regions=us")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got := blind.URL(u)
		if strings.Contains(got, "SOME-OTHER-KEY-ENTIRELY") {
			t.Fatalf("a redactor with no configured secret leaked an apiKey parameter: %q", got)
		}
		if !strings.Contains(got, "regions=us") {
			t.Errorf("redaction removed a non-sensitive parameter: %q", got)
		}
	})

	t.Run("does not mutate the caller's URL", func(t *testing.T) {
		u, err := url.Parse("https://api.the-odds-api.com/v4/sports/?apiKey=" + testAPIKey)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		_ = r.URL(u)
		if got := u.Query().Get(apiKeyParam); got != testAPIKey {
			// The URL still has to be REQUESTED after it has been logged.
			t.Fatalf("redaction mutated the caller's URL; the real key is gone and the request "+
				"would go out unauthenticated: %q", got)
		}
	})

	t.Run("refuses to mangle an implausibly short secret", func(t *testing.T) {
		// Replacing every occurrence of a two-character "secret" would corrupt
		// unrelated text into nonsense while protecting nothing.
		short := newRedactor("ab")
		if got := short.String("a bad cab"); got != "a bad cab" {
			t.Errorf("a 2-byte secret was redacted out of unrelated text: %q", got)
		}
	})
}

// TestRedactionAcrossAWholeAdapterLifecycle is the whole-surface sweep.
//
// Every other test in this file aims at one hazard with one call. This one
// drives the adapter the way `ingest` drives it — catalogue, then a successful
// sweep, then each failure class the provider documents — and asserts the
// credential appears in NOTHING the adapter produced: not a log record, not an
// error string at any depth of the wrap chain, not a span attribute, not a
// metric label.
//
// The reason to have both shapes is that the targeted tests prove the mechanism
// and this one proves the COVERAGE. A new code path that logs a URL is invisible
// to a test that calls one method; it is caught here, because here the assertion
// is over everything the adapter emitted rather than over the string the author
// remembered to check.
//
// The key used is `testAPIKey`, whose value literally reads
// "THISKEYMUSTNEVERAPPEAR" — so a failure prints something unambiguous rather
// than a plausible-looking hex string a reader might skim past.
func TestRedactionAcrossAWholeAdapterLifecycle(t *testing.T) {
	// Each stage is a separate harness because a stub answers one way at a
	// time, but every stage's output is accumulated and asserted together.
	var everything strings.Builder

	stage := func(name string, route func(*providerStub), call func(*testHarness) error) {
		t.Helper()
		stub := newProviderStub(t)
		route(stub)
		h := newHarness(t, stub, nil)

		err := call(h)

		everything.WriteString("\n--- " + name + " logs ---\n")
		everything.WriteString(h.Logs.text())
		everything.WriteString("\n--- " + name + " spans ---\n")
		everything.WriteString(h.spanText(t))
		if err != nil {
			everything.WriteString("\n--- " + name + " error ---\n")
			everything.WriteString(err.Error())
			// The wrap CHAIN, not just the top message: a sanitised outer
			// message around an unsanitised cause still leaks the moment
			// anything formats the cause with %v.
			for e := err; e != nil; e = errors.Unwrap(e) {
				everything.WriteString("\n" + e.Error())
			}
		}
		// The request the stub actually received is recorded too. It MUST
		// contain the real key — that is the whole point of authenticating —
		// so it is asserted separately below rather than swept in here.
		if len(stub.seen()) == 0 {
			t.Fatalf("%s issued no request; every assertion about it would pass vacuously", name)
		}
	}

	sports := readGolden(t, goldenSports)
	odds := stripDocsElision(t, readGolden(t, goldenOdds))
	nfl := mustLeagueID(t, "americanfootball_nfl")
	markets := []domain.MarketType{domain.MarketTypeMoneyline, domain.MarketTypeSpread}

	stage("catalogue ok",
		func(s *providerStub) { s.route("/v4/sports/", json200(sports)) },
		func(h *testHarness) error { _, err := h.Adapter.Catalogue(context.Background()); return err })

	stage("fetch ok",
		func(s *providerStub) {
			s.route("/v4/sports/americanfootball_nfl/odds/", json200(odds))
		},
		func(h *testHarness) error {
			_, err := h.Adapter.Fetch(context.Background(), provider.Scope{League: nfl, Markets: markets})
			return err
		})

	// The documented failure classes, each one a path that formats a URL.
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"401 invalid key", http.StatusUnauthorized},
		{"404 unknown event", http.StatusNotFound},
		{"422 bad parameters", http.StatusUnprocessableEntity},
		{"429 rate limited", http.StatusTooManyRequests},
		{"500 upstream", http.StatusInternalServerError},
	} {
		stage("fetch "+tc.name,
			func(s *providerStub) { s.fallback = echoURLHandler(tc.status) },
			func(h *testHarness) error {
				_, err := h.Adapter.Fetch(context.Background(), provider.Scope{League: nfl, Markets: markets})
				if err == nil {
					t.Fatalf("Fetch succeeded against a %d", tc.status)
				}
				return err
			})
	}

	// A malformed body, which is the path that stores a response EXCERPT.
	stage("fetch malformed body",
		func(s *providerStub) {
			s.fallback = func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				// The body echoes the request URI, so an unredacted excerpt
				// carries the key into the error.
				_, _ = fmt.Fprintf(w, "not json, and here is your request: %s", r.URL.RequestURI())
			}
		},
		func(h *testHarness) error {
			_, err := h.Adapter.Fetch(context.Background(), provider.Scope{League: nfl, Markets: markets})
			if err == nil {
				t.Fatalf("Fetch accepted a body that is not JSON")
			}
			return err
		})

	assertNoKey(t, "the adapter's complete output across a full lifecycle", everything.String())

	// Guard against the assertion passing because nothing was emitted.
	if n := len(everything.String()); n < 512 {
		t.Fatalf("only %d bytes of adapter output were collected; the leak assertion is close to "+
			"vacuous. Check that the stages actually ran.", n)
	}
	// And guard against it passing because the key never reached the wire at
	// all: the requests the stub SAW must carry it, or nothing was authenticated
	// and there was never a credential to leak.
	stub := newProviderStub(t)
	stub.route("/v4/sports/", json200(sports))
	h := newHarness(t, stub, nil)
	if _, err := h.Adapter.Catalogue(context.Background()); err != nil {
		t.Fatalf("Catalogue: %v", err)
	}
	seen := stub.seen()
	if len(seen) == 0 {
		t.Fatal("no request was recorded")
	}
	if got := seen[0].Query.Get(apiKeyParam); got != testAPIKey {
		t.Fatalf("the request carried %s=%q, want the real key. If the adapter is not sending the "+
			"credential at all, every redaction assertion in this file is vacuous.", apiKeyParam, got)
	}
}
