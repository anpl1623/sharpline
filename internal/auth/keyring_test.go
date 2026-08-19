package auth

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/anpl1623/sharpline/internal/domain"
)

func testKeyring(t *testing.T, ids ...string) *Keyring {
	t.Helper()
	if len(ids) == 0 {
		ids = []string{"k1"}
	}
	keys := make(map[string][]byte, len(ids))
	for i, id := range ids {
		key := make([]byte, KeyLen)
		for j := range key {
			key[j] = byte(i*31 + j)
		}
		keys[id] = key
	}
	kr, err := NewKeyring(keys, ids[0])
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	return kr
}

func TestSealOpenRoundTrip(t *testing.T) {
	t.Parallel()

	kr := testKeyring(t)
	owner := domain.UserID("usr_abc")
	secret := []byte("12345678901234567890")

	sealed, err := kr.Seal(owner, secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed.Ciphertext, secret) {
		t.Fatal("the plaintext is visible in the ciphertext")
	}
	// user_totp_nonce_nonempty: CHECK (octet_length BETWEEN 12 AND 32).
	if n := len(sealed.Nonce); n < 12 || n > 32 {
		t.Fatalf("nonce is %d bytes; user_totp_nonce_nonempty admits 12..32", n)
	}
	// user_totp_ciphertext_nonempty.
	if len(sealed.Ciphertext) == 0 {
		t.Fatal("empty ciphertext")
	}
	if sealed.KeyID != kr.ActiveKeyID() {
		t.Fatalf("KeyID = %q, want the active key %q", sealed.KeyID, kr.ActiveKeyID())
	}

	got, err := kr.Open(owner, sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("round trip = %x, want %x", got, secret)
	}
}

// THE test this file exists for.
//
// migrations/00005 requirement 3 on this phase: "The AEAD's additional
// authenticated data MUST include user_id, so a row copied from one user to
// another fails to decrypt rather than silently granting the attacker's device
// the victim's second factor."
//
// The attack: somebody with one UPDATE on user_totp copies their own enrolment
// row onto the victim's user_id. Without the binding, their authenticator app
// is now the victim's second factor and nothing anywhere reports it.
func TestSealedRowCopiedToAnotherUserFailsToDecrypt(t *testing.T) {
	t.Parallel()

	kr := testKeyring(t)
	attacker := domain.UserID("usr_attacker")
	victim := domain.UserID("usr_victim")
	secret := []byte("12345678901234567890")

	sealed, err := kr.Seal(attacker, secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// The row, byte for byte, under a different user_id — exactly what an
	// UPDATE would produce.
	if _, err := kr.Open(victim, sealed); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("Open of a row copied to another user = %v, want ErrDecrypt", err)
	}
	// And the original still works, so the binding is a binding rather than a
	// broken seal.
	if _, err := kr.Open(attacker, sealed); err != nil {
		t.Fatalf("Open by the rightful owner = %v, want nil", err)
	}
}

func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	t.Parallel()

	kr := testKeyring(t)
	owner := domain.UserID("usr_abc")

	sealed, err := kr.Seal(owner, []byte("12345678901234567890"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	t.Run("flipped ciphertext bit", func(t *testing.T) {
		bad := sealed
		bad.Ciphertext = append([]byte(nil), sealed.Ciphertext...)
		bad.Ciphertext[0] ^= 1
		if _, err := kr.Open(owner, bad); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("Open = %v, want ErrDecrypt", err)
		}
	})

	t.Run("flipped nonce bit", func(t *testing.T) {
		bad := sealed
		bad.Nonce = append([]byte(nil), sealed.Nonce...)
		bad.Nonce[0] ^= 1
		if _, err := kr.Open(owner, bad); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("Open = %v, want ErrDecrypt", err)
		}
	})

	t.Run("truncated ciphertext", func(t *testing.T) {
		bad := sealed
		bad.Ciphertext = sealed.Ciphertext[:len(sealed.Ciphertext)-1]
		if _, err := kr.Open(owner, bad); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("Open = %v, want ErrDecrypt", err)
		}
	})

	t.Run("wrong nonce length", func(t *testing.T) {
		bad := sealed
		bad.Nonce = sealed.Nonce[:4]
		if _, err := kr.Open(owner, bad); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("Open = %v, want ErrDecrypt", err)
		}
	})

	t.Run("relabelled key id", func(t *testing.T) {
		// The key id is in the AAD, so a ciphertext cannot be relabelled to a
		// different key even when both keys are in the ring.
		two := testKeyring(t, "k1", "k2")
		s, err := two.Seal(owner, []byte("12345678901234567890"))
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		s.KeyID = "k2"
		if _, err := two.Open(owner, s); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("Open of a relabelled ciphertext = %v, want ErrDecrypt", err)
		}
	})
}

func TestOpenReportsAnUnknownKeyID(t *testing.T) {
	t.Parallel()

	kr := testKeyring(t)
	sealed, err := kr.Seal("usr_abc", []byte("12345678901234567890"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	sealed.KeyID = "retired-key"

	// Distinct from ErrDecrypt, because the operator action is different: a
	// failed decryption is tampering or a copied row, an unknown key id is a
	// rotation that dropped a key too early.
	if _, err := kr.Open("usr_abc", sealed); !errors.Is(err, ErrKeyUnknown) {
		t.Fatalf("Open with an unknown key id = %v, want ErrKeyUnknown", err)
	}
}

// The whole reason user_totp carries key_id: rotation must be a re-encrypt
// pass, not a forced 2FA reset for every user.
func TestKeyringRotationKeepsOldCiphertextReadable(t *testing.T) {
	t.Parallel()

	owner := domain.UserID("usr_abc")
	secret := []byte("12345678901234567890")

	old := testKeyring(t, "k1")
	sealed, err := old.Seal(owner, secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// The rotation: a new active key, the OLD one kept for reads. Keeping it is
	// the whole point — dropping it here is what would force every enrolled
	// user to re-enrol.
	rotated, err := NewKeyring(map[string][]byte{
		"k1": keyMaterial(0),
		"k2": keyMaterial(1),
	}, "k2")
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}

	if !rotated.NeedsReseal(sealed.KeyID) {
		t.Error("NeedsReseal did not flag a row sealed under the retired key")
	}
	got, err := rotated.Open(owner, sealed)
	if err != nil {
		t.Fatalf("Open under the rotated ring = %v; every enrolled user would have to re-enrol", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatal("the re-read secret does not match")
	}

	// The re-encrypt pass writes under the new key.
	resealed, err := rotated.Seal(owner, got)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if resealed.KeyID != "k2" {
		t.Fatalf("resealed under %q, want the active key k2", resealed.KeyID)
	}
	if rotated.NeedsReseal(resealed.KeyID) {
		t.Error("a freshly resealed row is still flagged for resealing")
	}
}

// keyMaterial reproduces testKeyring's derivation for a given position, so a
// test can build two rings that agree on a key.
func keyMaterial(pos int) []byte {
	key := make([]byte, KeyLen)
	for j := range key {
		key[j] = byte(pos*31 + j)
	}
	return key
}

func TestNewKeyringValidatesItsInput(t *testing.T) {
	t.Parallel()

	good := keyMaterial(0)

	cases := []struct {
		name     string
		keys     map[string][]byte
		activeID string
		want     error
	}{
		{"empty ring", map[string][]byte{}, "k1", ErrKeyringEmpty},
		{"active key absent", map[string][]byte{"k1": good}, "k2", ErrInvalid},
		{"short key", map[string][]byte{"k1": good[:16]}, "k1", ErrKeyLength},
		{"long key", map[string][]byte{"k1": append(append([]byte(nil), good...), 0)}, "k1", ErrKeyLength},
		// user_totp_key_id_shape: `^[A-Za-z0-9._-]{1,128}$`. A key id the
		// database would refuse is a 500 on an enrolment rather than a clear
		// failure at startup.
		{"key id with a colon", map[string][]byte{"k:1": good}, "k:1", ErrInvalid},
		{"empty key id", map[string][]byte{"": good}, "", ErrInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewKeyring(c.keys, c.activeID); !errors.Is(err, c.want) {
				t.Fatalf("NewKeyring = %v, want %v", err, c.want)
			}
		})
	}
}

func TestNewKeyringCopiesItsKeys(t *testing.T) {
	t.Parallel()

	key := keyMaterial(0)
	kr, err := NewKeyring(map[string][]byte{"k1": key}, "k1")
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	sealed, err := kr.Seal("usr_abc", []byte("12345678901234567890"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	for i := range key {
		key[i] = 0
	}
	if _, err := kr.Open("usr_abc", sealed); err != nil {
		t.Fatalf("Open after the caller zeroed its key slice = %v, want nil", err)
	}
}

func TestParseKeyring(t *testing.T) {
	t.Parallel()

	k1 := base64.StdEncoding.EncodeToString(keyMaterial(0))
	k2 := base64.RawStdEncoding.EncodeToString(keyMaterial(1))

	t.Run("single key", func(t *testing.T) {
		kr, err := ParseKeyring("k1:" + k1)
		if err != nil {
			t.Fatalf("ParseKeyring: %v", err)
		}
		if kr.ActiveKeyID() != "k1" {
			t.Fatalf("active key = %q, want k1", kr.ActiveKeyID())
		}
	})

	t.Run("the first entry is active", func(t *testing.T) {
		kr, err := ParseKeyring("k2:" + k2 + ",k1:" + k1)
		if err != nil {
			t.Fatalf("ParseKeyring: %v", err)
		}
		if kr.ActiveKeyID() != "k2" {
			t.Fatalf("active key = %q, want k2 (the first entry)", kr.ActiveKeyID())
		}
		if len(kr.KeyIDs()) != 2 {
			t.Fatalf("ring holds %d keys, want 2", len(kr.KeyIDs()))
		}
	})

	t.Run("unpadded base64 is accepted", func(t *testing.T) {
		if _, err := ParseKeyring("k2:" + k2); err != nil {
			t.Fatalf("ParseKeyring with unpadded base64 = %v", err)
		}
	})

	bad := []struct{ name, spec string }{
		{"empty", ""},
		{"whitespace", "   "},
		{"no colon", k1},
		{"not base64", "k1:!!!!not-base64!!!!"},
		{"wrong length", "k1:" + base64.StdEncoding.EncodeToString(make([]byte, 16))},
		{"duplicate id", "k1:" + k1 + ",k1:" + k2},
		{"bad id", "k:1:" + k1},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseKeyring(c.spec)
			if err == nil {
				t.Fatal("ParseKeyring accepted a malformed specification")
			}
			// The key material must never appear in the error message. A
			// startup failure is exactly the log line an operator pastes into a
			// ticket.
			if strings.Contains(err.Error(), k1) || strings.Contains(err.Error(), k2) {
				t.Fatalf("the error message quotes key material: %v", err)
			}
		})
	}
}

func TestKeyringRedactsItself(t *testing.T) {
	t.Parallel()

	kr := testKeyring(t, "k1", "k2")
	material := string(keyMaterial(0))

	for _, s := range []string{
		fmt.Sprintf("%v", kr),
		fmt.Sprintf("%s", kr), //nolint:staticcheck // S1025: exercising fmt's dispatch is the assertion
		fmt.Sprintf("%#v", kr),
		fmt.Sprintf("%+v", kr),
	} {
		if strings.Contains(s, material) {
			t.Fatalf("the keyring printed its key material: %s", s)
		}
		if !strings.Contains(s, "k1") {
			t.Errorf("the keyring did not report its active key id: %s", s)
		}
	}
}

func TestSealRejectsAnEmptyOwnerOrPlaintext(t *testing.T) {
	t.Parallel()

	kr := testKeyring(t)
	if _, err := kr.Seal("", []byte("secret")); !errors.Is(err, ErrInvalid) {
		t.Errorf("Seal with no owner = %v, want ErrInvalid", err)
	}
	if _, err := kr.Seal("usr_abc", nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("Seal with no plaintext = %v, want ErrInvalid", err)
	}
	sealed, err := kr.Seal("usr_abc", []byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := kr.Open("", sealed); !errors.Is(err, ErrInvalid) {
		t.Errorf("Open with no owner = %v, want ErrInvalid", err)
	}
}

// Two seals of one plaintext must differ. GCM nonce reuse under one key is a
// catastrophic, silent break.
func TestSealUsesAFreshNoncePerCall(t *testing.T) {
	t.Parallel()

	kr := testKeyring(t)
	seen := make(map[string]bool)
	for i := 0; i < 64; i++ {
		sealed, err := kr.Seal("usr_abc", []byte("12345678901234567890"))
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		key := string(sealed.Nonce)
		if seen[key] {
			t.Fatal("a GCM nonce repeated under one key")
		}
		seen[key] = true
	}
}
