package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"

	"github.com/anpl1623/sharpline/internal/domain"
)

// KeyLen is the width of an encryption key: 32 bytes, for AES-256.
const KeyLen = 32

// aadLabel binds a ciphertext to its purpose.
//
// It is part of the additional authenticated data, so a ciphertext produced for
// some other use of this keyring cannot be pasted into user_totp.
// secret_ciphertext and decrypt. There is only one use today; the label costs
// nothing and makes adding a second one safe by construction rather than by
// remembering to add a label at that point.
const aadLabel = "sharpline/auth/totp-secret/v1"

// Keyring holds the symmetric keys that encrypt credential material at rest,
// and names which one is current.
//
// # Why a ring and not a key
//
// migrations/00005 asked for this by name and gave the reason:
//
//	"`key_id` names which key encrypted the row, so a key rotation is a
//	 re-encrypt pass rather than a forced 2FA reset for every user. Do not drop
//	 this column because there is only one key today."
//
// A single-key design makes rotation an outage: the moment the key changes,
// every enrolled user's second factor stops decrypting and every one of them
// has to re-enrol from scratch. With a ring, rotation is: add the new key as
// active, keep the old one for reads, re-encrypt rows in the background, then
// drop the old key. No user notices.
//
// # Where the key comes from
//
// From the environment, never from the database — that is the whole point.
// migrations/00005: "a key that lives in a Kubernetes Secret / .env and NEVER
// in the database. A column holding the raw secret would mean a single
// SELECT-only leak is a permanent, silent full 2FA bypass for every user at
// once."
//
// The variable is SHARPLINE_TOTP_ENCRYPTION_KEYS and its format is what
// [ParseKeyring] accepts. It is NOT yet declared in
// internal/platform/config — that file is not owned by this phase's auth work —
// and the handoff notes name it as the one configuration change this package
// needs. Until it exists, a caller supplies the ring directly, which is what
// the tests do.
//
// # Why not derive it from SHARPLINE_JWT_SIGNING_KEY
//
// Because rotating one would then rotate the other. The JWT key is expected to
// be rotated on any suspicion, and the cost of rotating it is "everyone's
// access token expires early", which is minutes. If the TOTP key were derived
// from it, that same rotation would invalidate every 2FA enrolment. Two keys,
// two lifetimes, two blast radii.
type Keyring struct {
	// keys maps key id to key material. Unexported and never exposed; the
	// struct has no accessor that returns a key.
	keys map[string][]byte

	// activeID names the key new ciphertext is written under.
	activeID string
}

// NewKeyring builds a ring from an id-to-key map and names the active key.
//
// The keys are copied, so a caller that zeroes or reuses its buffers cannot
// change the ring underneath a running service.
func NewKeyring(keys map[string][]byte, activeID string) (*Keyring, error) {
	if len(keys) == 0 {
		return nil, ErrKeyringEmpty
	}
	if _, ok := keys[activeID]; !ok {
		return nil, fmt.Errorf("%w: active key id %q is not in the ring", ErrInvalid, sample(activeID))
	}

	copied := make(map[string][]byte, len(keys))
	for id, key := range keys {
		if err := validKeyID(id); err != nil {
			return nil, err
		}
		if len(key) != KeyLen {
			// The LENGTH is reported and the key is not. There is no branch in
			// this package that prints key material.
			return nil, fmt.Errorf("%w: key %q is %d bytes", ErrKeyLength, sample(id), len(key))
		}
		buf := make([]byte, KeyLen)
		copy(buf, key)
		copied[id] = buf
	}

	return &Keyring{keys: copied, activeID: activeID}, nil
}

// ParseKeyring reads a ring from the frozen environment format:
//
//	id:base64key[,id:base64key...]
//
// The FIRST entry is the active key; the rest are kept for decryption only.
// Ordering carries the meaning because the alternative — a second variable
// naming the active id — is a second thing to get wrong, and the failure mode
// of getting it wrong (writing under a key that is not in the ring) is a
// service that starts and then cannot decrypt what it just wrote.
//
// The key is base64 (standard alphabet, padding optional) of exactly 32 bytes.
// Generate one the way .env.example generates the JWT key, in a container:
//
//	docker run --rm alpine sh -c 'head -c 32 /dev/urandom | base64'
//
// Errors never quote the key material — only the id and the decoded length.
func ParseKeyring(spec string) (*Keyring, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, ErrKeyringEmpty
	}

	keys := make(map[string][]byte)
	var activeID string

	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, encoded, ok := strings.Cut(entry, ":")
		if !ok {
			return nil, ErrKeyringFormat
		}
		id = strings.TrimSpace(id)
		if err := validKeyID(id); err != nil {
			return nil, err
		}
		if _, dup := keys[id]; dup {
			return nil, fmt.Errorf("%w: key id %q appears twice", ErrInvalid, sample(id))
		}

		key, err := decodeKey(strings.TrimSpace(encoded))
		if err != nil {
			return nil, fmt.Errorf("%w: key %q: %w", ErrKeyringFormat, sample(id), err)
		}
		keys[id] = key
		if activeID == "" {
			activeID = id
		}
	}

	if activeID == "" {
		return nil, ErrKeyringEmpty
	}
	return NewKeyring(keys, activeID)
}

// decodeKey accepts padded or unpadded standard base64 and requires exactly
// [KeyLen] bytes. Raw (unpadded) is tried second because a hand-generated key
// almost always carries padding.
func decodeKey(encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil {
		// The decoder's message quotes the offending input, which is key
		// material. It is discarded.
		return nil, fmt.Errorf("%w: not base64", ErrKeyringFormat)
	}
	if len(key) != KeyLen {
		return nil, fmt.Errorf("%w (decoded to %d)", ErrKeyLength, len(key))
	}
	return key, nil
}

// validKeyID enforces user_totp_key_id_shape: `^[A-Za-z0-9._-]{1,128}$`. A key
// id that fails this is one the database would refuse at INSERT, which is a 500
// on an enrolment rather than a clear failure at startup.
func validKeyID(id string) error {
	if id == "" || len(id) > domain.MaxIDLen {
		return fmt.Errorf("%w: key id %q is empty or over %d bytes", ErrInvalid, sample(id), domain.MaxIDLen)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.'
		if !ok {
			return fmt.Errorf("%w: key id %q contains a byte outside [A-Za-z0-9._-]", ErrInvalid, sample(id))
		}
	}
	return nil
}

// ActiveKeyID returns the id new ciphertext is written under. Safe to log: it
// is a name, not a key.
func (kr *Keyring) ActiveKeyID() string { return kr.activeID }

// KeyIDs returns every id in the ring, for an operator confirming that a
// rotation's old key is still present before the re-encrypt pass finishes.
func (kr *Keyring) KeyIDs() []string {
	ids := make([]string, 0, len(kr.keys))
	for id := range kr.keys {
		ids = append(ids, id)
	}
	return ids
}

// LogValue implements slog.LogValuer. A Keyring logged whole reports its shape
// and never its contents.
func (kr *Keyring) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("active_key_id", kr.activeID),
		slog.Int("key_count", len(kr.keys)),
	)
}

// String implements fmt.Stringer so %v and %s cannot reflect into the map.
func (kr *Keyring) String() string {
	return fmt.Sprintf("Keyring{active=%s, keys=%d}", kr.activeID, len(kr.keys))
}

// GoString implements fmt.GoStringer so %#v cannot either.
func (kr *Keyring) GoString() string { return kr.String() }

// Sealed is one AEAD ciphertext and everything needed to open it again. The
// three fields map one-to-one onto user_totp.secret_ciphertext,
// user_totp.secret_nonce and user_totp.key_id.
type Sealed struct {
	Ciphertext []byte
	Nonce      []byte
	KeyID      string
}

// Seal encrypts plaintext under the active key, bound to owner.
//
// # The binding is the requirement, not a nicety
//
// migrations/00005, requirement 3 on this phase:
//
//	"The AEAD's additional authenticated data MUST include user_id, so a row
//	 copied from one user to another fails to decrypt rather than silently
//	 granting the attacker's device the victim's second factor."
//
// That is the attack this parameter exists for. Without the binding, an
// attacker with one UPDATE on user_totp copies their own enrolment row onto the
// victim's user_id and now their authenticator app is the victim's second
// factor — with no password needed if the same actor already has the password
// hash. With the binding, the copied row fails authentication and the victim's
// 2FA breaks loudly instead of being silently transferred.
//
// The AAD also carries the key id, so a ciphertext cannot be relabelled to a
// different key, and a purpose label, so it cannot be moved to a different use
// of the same ring.
//
// AES-256-GCM is chosen over XChaCha20-Poly1305 on the deploy target's
// hardware: the Ampere A1's Neoverse N1 cores implement the ARMv8 AES
// instructions, so AES-GCM is the faster of the two AND is entirely in the
// standard library, which keeps go.mod unchanged. The 96-bit nonce is drawn
// fresh from crypto/rand per seal; at that width the safe budget is ~2^32
// messages under one key, and the number of TOTP enrolments this system will
// ever write is not close.
func (kr *Keyring) Seal(owner domain.UserID, plaintext []byte) (Sealed, error) {
	if owner.IsZero() {
		return Sealed{}, fmt.Errorf("%w: sealing without an owner", ErrInvalid)
	}
	if len(plaintext) == 0 {
		// user_totp_ciphertext_nonempty would refuse the row anyway; failing
		// here says why.
		return Sealed{}, fmt.Errorf("%w: sealing an empty plaintext", ErrInvalid)
	}

	aead, err := kr.aead(kr.activeID)
	if err != nil {
		return Sealed{}, err
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Sealed{}, fmt.Errorf("%w: reading AEAD nonce: %w", ErrInternal, err)
	}

	ct := aead.Seal(nil, nonce, plaintext, aad(kr.activeID, owner))
	return Sealed{Ciphertext: ct, Nonce: nonce, KeyID: kr.activeID}, nil
}

// Open decrypts a sealed value, requiring that it was sealed for owner.
//
// A failure is [ErrDecrypt] and says nothing about which check failed: a
// tampered ciphertext, a wrong nonce and a row copied from another user are
// indistinguishable to the caller, because AEAD authentication is
// all-or-nothing and there is nothing useful to distinguish.
func (kr *Keyring) Open(owner domain.UserID, sealed Sealed) ([]byte, error) {
	if owner.IsZero() {
		return nil, fmt.Errorf("%w: opening without an owner", ErrInvalid)
	}

	aead, err := kr.aead(sealed.KeyID)
	if err != nil {
		return nil, err
	}
	if len(sealed.Nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("%w: nonce is %d bytes, want %d", ErrDecrypt, len(sealed.Nonce), aead.NonceSize())
	}

	plaintext, err := aead.Open(nil, sealed.Nonce, sealed.Ciphertext, aad(sealed.KeyID, owner))
	if err != nil {
		// The underlying error is "cipher: message authentication failed" and
		// carries nothing; it is dropped rather than wrapped so the sentinel is
		// the whole story.
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// NeedsReseal reports whether a stored row was sealed under a key that is no
// longer active — the query a background re-encrypt pass drives off during a
// rotation.
func (kr *Keyring) NeedsReseal(keyID string) bool { return keyID != kr.activeID }

// aead builds the cipher for a key id.
func (kr *Keyring) aead(keyID string) (cipher.AEAD, error) {
	key, ok := kr.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("%w: key id %q", ErrKeyUnknown, sample(keyID))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		// Unreachable: NewKeyring already refused any key that is not 32 bytes,
		// and that is the only input aes.NewCipher rejects.
		return nil, fmt.Errorf("%w: building AES cipher: %w", ErrInternal, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: building GCM: %w", ErrInternal, err)
	}
	return aead, nil
}

// aad builds the additional authenticated data.
//
// The three components are joined with a byte that cannot appear in any of them
// — key ids are `[A-Za-z0-9._-]`, user ids are the same charset, and the label
// is a constant — so the encoding is unambiguous and no pair of distinct
// (label, key, owner) triples can produce the same AAD. Concatenating without a
// separator would allow exactly that, which is how a binding stops binding.
func aad(keyID string, owner domain.UserID) []byte {
	var sb strings.Builder
	sb.Grow(len(aadLabel) + len(keyID) + len(owner) + 2)
	sb.WriteString(aadLabel)
	sb.WriteByte(0)
	sb.WriteString(keyID)
	sb.WriteByte(0)
	sb.WriteString(owner.String())
	return []byte(sb.String())
}
