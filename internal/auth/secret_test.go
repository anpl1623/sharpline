package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// The point of these tests is narrow and worth stating: they assert that
// redaction is STRUCTURAL. Every one of them takes a route that a developer
// might reach for at 2am — a %v in a log line, an slog.Any on a whole struct, a
// json.Marshal of a response — and shows that the route cannot produce the
// value. A comment saying "do not log the token" is not testable; this is.

const probe = "sk-super-secret-value"

func TestSecretRedactsThroughEveryFormattingVerb(t *testing.T) {
	t.Parallel()

	s := NewSecret(probe)

	cases := []struct {
		verb string
		got  string
	}{
		{"%v", fmt.Sprintf("%v", s)},
		// %s on a value that has String() is what staticcheck's S1025 asks you
		// to replace with a direct String() call. Here it is the POINT: the
		// test is that fmt's verb dispatch finds the method, not that String()
		// works when called explicitly.
		{"%s", fmt.Sprintf("%s", s)}, //nolint:staticcheck // S1025: exercising fmt's dispatch is the assertion
		{"%q", fmt.Sprintf("%q", s)},
		{"%#v", fmt.Sprintf("%#v", s)},
		{"%+v", fmt.Sprintf("%+v", s)},
		{"Sprint", fmt.Sprint(s)},
		{"Errorf", fmt.Errorf("wrapping %s", s).Error()},
	}
	for _, c := range cases {
		if strings.Contains(c.got, probe) {
			t.Errorf("%s leaked the secret: %s", c.verb, c.got)
		}
		if !strings.Contains(c.got, Redacted) {
			t.Errorf("%s = %q, want it to contain %q", c.verb, c.got, Redacted)
		}
	}
}

// A Secret inside a struct is the realistic case: nobody logs a bare token,
// they log the session that holds one.
func TestSecretRedactsWhenNestedInAStruct(t *testing.T) {
	t.Parallel()

	type response struct {
		UserID  string
		Access  Secret
		Refresh Secret
	}
	r := response{UserID: "usr_abc", Access: NewSecret(probe), Refresh: NewSecret("refresh-" + probe)}

	for _, verb := range []string{"%v", "%+v", "%#v"} {
		got := fmt.Sprintf(verb, r)
		if strings.Contains(got, probe) {
			t.Errorf("%s on a struct leaked: %s", verb, got)
		}
		if !strings.Contains(got, "usr_abc") {
			t.Errorf("%s on a struct dropped the non-secret field: %s", verb, got)
		}
	}
}

func TestSecretRedactsThroughSlog(t *testing.T) {
	t.Parallel()

	type session struct {
		UserID string
		Token  Secret
	}

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("issued",
		slog.String("user_id", "usr_abc"),
		slog.Any("token", NewSecret(probe)),
		slog.Any("session", session{UserID: "usr_abc", Token: NewSecret(probe)}),
	)

	out := buf.String()
	if strings.Contains(out, probe) {
		t.Fatalf("slog leaked the secret: %s", out)
	}
	if !strings.Contains(out, Redacted) {
		t.Fatalf("slog output has no redaction marker: %s", out)
	}
	// The zero Secret must redact too: an error path returns a struct whose
	// secrets are unset, and those must be as unprintable as populated ones.
	buf.Reset()
	log.Info("empty", slog.Any("token", Secret{}))
	if !strings.Contains(buf.String(), Redacted) {
		t.Fatalf("the zero Secret did not redact: %s", buf.String())
	}
}

// Marshalling REFUSES rather than redacting. See the type comment: a silent
// placeholder is safe and quiet; an error is safe and loud, and the only
// legitimate serialisation of a secret is one deliberate Expose() at the
// transport boundary.
func TestSecretRefusesToMarshal(t *testing.T) {
	t.Parallel()

	if _, err := json.Marshal(NewSecret(probe)); !errors.Is(err, ErrSecretNotShown) {
		t.Fatalf("json.Marshal(Secret) error = %v, want ErrSecretNotShown", err)
	}

	type response struct {
		UserID string `json:"user_id"`
		Token  Secret `json:"token"`
	}
	out, err := json.Marshal(response{UserID: "usr_abc", Token: NewSecret(probe)})
	if err == nil {
		t.Fatalf("json.Marshal of a struct containing a Secret succeeded: %s", out)
	}
	if !errors.Is(err, ErrSecretNotShown) {
		t.Fatalf("json.Marshal error = %v, want it to wrap ErrSecretNotShown", err)
	}

	if _, err := NewSecret(probe).MarshalText(); !errors.Is(err, ErrSecretNotShown) {
		t.Fatalf("MarshalText error = %v, want ErrSecretNotShown", err)
	}
}

// Decoding INTO a Secret is safe and must work: a login request body carries a
// password. The asymmetry with marshalling is the design.
func TestSecretUnmarshalsFromJSON(t *testing.T) {
	t.Parallel()

	type request struct {
		Password Secret `json:"password"`
	}

	// A passphrase containing a quote, a backslash and a non-BMP rune. A
	// hand-rolled unquote would silently register a DIFFERENT password than the
	// user typed, and the user would discover it as "I cannot log in".
	want := `he said "hi"\ 🎲 pässwörd`
	body, err := json.Marshal(map[string]string{"password": want})
	if err != nil {
		t.Fatalf("building the request body: %v", err)
	}

	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := req.Password.Expose(); got != want {
		t.Fatalf("round trip = %q, want %q", got, want)
	}
}

func TestSecretRejectsNonStringJSON(t *testing.T) {
	t.Parallel()

	for _, body := range []string{`123`, `null`, `{}`, `[]`, `true`} {
		var s Secret
		if err := json.Unmarshal([]byte(body), &s); err == nil {
			t.Errorf("Unmarshal(%s) succeeded, want an error", body)
		}
	}
}

func TestSecretEqualIsContentComparison(t *testing.T) {
	t.Parallel()

	a := NewSecret("abcdef")
	b := NewSecret("abcdef")
	c := NewSecret("abcdeg")
	d := NewSecret("abcde")

	if !a.Equal(b) {
		t.Error("equal secrets compared unequal")
	}
	if a.Equal(c) {
		t.Error("secrets differing in one byte compared equal")
	}
	if a.Equal(d) {
		t.Error("secrets of different length compared equal")
	}
	if !(Secret{}).Equal(Secret{}) {
		t.Error("two zero secrets compared unequal")
	}
}

func TestSecretLenAndIsZero(t *testing.T) {
	t.Parallel()

	if !(Secret{}).IsZero() {
		t.Error("the zero Secret reports non-zero")
	}
	if NewSecret("x").IsZero() {
		t.Error("a populated Secret reports zero")
	}
	if got := NewSecret(probe).Len(); got != len(probe) {
		t.Errorf("Len = %d, want %d", got, len(probe))
	}
}
