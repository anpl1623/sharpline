package wsgw

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/anpl1623/sharpline/internal/domain"
)

func TestParseChannel(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantKind ChannelKind
		wantID   string
		wantErr  bool
		wantRej  RejectReason
	}{
		{name: "event", in: "event:evt-1", wantKind: ChannelEvent, wantID: "evt-1"},
		{name: "market", in: "market:mkt-1", wantKind: ChannelMarket, wantID: "mkt-1"},
		{name: "league", in: "league:nfl", wantKind: ChannelLeague, wantID: "nfl"},
		{
			name:    "no colon",
			in:      "market",
			wantErr: true,
			wantRej: RejectMalformed,
		},
		{
			name:    "unknown kind",
			in:      "user:me",
			wantErr: true,
			wantRej: RejectUnknownKind,
		},
		{
			// The load-bearing case. domain/ids.go excludes ':' from the
			// identifier charset precisely so this split is unambiguous, and a
			// colon in the tail must therefore be a parse error rather than a
			// silently truncated subscription that leaks across channels.
			name:    "colon inside the identifier",
			in:      "market:a:b",
			wantErr: true,
			wantRej: RejectInvalidID,
		},
		{
			name:    "empty identifier",
			in:      "market:",
			wantErr: true,
			wantRej: RejectInvalidID,
		},
		{
			name:    "empty string",
			in:      "",
			wantErr: true,
			wantRej: RejectMalformed,
		},
		{
			// A league is keyed by slug, which is lowercase by construction —
			// domain.NewSlug rejects rather than folds, so "NFL" and "nfl"
			// cannot become two spellings of one subscription.
			name:    "uppercase league slug",
			in:      "league:NFL",
			wantErr: true,
			wantRej: RejectInvalidID,
		},
		{
			name:    "whitespace in the identifier",
			in:      "market:a b",
			wantErr: true,
			wantRej: RejectInvalidID,
		},
		{
			name:    "over-long",
			in:      "market:" + strings.Repeat("x", domain.MaxIDLen+64),
			wantErr: true,
			wantRej: RejectTooLong,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseChannel(tc.in)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidChannel) {
					t.Fatalf("ParseChannel(%q) error = %v, want ErrInvalidChannel", tc.in, err)
				}
				if got := ChannelRejectReason(tc.in); got != tc.wantRej {
					t.Errorf("ChannelRejectReason(%q) = %q, want %q", tc.in, got, tc.wantRej)
				}
				if !got.IsZero() {
					t.Errorf("a failed parse returned a non-zero channel %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseChannel(%q): %v", tc.in, err)
			}
			if got.Kind() != tc.wantKind || got.ID() != tc.wantID {
				t.Errorf("got kind=%q id=%q, want kind=%q id=%q", got.Kind(), got.ID(), tc.wantKind, tc.wantID)
			}
			if got.String() != tc.in {
				t.Errorf("String() = %q, want the input %q", got.String(), tc.in)
			}
			if got.IsZero() {
				t.Error("a parsed channel reports itself zero")
			}
		})
	}
}

// TestParseChannelErrorsDoNotEchoTheInput. A channel string is untrusted input
// and its parse error becomes a log line. The classification travels through
// ChannelRejectReason instead, and the value only reaches the ack frame, bounded
// by SafeEcho.
func TestParseChannelErrorsDoNotEchoTheInput(t *testing.T) {
	hostile := "market:" + strings.Repeat("secret", 40)
	_, err := ParseChannel(hostile)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "secretsecret") {
		t.Errorf("the parse error echoes the input unbounded: %v", err)
	}
}

func TestChannelTextRoundTrip(t *testing.T) {
	for _, want := range []string{"event:evt-1", "market:mkt-1", "league:nfl"} {
		t.Run(want, func(t *testing.T) {
			ch, err := ParseChannel(want)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(ch)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(raw) != `"`+want+`"` {
				t.Fatalf("Marshal = %s, want a JSON string %q", raw, want)
			}
			var back Channel
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if back != ch {
				t.Fatalf("round trip = %q, want %q", back, ch)
			}
		})
	}
}

// TestZeroChannelRefusesToMarshal. A zero Channel can only come from a frame
// assembled without one, which is a hub bug. Failing the marshal fails the frame
// loudly; an empty string would ship a frame naming a channel nobody can hold.
func TestZeroChannelRefusesToMarshal(t *testing.T) {
	if _, err := json.Marshal(Channel{}); err == nil {
		t.Fatal("a zero channel marshalled successfully")
	}
	if got := (Channel{}).String(); got != "" {
		t.Errorf("zero Channel String() = %q, want the empty string", got)
	}
}

// TestChannelIsUsableAsAMapKey. The routing table and the snapshot map both key
// on Channel, so comparability is a requirement rather than an accident of the
// struct's current fields.
func TestChannelIsUsableAsAMapKey(t *testing.T) {
	a, err := ParseChannel("league:nfl")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseChannel("league:nfl")
	if err != nil {
		t.Fatal(err)
	}
	m := map[Channel]int{a: 1}
	m[b]++
	if len(m) != 1 || m[a] != 2 {
		t.Fatalf("two parses of one channel are not the same map key: %v", m)
	}
}

func TestChannelConstructorsValidate(t *testing.T) {
	if _, err := EventChannel(domain.EventID("bad:id")); !errors.Is(err, ErrInvalidChannel) {
		t.Errorf("EventChannel accepted an identifier containing a colon")
	}
	if _, err := MarketChannel(domain.MarketID("")); !errors.Is(err, ErrInvalidChannel) {
		t.Errorf("MarketChannel accepted an empty identifier")
	}
	if _, err := LeagueChannel(domain.Slug("NFL")); !errors.Is(err, ErrInvalidChannel) {
		t.Errorf("LeagueChannel accepted an uppercase slug")
	}
}

// TestChannelsForIsTheSingleDefinition. Both the publish path and the snapshot
// path call it; if they each derived the set, a market could be delivered on
// event:X and snapshotted only on market:Y, and the board would be correct on
// connect and drift afterwards.
func TestChannelsForIsTheSingleDefinition(t *testing.T) {
	got, err := ChannelsFor(sampleComputed(t))
	if err != nil {
		t.Fatalf("ChannelsFor: %v", err)
	}
	want := []string{"market:" + sampleMarketID, "event:" + sampleEventID, "league:" + sampleLeagueSlug}
	if len(got) != len(want) {
		t.Fatalf("got %d channels, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("channel[%d] = %q, want %q — the order is market, event, league",
				i, got[i], want[i])
		}
	}
}

func TestChannelsForRefusesAnUnroutableMarket(t *testing.T) {
	t.Run("bad event id", func(t *testing.T) {
		c := sampleComputed(t)
		c.Event.ID = "not a valid id"
		if _, err := ChannelsFor(c); !errors.Is(err, ErrInvalidChannel) {
			t.Fatalf("error = %v, want ErrInvalidChannel", err)
		}
	})
	t.Run("bad league slug", func(t *testing.T) {
		c := sampleComputed(t)
		c.League.Slug = "NFL"
		if _, err := ChannelsFor(c); !errors.Is(err, ErrInvalidChannel) {
			t.Fatalf("error = %v, want ErrInvalidChannel", err)
		}
	})
	t.Run("bad market id", func(t *testing.T) {
		c := sampleComputed(t)
		c.Market.ID = ""
		if _, err := ChannelsFor(c); !errors.Is(err, ErrInvalidChannel) {
			t.Fatalf("error = %v, want ErrInvalidChannel", err)
		}
	})
}

func TestChannelKindsAreClosed(t *testing.T) {
	for _, k := range ChannelKinds() {
		if !k.Valid() {
			t.Errorf("ChannelKind %q is listed but not valid", k)
		}
	}
	if ChannelKind("user").Valid() {
		t.Error("an unlisted kind reports itself valid")
	}
}
