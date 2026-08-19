package wsgw

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// frameAt is the instant every rendered frame in this file is stamped with. A
// fixed value rather than time.Now, so a failure is reproducible.
var frameAt = time.Date(2026, 8, 19, 12, 0, 0, 123456789, time.UTC)

func mustChannel(t *testing.T, s string) Channel {
	t.Helper()
	ch, err := ParseChannel(s)
	if err != nil {
		t.Fatalf("ParseChannel(%q): %v", s, err)
	}
	return ch
}

// decodeFrame unmarshals a rendered frame into a generic map, which is the
// closest a test can get to what a browser actually does with it.
func decodeFrame(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the rendered frame is not valid JSON: %v\nframe: %s", err, raw)
	}
	return got
}

// TestEveryServerFrameRoundTripsThroughEncodingJSON is the assertion that the
// shared-body split produces valid JSON.
//
// The split is a byte-level trick — marshal the struct, drop the leading '{',
// prepend `{"seq":N,` — and the two ways it could go wrong are a doubled comma
// and a missing brace. Neither is visible by reading the code; both are visible
// the instant encoding/json is asked to read the result back.
func TestEveryServerFrameRoundTripsThroughEncodingJSON(t *testing.T) {
	market := mustChannel(t, "market:mkt-1")

	cases := []struct {
		name string
		body FrameBody
		kind MessageKind
		want map[string]any
	}{
		{
			name: "hello",
			body: NewHello("conn-1", "sess-1", 20*time.Second, true, false, []Channel{market}),
			kind: KindHello,
			want: map[string]any{
				"connection_id":     "conn-1",
				"protocol":          Protocol,
				"heartbeat_seconds": float64(20),
				"session_id":        "sess-1",
				"resumed":           true,
				"authenticated":     false,
			},
		},
		{
			name: "ack",
			body: NewAck([]Channel{market}, []RejectedChannel{{Channel: "nope", Reason: RejectMalformed}}),
			kind: KindAck,
		},
		{
			name: "snapshot",
			body: NewSnapshot(market, []json.RawMessage{json.RawMessage(`{"market":{"id":"mkt-1"}}`)}),
			kind: KindSnapshot,
			want: map[string]any{"complete": true},
		},
		{
			name: "delta update",
			body: NewDelta(market, json.RawMessage(`{"market":{"id":"mkt-1"}}`)),
			kind: KindDelta,
		},
		{
			name: "delta tombstone",
			body: NewRemoval(market, "mkt-1"),
			kind: KindDelta,
			want: map[string]any{"removed": "mkt-1"},
		},
		{
			name: "resync",
			body: NewResync(ResyncSlowConsumer, 37, 5, 41),
			kind: KindResync,
			want: map[string]any{
				"reason":   string(ResyncSlowConsumer),
				"dropped":  float64(37),
				"from_seq": float64(5),
				"to_seq":   float64(41),
			},
		},
		{
			name: "error",
			body: NewError(CodeFrameTooLarge, "frame exceeds the size limit"),
			kind: KindError,
			want: map[string]any{"code": string(CodeFrameTooLarge)},
		},
		{
			name: "pong",
			body: NewPong(),
			kind: KindPong,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := Frame(7, tc.body, frameAt, "req-9")
			if err != nil {
				t.Fatalf("Frame: %v", err)
			}

			// The sequence number is the FIRST key, and the seam between the
			// prefix and the body is a single comma.
			if !strings.HasPrefix(string(raw), `{"seq":7,"type":`) {
				t.Fatalf("frame does not open with the sequence number and the type: %s", raw)
			}
			if strings.Contains(string(raw), ",,") {
				t.Fatalf("frame contains a doubled comma: %s", raw)
			}

			got := decodeFrame(t, raw)
			if got["seq"] != float64(7) {
				t.Errorf("seq = %v, want 7", got["seq"])
			}
			if got["type"] != string(tc.kind) {
				t.Errorf("type = %v, want %q", got["type"], tc.kind)
			}
			if got["id"] != "req-9" {
				t.Errorf("id = %v, want the echoed request id", got["id"])
			}
			ts, ok := got["ts"].(string)
			if !ok {
				t.Fatalf("ts = %v, want an RFC 3339 string", got["ts"])
			}
			if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
				t.Errorf("ts %q does not parse as RFC 3339 with nanoseconds: %v", ts, err)
			}
			for k, want := range tc.want {
				if got[k] != want {
					t.Errorf("%s = %#v, want %#v", k, got[k], want)
				}
			}
		})
	}
}

// TestOneBodyServesManyConnections is the fanout-cost decision, asserted.
//
// CLAUDE.md §10 targets 10k concurrent subscribers; a delta that marshalled once
// per subscriber would make that impossible. The property that makes the split
// safe is that Body is never mutated, so two connections rendering different
// sequence numbers from ONE body get two correct frames.
func TestOneBodyServesManyConnections(t *testing.T) {
	body, err := Render(NewDelta(mustChannel(t, "league:nfl"), json.RawMessage(`{"a":1}`)), frameAt, "")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	before := string(body)

	first := decodeFrame(t, body.Frame(1))
	second := decodeFrame(t, body.Frame(9_223_372_036_854_775_808)) // past int64, to prove uint64

	if string(body) != before {
		t.Fatalf("Body was mutated by Frame; every other connection's frame would be corrupted")
	}
	if first["seq"] != float64(1) {
		t.Errorf("first seq = %v, want 1", first["seq"])
	}
	if second["seq"] == first["seq"] {
		t.Errorf("both frames carry seq %v; the body is not being re-stamped", first["seq"])
	}
	if first["type"] != second["type"] || first["ts"] != second["ts"] {
		t.Errorf("the two frames disagree about the shared body: %v vs %v", first, second)
	}
}

// TestMarketPayloadIsPropagatedByteForByte pins the decision in protocol.go's
// header: the market document is carried through, never re-marshalled.
//
// The witness is a decimal that does not survive a float64 round trip in the
// form it was written. If anything in this package decoded and re-encoded the
// payload, the digits would change and this test would say so — which is exactly
// the class of silent defect a second mapping of the pricer's schema produces.
func TestMarketPayloadIsPropagatedByteForByte(t *testing.T) {
	const payload = `{"decimal":2.0000000000000004,"tiny":1e-320,"id":"mkt-1"}`

	raw, err := Frame(1, NewDelta(mustChannel(t, "market:mkt-1"), json.RawMessage(payload)), frameAt, "")
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	if !strings.Contains(string(raw), payload) {
		t.Fatalf("the market payload was re-encoded on the way out.\nwant substring: %s\ngot: %s", payload, raw)
	}
}

// TestEmptyCollectionsRenderAsArraysNotNull. A nil slice marshals to `null`, and
// a client that must special-case null where it expected an array eventually
// forgets to. An empty snapshot is a CORRECT answer for a channel with no
// markets, so it has to be representable without looking like an error.
func TestEmptyCollectionsRenderAsArraysNotNull(t *testing.T) {
	cases := map[string]FrameBody{
		"snapshot of an empty channel": NewSnapshot(mustChannel(t, "league:nfl"), nil),
		"ack with nothing accepted":    NewAck(nil, nil),
		"hello with nothing restored":  NewHello("c", "", 20*time.Second, false, false, nil),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			raw, err := Frame(1, body, frameAt, "")
			if err != nil {
				t.Fatalf("Frame: %v", err)
			}
			if strings.Contains(string(raw), "null") {
				t.Fatalf("an empty collection rendered as null: %s", raw)
			}
			decodeFrame(t, raw)
		})
	}
}

// TestRenderRefusesAFrameItCannotMakeValid. Both cases are hub bugs rather than
// client input, and both would otherwise produce a frame naming something that
// does not exist. CLAUDE.md §12 forbids a panic outside main, so they are errors.
func TestRenderRefusesAFrameItCannotMakeValid(t *testing.T) {
	if _, err := Render(nil, frameAt, ""); !errors.Is(err, ErrInvalidFrame) {
		t.Errorf("Render(nil) error = %v, want ErrInvalidFrame", err)
	}
	// A zero Channel can only come from a frame assembled without one.
	if _, err := Render(&DeltaFrame{}, frameAt, ""); !errors.Is(err, ErrInvalidFrame) {
		t.Errorf("Render(zero channel) error = %v, want ErrInvalidFrame", err)
	}
}

// TestClosedSetsAreClosed. Every one of these is a Prometheus label value, and
// the dashboards and alert rules select single values by exact string. A value
// added without a Valid() branch would pass silently everywhere else.
func TestClosedSetsAreClosed(t *testing.T) {
	for _, k := range MessageKinds() {
		if !k.Valid() || k.String() == "" {
			t.Errorf("MessageKind %q is listed but not valid", k)
		}
	}
	for _, k := range ClientKinds() {
		if !k.Valid() {
			t.Errorf("ClientKind %q is listed but not valid", k)
		}
	}
	for _, r := range DropReasons() {
		if !r.Valid() {
			t.Errorf("DropReason %q is listed but not valid", r)
		}
	}
	for _, r := range ResyncReasons() {
		if !r.Valid() {
			t.Errorf("ResyncReason %q is listed but not valid", r)
		}
	}
	for _, r := range ConnectionResults() {
		if !r.Valid() {
			t.Errorf("ConnectionResult %q is listed but not valid", r)
		}
	}
	for _, r := range RejectReasons() {
		if !r.Valid() {
			t.Errorf("RejectReason %q is listed but not valid", r)
		}
	}
	for _, o := range PresenceOps() {
		if !o.Valid() {
			t.Errorf("PresenceOp %q is listed but not valid", o)
		}
	}

	// The spellings the observability files select on. These are frozen; a
	// rename here is a rename in sharpline-alerts.yml.
	if string(DropSlowConsumer) != "slow_consumer" {
		t.Errorf("DropSlowConsumer = %q; WebSocketClientsDropping names slow_consumer", DropSlowConsumer)
	}
	if string(KindDelta) != "delta" {
		t.Errorf("KindDelta = %q; WebSocketResyncStorm selects kind=\"delta\"", KindDelta)
	}
}

func TestDecodeClient(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		maxBytes int
		wantErr  error
		want     ClientFrame
	}{
		{
			name: "subscribe",
			raw:  `{"type":"subscribe","id":"r1","channels":["market:a","league:nfl"]}`,
			want: ClientFrame{Type: ClientSubscribe, ID: "r1", Channels: []string{"market:a", "league:nfl"}},
		},
		{
			name: "unsubscribe",
			raw:  `{"type":"unsubscribe","channels":["market:a"]}`,
			want: ClientFrame{Type: ClientUnsubscribe, Channels: []string{"market:a"}},
		},
		{
			name: "resync with no channels means every channel",
			raw:  `{"type":"resync","id":"r2"}`,
			want: ClientFrame{Type: ClientResync, ID: "r2"},
		},
		{
			name: "ping",
			raw:  `{"type":"ping","id":"r3"}`,
			want: ClientFrame{Type: ClientPing, ID: "r3"},
		},
		{
			name:    "unknown type is named, not silently dropped",
			raw:     `{"type":"teleport"}`,
			wantErr: ErrUnknownFrameType,
		},
		{
			// The whole reason DisallowUnknownFields is on: this is a typo away
			// from correct and would otherwise subscribe to nothing.
			name:    "a singular channel field is refused rather than tolerated",
			raw:     `{"type":"subscribe","channel":"market:a"}`,
			wantErr: ErrMalformedFrame,
		},
		{
			name:    "not JSON",
			raw:     `{"type":`,
			wantErr: ErrMalformedFrame,
		},
		{
			name:    "trailing value",
			raw:     `{"type":"ping"}{"type":"ping"}`,
			wantErr: ErrMalformedFrame,
		},
		{
			name:    "subscribe with no channels",
			raw:     `{"type":"subscribe","channels":[]}`,
			wantErr: ErrMalformedFrame,
		},
		{
			name:    "ping carrying channels",
			raw:     `{"type":"ping","channels":["market:a"]}`,
			wantErr: ErrMalformedFrame,
		},
		{
			name:    "request id with a control byte",
			raw:     "{\"type\":\"ping\",\"id\":\"a\\nb\"}",
			wantErr: ErrMalformedFrame,
		},
		{
			name:     "over-long frame is refused, never truncated",
			raw:      `{"type":"ping","id":"r"}`,
			maxBytes: 4,
			wantErr:  ErrFrameTooLarge,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeClient([]byte(tc.raw), tc.maxBytes)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
				// Nothing a client sent may appear in an error that becomes a
				// log line. The unknown-type case is the one that carries a
				// list, and the list is OURS.
				if err != nil && strings.Contains(err.Error(), "teleport") {
					t.Errorf("the error echoes untrusted input: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Type != tc.want.Type || got.ID != tc.want.ID {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			if len(got.Channels) != len(tc.want.Channels) {
				t.Fatalf("channels = %v, want %v", got.Channels, tc.want.Channels)
			}
			for i := range got.Channels {
				if got.Channels[i] != tc.want.Channels[i] {
					t.Errorf("channels[%d] = %q, want %q", i, got.Channels[i], tc.want.Channels[i])
				}
			}
		})
	}
}

// TestDecodeClientNamesTheSupportedTypes. An unknown type is the one protocol
// failure with a genuinely useful answer, and the answer is the list.
func TestDecodeClientNamesTheSupportedTypes(t *testing.T) {
	_, err := DecodeClient([]byte(`{"type":"nope"}`), 0)
	if !errors.Is(err, ErrUnknownFrameType) {
		t.Fatalf("error = %v, want ErrUnknownFrameType", err)
	}
	for _, k := range ClientKinds() {
		if !strings.Contains(err.Error(), k.String()) {
			t.Errorf("the error does not name %q: %v", k, err)
		}
	}
}

func TestSafeEcho(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "printable passes through", in: "market:abc", want: "market:abc"},
		{name: "control bytes are replaced", in: "a\nb\x00c", want: "a?b?c"},
		{name: "high bytes are replaced", in: "caf\xc3\xa9", want: "caf??"},
		{
			name: "over-long is truncated and marked",
			in:   strings.Repeat("x", 100),
			want: strings.Repeat("x", 64) + "~",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SafeEcho(tc.in); got != tc.want {
				t.Errorf("SafeEcho(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSafeEchoSurvivesAFrameRoundTrip. The echoed value is untrusted OUTPUT as
// well as untrusted input; it goes back to the client on the ack's rejected
// list, so it has to survive encoding/json without breaking the frame.
func TestSafeEchoSurvivesAFrameRoundTrip(t *testing.T) {
	hostile := "\"},{\"seq\":999,\"type\":\"delta\",\"x\":\""
	raw, err := Frame(1, NewAck(nil, []RejectedChannel{{
		Channel: SafeEcho(hostile),
		Reason:  RejectMalformed,
	}}), frameAt, "")
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	got := decodeFrame(t, raw)
	if got["type"] != string(KindAck) {
		t.Fatalf("a crafted channel string changed the frame's type to %v", got["type"])
	}
	if got["seq"] != float64(1) {
		t.Fatalf("a crafted channel string changed the frame's sequence number to %v", got["seq"])
	}
}

func TestErrorCodeAndDropReasonMapping(t *testing.T) {
	cases := []struct {
		err      error
		wantCode ErrorCode
		wantDrop DropReason
	}{
		{err: ErrFrameTooLarge, wantCode: CodeFrameTooLarge, wantDrop: DropProtocolError},
		{err: ErrUnknownFrameType, wantCode: CodeUnknownType, wantDrop: DropProtocolError},
		{err: ErrMalformedFrame, wantCode: CodeMalformedFrame, wantDrop: DropProtocolError},
		{err: ErrChannelLimit, wantCode: CodeChannelLimit, wantDrop: DropProtocolError},
		{err: ErrInvalidChannel, wantCode: CodeInvalidChannel, wantDrop: DropProtocolError},
		{err: ErrInvalidCredential, wantCode: CodeUnauthorized, wantDrop: DropReadError},
		{err: ErrTokenInQuery, wantCode: CodeUnauthorized, wantDrop: DropReadError},
		// An unclassified failure is OURS, not the client's. Telling a client
		// author it was their fault sends them looking in the wrong place.
		{err: errors.New("something else"), wantCode: CodeInternal, wantDrop: DropReadError},
	}
	for _, tc := range cases {
		t.Run(tc.err.Error(), func(t *testing.T) {
			if got := ErrorCodeFor(tc.err); got != tc.wantCode {
				t.Errorf("ErrorCodeFor = %q, want %q", got, tc.wantCode)
			}
			if got := DropReasonFor(tc.err); got != tc.wantDrop {
				t.Errorf("DropReasonFor = %q, want %q", got, tc.wantDrop)
			}
		})
	}
	if got := ErrorCodeFor(nil); got != "" {
		t.Errorf("ErrorCodeFor(nil) = %q, want the empty code", got)
	}
}

// TestRequestIDBoundIsEnforced keeps the echo bounded at the door rather than at
// the point of use, so nothing downstream has to remember.
func TestRequestIDBoundIsEnforced(t *testing.T) {
	long := strings.Repeat("a", MaxRequestIDLen+1)
	if _, err := DecodeClient([]byte(`{"type":"ping","id":"`+long+`"}`), 0); !errors.Is(err, ErrMalformedFrame) {
		t.Errorf("an over-long request id was accepted")
	}
	ok := strings.Repeat("a", MaxRequestIDLen)
	if _, err := DecodeClient([]byte(`{"type":"ping","id":"`+ok+`"}`), 0); err != nil {
		t.Errorf("a request id at the limit was refused: %v", err)
	}
}

// TestChannelsRenderAsStrings. Channel is a struct; without MarshalText it would
// render as an object and every client would have to know the internal shape.
func TestChannelsRenderAsStrings(t *testing.T) {
	id, err := domain.NewMarketID("mkt-1")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := MarketChannel(id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Frame(1, NewAck([]Channel{ch}, nil), frameAt, "")
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	if !strings.Contains(string(raw), `"subscribed":["market:mkt-1"]`) {
		t.Fatalf("a channel did not render as its wire string: %s", raw)
	}
}

// -----------------------------------------------------------------------------
// Options
//
// These live here rather than in an options_test.go because this phase's file
// split gave options.go no test file of its own. They belong with the protocol
// because every value they validate is a REFUSAL threshold on the wire.
// -----------------------------------------------------------------------------

func TestOptionsNormaliseFillsEveryZeroValue(t *testing.T) {
	got := Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}.Normalise()

	if got.SendQueueCapacity != DefaultSendQueueCapacity {
		t.Errorf("SendQueueCapacity = %d, want %d", got.SendQueueCapacity, DefaultSendQueueCapacity)
	}
	if got.MaxChannelsPerConnection != DefaultMaxChannelsPerConnection {
		t.Errorf("MaxChannelsPerConnection = %d", got.MaxChannelsPerConnection)
	}
	if got.MaxFrameBytes != DefaultMaxFrameBytes {
		t.Errorf("MaxFrameBytes = %d", got.MaxFrameBytes)
	}
	if got.PingInterval != DefaultPingInterval || got.PongTimeout != DefaultPongTimeout {
		t.Errorf("heartbeat = %s/%s", got.PingInterval, got.PongTimeout)
	}
	if got.WriteTimeout != DefaultWriteTimeout || got.ShutdownDrain != DefaultShutdownDrain {
		t.Errorf("timeouts = %s/%s", got.WriteTimeout, got.ShutdownDrain)
	}
	if got.SubscriptionTTL != DefaultSubscriptionTTL {
		t.Errorf("SubscriptionTTL = %s", got.SubscriptionTTL)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("a fully defaulted Options does not validate: %v", err)
	}
}

// TestOptionsValidate. Configuration fails at construction, loudly, rather than
// at the first connection — and a NEGATIVE value is a mistake rather than a
// disable, which is why Normalise and Validate are separate steps.
func TestOptionsValidate(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	base := func() Options { return Options{Logger: log}.Normalise() }

	cases := []struct {
		name  string
		mutet func(*Options)
		want  bool
	}{
		{name: "defaults", mutet: func(*Options) {}},
		{name: "no logger", mutet: func(o *Options) { o.Logger = nil }, want: true},
		{name: "zero send queue", mutet: func(o *Options) { o.SendQueueCapacity = 0 }, want: true},
		{name: "negative send queue", mutet: func(o *Options) { o.SendQueueCapacity = -1 }, want: true},
		{name: "no channels allowed", mutet: func(o *Options) { o.MaxChannelsPerConnection = 0 }, want: true},
		{name: "frame ceiling below one subscribe", mutet: func(o *Options) { o.MaxFrameBytes = 8 }, want: true},
		{name: "no heartbeat", mutet: func(o *Options) { o.PingInterval = -1 }, want: true},
		{
			// Two pings could then be in flight and the timeout would be
			// measuring something other than what it says.
			name:  "pong timeout at or above the ping interval",
			mutet: func(o *Options) { o.PongTimeout = o.PingInterval },
			want:  true,
		},
		{name: "unbounded write", mutet: func(o *Options) { o.WriteTimeout = -1 }, want: true},
		{name: "no drain", mutet: func(o *Options) { o.ShutdownDrain = -1 }, want: true},
		{name: "no subscription ttl", mutet: func(o *Options) { o.SubscriptionTTL = -1 }, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := base()
			tc.mutet(&o)
			err := o.Validate()
			if tc.want && !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("error = %v, want ErrInvalidOptions", err)
			}
			if !tc.want && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestDefaultFrameCeilingAdmitsTheLargestLegitimateSubscribe. The ceiling is a
// refusal, so it has to be above anything a correct client sends — otherwise the
// gateway is configured to reject valid traffic and the failure looks like a
// client bug.
func TestDefaultFrameCeilingAdmitsTheLargestLegitimateSubscribe(t *testing.T) {
	channels := make([]string, MaxChannelsPerFrame)
	id := strings.Repeat("a", domain.MaxIDLen)
	for i := range channels {
		channels[i] = `"market:` + id + `"`
	}
	raw := `{"type":"subscribe","id":"` + strings.Repeat("r", MaxRequestIDLen) + `","channels":[` +
		strings.Join(channels, ",") + `]}`

	if _, err := DecodeClient([]byte(raw), DefaultMaxFrameBytes); err != nil {
		t.Fatalf("the default frame ceiling refuses the largest legitimate subscribe "+
			"(%d bytes against a %d ceiling): %v", len(raw), DefaultMaxFrameBytes, err)
	}
}
