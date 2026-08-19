package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestNewEmailNormalises(t *testing.T) {
	t.Parallel()

	// users_email_normalised: CHECK (email = lower(email)). Normalisation
	// happens here or the row is unstorable — and, worse, a login that looked
	// up the raw input would miss the row for a user who capitalised their
	// address and be indistinguishable from a wrong password.
	cases := []struct{ in, want string }{
		{"person@example.com", "person@example.com"},
		{"Person@Example.COM", "person@example.com"},
		{"  person@example.com  ", "person@example.com"},
		{"\tperson@example.com\n", "person@example.com"},
		{"PÄRSON@example.com", "pärson@example.com"},
		// Deliberately NOT normalised away: a '+tag' and dots in the local part
		// are provider-specific policy, and merging two addresses their owner
		// considers distinct is how one user claims another's account.
		{"person+tag@example.com", "person+tag@example.com"},
		{"first.last@example.com", "first.last@example.com"},
		// A single-label domain is deliverable on an internal network and the
		// schema's CHECK admits it, so this validator must too.
		{"root@localhost", "root@localhost"},
	}
	for _, c := range cases {
		got, err := NewEmail(c.in)
		if err != nil {
			t.Errorf("NewEmail(%q) = %v", c.in, err)
			continue
		}
		if got.String() != c.want {
			t.Errorf("NewEmail(%q) = %q, want %q", c.in, got, c.want)
		}
		if got.String() != strings.ToLower(got.String()) {
			t.Errorf("NewEmail(%q) produced a value users_email_normalised would refuse", c.in)
		}
	}
}

func TestNewEmailRejectsMalformedAddresses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want error
	}{
		{"empty", "", ErrEmailEmpty},
		{"whitespace only", "   ", ErrEmailEmpty},
		{"no at sign", "person.example.com", ErrEmailShape},
		{"two at signs", "a@b@c.com", ErrEmailShape},
		{"empty local part", "@example.com", ErrEmailShape},
		{"empty domain", "person@", ErrEmailShape},
		{"space inside", "per son@example.com", ErrEmailShape},
		{"newline inside", "person\n@example.com", ErrEmailShape},
		{"tab in domain", "person@exa\tmple.com", ErrEmailShape},
		{"control byte", "person@exam\x01ple.com", ErrEmailShape},
		{"invalid UTF-8", "person@examp\xffle.com", ErrEmailShape},
		{"too short", "a@", ErrEmailTooShort},
		{"too long", strings.Repeat("a", 250) + "@example.com", ErrEmailTooLong},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewEmail(c.in)
			if !errors.Is(err, c.want) {
				t.Fatalf("NewEmail(%q) = %v, want %v", c.in, err, c.want)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("NewEmail(%q) = %v, which is outside the ErrInvalid root", c.in, err)
			}
		})
	}
}

func TestNewEmailBoundsMatchTheSchema(t *testing.T) {
	t.Parallel()

	// users_email_length: CHECK (length(email) BETWEEN 3 AND 254).
	if MinEmailLen != 3 || MaxEmailLen != 254 {
		t.Fatalf("bounds are %d..%d; users_email_length is 3..254", MinEmailLen, MaxEmailLen)
	}

	// At the maximum exactly.
	atMax := strings.Repeat("a", MaxEmailLen-len("@example.com")) + "@example.com"
	if len(atMax) != MaxEmailLen {
		t.Fatalf("test address is %d bytes, want %d", len(atMax), MaxEmailLen)
	}
	if _, err := NewEmail(atMax); err != nil {
		t.Errorf("NewEmail at the maximum length = %v, want nil", err)
	}
	if _, err := NewEmail("a" + atMax); !errors.Is(err, ErrEmailTooLong) {
		t.Errorf("NewEmail one byte over = %v, want ErrEmailTooLong", err)
	}
}

// An email address is personal data. It is not a credential, so a truncated
// echo in a validation error is acceptable — but only truncated: the input is
// untrusted and unbounded.
func TestEmailErrTruncatesTheOffendingValue(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("z", 4096)
	err := emailErr(long, ErrEmailShape)
	if len(err.Error()) > 256 {
		t.Fatalf("the message is %d bytes; the address is not being truncated", len(err.Error()))
	}
	if !errors.Is(err, ErrEmailShape) {
		t.Fatal("emailErr dropped the wrapped sentinel")
	}
}

func TestEmailZeroValue(t *testing.T) {
	t.Parallel()

	var e Email
	if !e.IsZero() {
		t.Error("the zero Email reports non-zero")
	}
	got, err := NewEmail("person@example.com")
	if err != nil {
		t.Fatalf("NewEmail: %v", err)
	}
	if got.IsZero() {
		t.Error("a populated Email reports zero")
	}
}
