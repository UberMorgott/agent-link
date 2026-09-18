package node

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/gtank/ristretto255"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The ristretto255/SHA-512 test vectors of draft-irtf-cfrg-cpace-20, appendix B.3.
func TestCPaceDraftVectors(t *testing.T) {
	prs, ci := []byte("Password"), unhex(t, "0b415f696e69746961746f720b425f726573706f6e646572")
	sid := unhex(t, "7e4b4791d6a8ef019b936c79fb7f2c57")
	gs := cpaceGeneratorString(prs, ci, sid)
	if len(gs) != 170 {
		t.Fatalf("generator string length %d", len(gs))
	}
	g, err := cpaceGenerator(prs, ci, sid)
	if err != nil {
		t.Fatal(err)
	}
	if want := unhex(t, "222b6b195fe84b1652badb6f6a3ae3d24341e7306967f0b8115b40d5698c7e56"); !bytes.Equal(g.Bytes(), want) {
		t.Fatalf("generator %x", g.Bytes())
	}
	scalar := func(s string) *ristretto255.Scalar {
		y, err := ristretto255.NewScalar().SetCanonicalBytes(unhex(t, s))
		if err != nil {
			t.Fatal(err)
		}
		return y
	}
	ya := scalar("da3d23700a9e5699258aef94dc060dfda5ebb61f02a5ea77fad53f4ff0976d08")
	yb := scalar("d2316b454718c35362d83d69df6320f38578ed5984651435e2949762d900b80d")
	a := &cpace{sid: sid, y: ya, share: ristretto255.NewIdentityElement().ScalarMult(ya, g).Bytes()}
	b := &cpace{sid: sid, y: yb, share: ristretto255.NewIdentityElement().ScalarMult(yb, g).Bytes()}
	if want := unhex(t, "d6bac480f2c386c394efc7c47adb9925dcd2630b64f240c50f8d0eec482b9157"); !bytes.Equal(a.share, want) {
		t.Fatalf("Ya %x", a.share)
	}
	if want := unhex(t, "3ea7e0b19560d7c0b0f5734f63b955286dfa8232b5ebe63324e2d9e7433f7258"); !bytes.Equal(b.share, want) {
		t.Fatalf("Yb %x", b.share)
	}
	ka, err := a.secret(b.share)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := b.secret(a.share)
	if want := unhex(t, "80b69a8a76457ab6a4d7f887a4bf6b55a2f80ac19c333f917a05fc9887c8b40f"); !bytes.Equal(ka, want) || !bytes.Equal(kb, want) {
		t.Fatalf("K %x %x", ka, kb)
	}
	isk := cpaceISK(sid, ka, a.share, []byte("ADa"), b.share, []byte("ADb"))
	want := unhex(t, `b69effbf61b51d56401c0f65601abe428de8206feaaf0e32198896dc
		ae7b35cd2b38950a39dfd5d4a79164614c2984f7daa460b588c1e80c3fa2068af7900447`)
	if !bytes.Equal(isk, want) {
		t.Fatalf("ISK %x", isk)
	}
}

func TestCPaceRejectsBadShares(t *testing.T) {
	c, err := newCPace([]byte(testSecret), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	identity := ristretto255.NewIdentityElement().Bytes()
	for _, bad := range [][]byte{nil, make([]byte, 31), identity, bytes.Repeat([]byte{0xff}, 32)} {
		if _, err := c.keys(bad, true, nil, nil); err == nil {
			t.Errorf("share %x accepted", bad)
		}
	}
}

// Same key: both sides agree on tags and session key. Different keys: nothing matches.
func TestCPaceAgreement(t *testing.T) {
	sid := make([]byte, 32)
	ad1, ad2 := cpaceAD("a", "id-a"), cpaceAD("b", "id-b")
	run := func(k1, k2 string) (sessionKeys, sessionKeys) {
		d, _ := newCPace([]byte(k1), sid)
		a, _ := newCPace([]byte(k2), sid)
		dk, err := d.keys(a.share, true, ad1, ad2)
		if err != nil {
			t.Fatal(err)
		}
		ak, err := a.keys(d.share, false, ad1, ad2)
		if err != nil {
			t.Fatal(err)
		}
		return dk, ak
	}
	dk, ak := run(testSecret, testSecret)
	if !bytes.Equal(dk.dialerTag, ak.dialerTag) || !bytes.Equal(dk.acceptorTag, ak.acceptorTag) || !bytes.Equal(dk.session, ak.session) {
		t.Fatal("same key, different results")
	}
	if bytes.Equal(dk.dialerTag, dk.acceptorTag) || bytes.Equal(dk.session, dk.dialerTag) {
		t.Fatal("keys not separated")
	}
	dk, ak = run(testSecret, "another-secret-0123456789")
	if bytes.Equal(dk.dialerTag, ak.dialerTag) || bytes.Equal(dk.acceptorTag, ak.acceptorTag) || bytes.Equal(dk.session, ak.session) {
		t.Fatal("different keys agree")
	}
}
