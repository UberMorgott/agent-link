package node

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"errors"

	"github.com/gtank/ristretto255"
)

// CPace (draft-irtf-cfrg-cpace-20, initiator-responder mode) over
// ristretto255 with SHA-512, keyed by the session key (the pairing code's
// HKDF output) as its password-related string. Each side sends one group
// element; a transcript lets an attacker test exactly one code guess per
// connection it takes part in, and a recorded handshake tests none. The
// intermediate session key (ISK) then keys a confirmation MAC in each
// direction and the record keys of the session (wire.go).
const (
	cpaceDSI      = "CPaceRistretto255"
	cpaceDSIISK   = "CPaceRistretto255_ISK"
	cpaceSInBytes = 128 // SHA-512 block size
	// cpaceCI is the channel identifier: this protocol and version.
	cpaceCI = "agentlink/cpace/v1"
)

// errPAKE: the peer's element is not a valid group element, or the shared
// point is the identity.
var errPAKE = errors.New("invalid PAKE share")

// prependLen prefixes b with its length as LEB128.
func prependLen(b []byte) []byte {
	var out []byte
	for n := len(b); ; {
		c := byte(n & 0x7f)
		n >>= 7
		if n == 0 {
			out = append(out, c)
			break
		}
		out = append(out, c|0x80)
	}
	return append(out, b...)
}

// lvCat is the draft's lv_cat: every part prefixed by its length.
func lvCat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, prependLen(p)...)
	}
	return out
}

// cpaceGeneratorString is generator_string(DSI, PRS, CI, sid, s_in_bytes).
func cpaceGeneratorString(prs, ci, sid []byte) []byte {
	zpad := max(0, cpaceSInBytes-len(prependLen(prs))-len(prependLen([]byte(cpaceDSI)))-1)
	return lvCat([]byte(cpaceDSI), prs, make([]byte, zpad), ci, sid)
}

// cpaceGenerator is calculate_generator: SHA-512 of the generator string,
// mapped to the group by RFC 9496 element derivation.
func cpaceGenerator(prs, ci, sid []byte) (*ristretto255.Element, error) {
	h := sha512.Sum512(cpaceGeneratorString(prs, ci, sid))
	return ristretto255.NewIdentityElement().SetUniformBytes(h[:])
}

// cpace is one side of a CPace run.
type cpace struct {
	sid   []byte
	y     *ristretto255.Scalar
	share []byte // Y = y*g, encoded
}

// projectCI is the channel identifier of a project's sessions: it binds the
// project id into the generator, so the same secret under another project id
// never yields a matching share.
func projectCI(pid string) []byte { return []byte("agentlink/cpace/project/v1\x00" + pid) }

// newCPace starts a run with a random scalar under channel identifier ci.
func newCPace(prs, ci, sid []byte) (*cpace, error) {
	var b [64]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	y, err := ristretto255.NewScalar().SetUniformBytes(b[:])
	if err != nil {
		return nil, err
	}
	return newCPaceScalar(prs, ci, sid, y)
}

func newCPaceScalar(prs, ci, sid []byte, y *ristretto255.Scalar) (*cpace, error) {
	g, err := cpaceGenerator(prs, ci, sid)
	if err != nil {
		return nil, err
	}
	return &cpace{sid: sid, y: y, share: ristretto255.NewIdentityElement().ScalarMult(y, g).Bytes()}, nil
}

// secret is scalar_mult_vfy(y, peer): it refuses a non-canonical encoding
// and an identity result.
func (c *cpace) secret(peer []byte) ([]byte, error) {
	p, err := ristretto255.NewIdentityElement().SetCanonicalBytes(peer)
	if err != nil {
		return nil, errPAKE
	}
	k := ristretto255.NewIdentityElement().ScalarMult(c.y, p)
	if k.Equal(ristretto255.NewIdentityElement()) == 1 {
		return nil, errPAKE
	}
	return k.Bytes(), nil
}

// cpaceISK is the initiator-responder ISK: H(lv_cat(DSI_ISK, sid, K) ||
// lv_cat(Ya, ADa) || lv_cat(Yb, ADb)).
func cpaceISK(sid, k, ya, ada, yb, adb []byte) []byte {
	h := sha512.New()
	h.Write(lvCat([]byte(cpaceDSIISK), sid, k))
	h.Write(lvCat(ya, ada))
	h.Write(lvCat(yb, adb))
	return h.Sum(nil)
}

// sessionKeys are what one CPace run yields: the acceptor's confirmation tag,
// the key of the dialer's tag and the ISK the record keys come from.
type sessionKeys struct {
	acceptorTag []byte
	dialerKey   []byte
	isk         []byte
}

// channelKeys are the record keys of a sealed session, one per direction.
type channelKeys struct {
	dialerToAcceptor, acceptorToDialer []byte
}

// transcript hashes the raw hello lines of both sides. The dialer's tag and
// the record keys bind it, so a hello altered on the way (areas, caps, app,
// port) fails the handshake or the first record.
func transcript(dialerHello, acceptorHello []byte) []byte {
	h := sha256.Sum256(lvCat([]byte("agentlink/transcript/v1"), dialerHello, acceptorHello))
	return h[:]
}

// dialerTag is the dialer's confirmation tag over the transcript th. The
// dialer sends it last, after checking the acceptor's tag.
func (k sessionKeys) dialerTag(th []byte) []byte {
	h := hmac.New(sha256.New, k.dialerKey)
	h.Write([]byte("agentlink/cpace/confirm/dialer"))
	h.Write(th)
	return h.Sum(nil)
}

// channel derives the record keys from the ISK, salted with the transcript th.
func (k sessionKeys) channel(th []byte) (channelKeys, error) {
	d2a, err := hkdf.Key(sha256.New, k.isk, th, "agentlink/records/dialer-to-acceptor", 32)
	if err != nil {
		return channelKeys{}, err
	}
	a2d, err := hkdf.Key(sha256.New, k.isk, th, "agentlink/records/acceptor-to-dialer", 32)
	if err != nil {
		return channelKeys{}, err
	}
	return channelKeys{dialerToAcceptor: d2a, acceptorToDialer: a2d}, nil
}

// keys finishes the run given the peer's share. The dialer is the initiator
// (A), the acceptor the responder (B); adDialer/adAcceptor bind each side's
// name and node id.
func (c *cpace) keys(peer []byte, dialer bool, adDialer, adAcceptor []byte) (sessionKeys, error) {
	k, err := c.secret(peer)
	if err != nil {
		return sessionKeys{}, err
	}
	ya, yb := c.share, peer
	if !dialer {
		ya, yb = peer, c.share
	}
	isk := cpaceISK(c.sid, k, ya, adDialer, yb, adAcceptor)
	derive := func(info string) ([]byte, error) { return hkdf.Key(sha256.New, isk, nil, info, 32) }
	kd, err := derive("agentlink/cpace/confirm/dialer")
	if err != nil {
		return sessionKeys{}, err
	}
	ka, err := derive("agentlink/cpace/confirm/acceptor")
	if err != nil {
		return sessionKeys{}, err
	}
	h := hmac.New(sha256.New, ka)
	h.Write([]byte("agentlink/cpace/confirm/acceptor"))
	return sessionKeys{acceptorTag: h.Sum(nil), dialerKey: kd, isk: isk}, nil
}

// cpaceAD is a side's associated data: its name and node id.
func cpaceAD(name, id string) []byte { return lvCat([]byte(name), []byte(id)) }
