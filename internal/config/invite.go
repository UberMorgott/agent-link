package config

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// A project invite carries everything a member needs to join a project:
//
//	ALP1.<pid>.<epoch>.<secret>.<check>
//
// pid and secret are 16 random bytes each in unpadded RFC 4648 base32
// (A-Z, 2-7; 26 characters, canonical: upper case, unused trailing bits
// zero), epoch is decimal (ProjectEpoch), and check is the first 4 base32
// characters of SHA-256 over "ALP1.<pid>.<epoch>.<secret>", which catches
// typos. The secret has 128 bits, so the project key needs no slow KDF.
const (
	invitePrefix = "ALP1"
	// ProjectEpoch is the only secret epoch of v1 (no rotation yet).
	ProjectEpoch   = 1
	projectIDLen   = 16 // bytes
	projectB32Len  = 26 // characters of 16 bytes
	inviteCheckLen = 4
	projectKeyInfo = "agentlink/project-key/v1/epoch="
	projectTagInfo = "agentlink/project-tag/v1"
	projectTagLen  = 8
)

// ErrInvite: the text is not a valid project invite.
var ErrInvite = errors.New("invalid project invite")

// b32 decodes leniently (non-zero trailing bits); decodeB32 re-encodes to
// insist on the canonical form.
var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// Invite is a parsed project invite.
type Invite struct {
	ProjectID string // canonical base32
	Epoch     uint32
	Secret    string // canonical base32
}

func newB32() (string, error) {
	b := make([]byte, projectIDLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b32.EncodeToString(b), nil
}

// NewProjectID returns a fresh random project id.
func NewProjectID() (string, error) { return newB32() }

// NewProjectSecret returns a fresh random project secret.
func NewProjectSecret() (string, error) { return newB32() }

// decodeB32 decodes a canonical 26-character base32 string of 16 bytes.
func decodeB32(s string) ([]byte, bool) {
	if len(s) != projectB32Len {
		return nil, false
	}
	b, err := b32.DecodeString(s)
	if err != nil || len(b) != projectIDLen || b32.EncodeToString(b) != s {
		return nil, false
	}
	return b, true
}

// ValidProjectID reports a canonical project id.
func ValidProjectID(s string) bool {
	_, ok := decodeB32(s)
	return ok
}

// ValidProjectSecret reports a canonical project secret.
func ValidProjectSecret(s string) bool {
	_, ok := decodeB32(s)
	return ok
}

func inviteCheck(pid, epoch, secret string) string {
	sum := sha256.Sum256([]byte(invitePrefix + "." + pid + "." + epoch + "." + secret))
	return b32.EncodeToString(sum[:])[:inviteCheckLen]
}

// FormatInvite returns the canonical invite of a project. pid and secret must
// be canonical (ValidProjectID, ValidProjectSecret).
func FormatInvite(pid string, epoch uint32, secret string) string {
	e := strconv.FormatUint(uint64(epoch), 10)
	return invitePrefix + "." + pid + "." + e + "." + secret + "." + inviteCheck(pid, e, secret)
}

// ParseInvite reads an invite as a person may paste it: surrounding and inner
// whitespace and case do not matter; anything else must be exact.
func ParseInvite(s string) (Invite, error) {
	s = strings.ToUpper(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s))
	parts := strings.Split(s, ".")
	if len(parts) != 5 || parts[0] != invitePrefix {
		return Invite{}, ErrInvite
	}
	pid, epoch, secret, check := parts[1], parts[2], parts[3], parts[4]
	if epoch != strconv.Itoa(ProjectEpoch) {
		return Invite{}, fmt.Errorf("%w: epoch %q", ErrInvite, epoch)
	}
	if !ValidProjectID(pid) || !ValidProjectSecret(secret) {
		return Invite{}, ErrInvite
	}
	if check != inviteCheck(pid, epoch, secret) {
		return Invite{}, fmt.Errorf("%w: checksum", ErrInvite)
	}
	return Invite{ProjectID: pid, Epoch: ProjectEpoch, Secret: secret}, nil
}

// ProjectKey derives a project's 32-byte session key: HKDF-SHA256 of the
// secret's bytes, salted with the project id's bytes, bound to the epoch.
func ProjectKey(pid string, epoch uint32, secret string) ([]byte, error) {
	salt, ok := decodeB32(pid)
	if !ok {
		return nil, fmt.Errorf("invalid project id %q", pid)
	}
	ikm, ok := decodeB32(secret)
	if !ok {
		return nil, errors.New("invalid project secret")
	}
	return hkdf.Key(sha256.New, ikm, salt, projectKeyInfo+strconv.FormatUint(uint64(epoch), 10), 32)
}

// ProjectTag is a project's discovery tag in beacons: 8 bytes of HKDF of the
// project key, in hex. A heard tag gives nothing to guess against: the key
// comes from 128 random bits.
func ProjectTag(key []byte) string {
	b, err := hkdf.Key(sha256.New, key, nil, projectTagInfo, projectTagLen)
	if err != nil { // only for lengths HKDF cannot produce
		return ""
	}
	return hex.EncodeToString(b)
}
