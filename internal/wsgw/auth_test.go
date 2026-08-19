package wsgw

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

const sampleToken = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1LTEifQ.c2ln"

func upgradeRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodGet, target, nil)
}

func TestExtractCredential(t *testing.T) {
	cases := []struct {
		name       string
		target     string
		protocols  []string
		authHeader string
		wantSource CredentialSource
		wantToken  string
		wantErr    error
	}{
		{
			name:       "no credential is the normal case",
			target:     "/ws",
			protocols:  []string{Protocol},
			wantSource: CredentialAbsent,
		},
		{
			name:       "subprotocol offer",
			target:     "/ws",
			protocols:  []string{Protocol, BearerSubprotocolPrefix + sampleToken},
			wantSource: CredentialSubprotocol,
			wantToken:  sampleToken,
		},
		{
			name:       "authorization header",
			target:     "/ws",
			protocols:  []string{Protocol},
			authHeader: "Bearer " + sampleToken,
			wantSource: CredentialHeader,
			wantToken:  sampleToken,
		},
		{
			name:       "the scheme match is case-insensitive, as RFC 7235 requires",
			target:     "/ws",
			protocols:  []string{Protocol},
			authHeader: "bearer " + sampleToken,
			wantSource: CredentialHeader,
			wantToken:  sampleToken,
		},
		{
			// The subprotocol is the browser's only option, so it wins where
			// both are offered — a client that can set headers is a client that
			// chose to send both.
			name:       "the subprotocol offer takes precedence over the header",
			target:     "/ws",
			protocols:  []string{Protocol, BearerSubprotocolPrefix + "from-subprotocol"},
			authHeader: "Bearer from-header",
			wantSource: CredentialSubprotocol,
			wantToken:  "from-subprotocol",
		},
		{
			name:      "a token in ?token= is REFUSED, not ignored",
			target:    "/ws?token=" + sampleToken,
			protocols: []string{Protocol},
			wantErr:   ErrTokenInQuery,
		},
		{
			name:      "a token in ?access_token= is refused too",
			target:    "/ws?access_token=" + sampleToken,
			protocols: []string{Protocol},
			wantErr:   ErrTokenInQuery,
		},
		{
			// Already in the proxy's access log by the time we see it. Serving
			// the connection anyway would teach the client that it worked.
			name:      "a query token is refused even when a good credential is also offered",
			target:    "/ws?token=" + sampleToken,
			protocols: []string{Protocol, BearerSubprotocolPrefix + sampleToken},
			wantErr:   ErrTokenInQuery,
		},
		{
			name:      "an empty query parameter is still a refusal",
			target:    "/ws?token=",
			protocols: []string{Protocol},
			wantErr:   ErrTokenInQuery,
		},
		{
			name:      "an empty subprotocol credential is a refusal, never anonymous",
			target:    "/ws",
			protocols: []string{Protocol, BearerSubprotocolPrefix},
			wantErr:   ErrInvalidCredential,
		},
		{
			name:       "a non-bearer Authorization header is a refusal, never anonymous",
			target:     "/ws",
			protocols:  []string{Protocol},
			authHeader: "Basic dXNlcjpwYXNz",
			wantErr:    ErrInvalidCredential,
		},
		{
			name:       "a comma-separated credential list is refused rather than partially parsed",
			target:     "/ws",
			protocols:  []string{Protocol},
			authHeader: "Bearer " + sampleToken + ", Bearer other",
			wantErr:    ErrInvalidCredential,
		},
		{
			name:      "a credential carrying bytes no access token can carry is refused",
			target:    "/ws",
			protocols: []string{Protocol, BearerSubprotocolPrefix + "not a token"},
			wantErr:   ErrInvalidCredential,
		},
		{
			name:      "an over-long credential is refused",
			target:    "/ws",
			protocols: []string{Protocol, BearerSubprotocolPrefix + strings.Repeat("a", MaxTokenLen+1)},
			wantErr:   ErrInvalidCredential,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := upgradeRequest(t, tc.target)
			if len(tc.protocols) > 0 {
				r.Header.Set("Sec-WebSocket-Protocol", strings.Join(tc.protocols, ", "))
			}
			if tc.authHeader != "" {
				r.Header.Set("Authorization", tc.authHeader)
			}

			got, err := ExtractCredential(r)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
				// The credential must never reach an error, which becomes a log
				// line and a close reason.
				if err != nil && strings.Contains(err.Error(), sampleToken) {
					t.Errorf("the error carries the token: %v", err)
				}
				if got.Present() {
					t.Errorf("a refused request produced a present credential")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Source() != tc.wantSource {
				t.Errorf("source = %q, want %q", got.Source(), tc.wantSource)
			}
			if got.Token() != tc.wantToken {
				t.Errorf("token = %q, want %q", got.Token(), tc.wantToken)
			}
			if got.Present() != (tc.wantSource != CredentialAbsent) {
				t.Errorf("Present() = %v for source %q", got.Present(), got.Source())
			}
		})
	}
}

// TestCredentialNeverRendersItsToken. The token is unexported and reachable only
// through Token(), so a Credential cannot be spread into a log line by accident.
// LogValue is what makes logging one deliberate rather than dangerous.
func TestCredentialNeverRendersItsToken(t *testing.T) {
	r := upgradeRequest(t, "/ws")
	r.Header.Set("Sec-WebSocket-Protocol", Protocol+", "+BearerSubprotocolPrefix+sampleToken)

	c, err := ExtractCredential(r)
	if err != nil {
		t.Fatal(err)
	}
	rendered := c.LogValue().String()
	if strings.Contains(rendered, sampleToken) {
		t.Fatalf("LogValue rendered the token: %s", rendered)
	}
	if !strings.Contains(rendered, string(CredentialSubprotocol)) {
		t.Errorf("LogValue does not name the mechanism: %s", rendered)
	}
}

// TestSelectSubprotocolEchoesOnlyTheProtocol.
//
// Echoing the bearer offer would write the client's own access token into the
// handshake RESPONSE — a header a proxy may log on a path where the request
// headers were not logged at all.
func TestSelectSubprotocolEchoesOnlyTheProtocol(t *testing.T) {
	r := upgradeRequest(t, "/ws")
	r.Header.Set("Sec-WebSocket-Protocol", Protocol+", "+BearerSubprotocolPrefix+sampleToken)

	got, ok := SelectSubprotocol(r)
	if !ok {
		t.Fatal("the offered protocol was not selected")
	}
	if got != Protocol {
		t.Fatalf("selected %q, want only %q", got, Protocol)
	}
}

// TestSelectSubprotocolRefusesAnUnversionedClient. There is no default: an
// unversioned connection cannot have its frame shapes changed later without
// breaking somebody silently.
func TestSelectSubprotocolRefusesAnUnversionedClient(t *testing.T) {
	cases := map[string]string{
		"no header":       "",
		"only the bearer": BearerSubprotocolPrefix + sampleToken,
		"another version": "sharpline.v2",
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			r := upgradeRequest(t, "/ws")
			if header != "" {
				r.Header.Set("Sec-WebSocket-Protocol", header)
			}
			if _, ok := SelectSubprotocol(r); ok {
				t.Fatal("a client that did not offer sharpline.v1 was accepted")
			}
		})
	}
}

// TestOffersAreBounded. Sec-WebSocket-Protocol is unbounded on the wire: a
// comma-separated list, repeatable. The parse is bounded so a stranger cannot
// make this package walk an arbitrary list.
func TestOffersAreBounded(t *testing.T) {
	r := upgradeRequest(t, "/ws")
	r.Header.Set("Sec-WebSocket-Protocol", strings.Repeat("x,", 500)+Protocol)
	if got := len(Offers(r)); got > maxSubprotocolOffers {
		t.Fatalf("Offers returned %d entries, want at most %d", got, maxSubprotocolOffers)
	}

	// Repeated headers are flattened, not just the first one read.
	r2 := upgradeRequest(t, "/ws")
	r2.Header.Add("Sec-WebSocket-Protocol", "other")
	r2.Header.Add("Sec-WebSocket-Protocol", Protocol)
	if _, ok := SelectSubprotocol(r2); !ok {
		t.Fatal("an offer in a second Sec-WebSocket-Protocol header was not seen")
	}
}

func TestAuthenticate(t *testing.T) {
	verified := Identity{
		UserID:    domain.UserID("usr-1"),
		SessionID: "sess-1",
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	ok := TokenVerifierFunc(func(context.Context, string) (Identity, error) { return verified, nil })
	broken := TokenVerifierFunc(func(context.Context, string) (Identity, error) {
		// An INTERNAL failure inside the verifier — a database timeout, a
		// missing key. It must be a rejection, never a pass-through.
		return Identity{}, errors.New("key store unavailable")
	})
	empty := TokenVerifierFunc(func(context.Context, string) (Identity, error) { return Identity{}, nil })

	cases := []struct {
		name     string
		verifier TokenVerifier
		cred     Credential
		wantErr  bool
		wantAnon bool
	}{
		{
			name:     "no credential is anonymous, which is legal",
			verifier: ok,
			cred:     Credential{source: CredentialAbsent},
			wantAnon: true,
		},
		{
			name:     "no credential and no verifier is still anonymous",
			verifier: nil,
			cred:     Credential{source: CredentialAbsent},
			wantAnon: true,
		},
		{
			name:     "a verified credential produces an identity",
			verifier: ok,
			cred:     Credential{token: sampleToken, source: CredentialSubprotocol},
		},
		{
			name:     "a credential with no verifier is refused, never downgraded",
			verifier: nil,
			cred:     Credential{token: sampleToken, source: CredentialSubprotocol},
			wantErr:  true,
		},
		{
			name:     "an internal verifier failure is a rejection, not a pass-through",
			verifier: broken,
			cred:     Credential{token: sampleToken, source: CredentialHeader},
			wantErr:  true,
		},
		{
			name:     "a verifier returning nobody with no error fails closed",
			verifier: empty,
			cred:     Credential{token: sampleToken, source: CredentialHeader},
			wantErr:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := Authenticate(context.Background(), tc.verifier, tc.cred)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidCredential) {
					t.Fatalf("error = %v, want ErrInvalidCredential", err)
				}
				if !id.Anonymous() {
					t.Errorf("a refused credential produced an identity: %+v", id)
				}
				if strings.Contains(err.Error(), sampleToken) {
					t.Errorf("the error carries the token: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if id.Anonymous() != tc.wantAnon {
				t.Errorf("Anonymous() = %v, want %v", id.Anonymous(), tc.wantAnon)
			}
		})
	}
}

// TestAuthenticatePassesTheRawTokenToTheVerifierAndNothingElse.
func TestAuthenticatePassesTheRawTokenToTheVerifierAndNothingElse(t *testing.T) {
	var seen string
	v := TokenVerifierFunc(func(_ context.Context, token string) (Identity, error) {
		seen = token
		return Identity{UserID: domain.UserID("usr-1")}, nil
	})
	if _, err := Authenticate(context.Background(), v,
		Credential{token: sampleToken, source: CredentialHeader}); err != nil {
		t.Fatal(err)
	}
	if seen != sampleToken {
		t.Fatalf("the verifier received %q, want the raw credential", seen)
	}
}

// TestIdentityLogValueIsSafeByConstruction. There is no token field on Identity,
// deliberately, so logging one whole is safe — and the anonymous case must not
// pretend to name a user.
func TestIdentityLogValueIsSafeByConstruction(t *testing.T) {
	anon := Identity{}
	if !anon.Anonymous() {
		t.Fatal("the zero Identity does not report itself anonymous")
	}
	if got := anon.LogValue().String(); !strings.Contains(got, "anonymous") {
		t.Errorf("anonymous LogValue = %s", got)
	}

	named := Identity{UserID: domain.UserID("usr-1"), SessionID: "sess-1"}
	if got := named.LogValue().String(); !strings.Contains(got, "usr-1") {
		t.Errorf("LogValue does not name the user: %s", got)
	}
}

func TestExtractCredentialRejectsANilRequest(t *testing.T) {
	if _, err := ExtractCredential(nil); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("error = %v, want ErrInvalidCredential", err)
	}
}
