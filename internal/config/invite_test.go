package config

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type inviteVector struct {
	PIDHex    string `json:"pid_hex"`
	SecretHex string `json:"secret_hex"`
	PID       string `json:"pid"`
	Secret    string `json:"secret"`
	Invite    string `json:"invite"`
	KeyHex    string `json:"key_hex"`
	TagHex    string `json:"tag_hex"`
}

// The vectors were computed independently of this package (.NET SHA256/HKDF
// and a hand-written base32), so they pin the format, not the code.
func loadVectors(t *testing.T) []inviteVector {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "invite_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vs []inviteVector
	if err := json.Unmarshal(data, &vs); err != nil || len(vs) == 0 {
		t.Fatalf("vectors: %v", err)
	}
	return vs
}

func TestInviteVectors(t *testing.T) {
	for _, v := range loadVectors(t) {
		pidBytes, _ := hex.DecodeString(v.PIDHex)
		secBytes, _ := hex.DecodeString(v.SecretHex)
		if got := b32.EncodeToString(pidBytes); got != v.PID {
			t.Errorf("pid %s, want %s", got, v.PID)
		}
		if got := b32.EncodeToString(secBytes); got != v.Secret {
			t.Errorf("secret %s, want %s", got, v.Secret)
		}
		if got := FormatInvite(v.PID, ProjectEpoch, v.Secret); got != v.Invite {
			t.Errorf("invite %s, want %s", got, v.Invite)
		}
		inv, err := ParseInvite(v.Invite)
		if err != nil || inv != (Invite{ProjectID: v.PID, Epoch: 1, Secret: v.Secret}) {
			t.Errorf("parse %s = %+v, %v", v.Invite, inv, err)
		}
		key, err := ProjectKey(v.PID, ProjectEpoch, v.Secret)
		if err != nil || hex.EncodeToString(key) != v.KeyHex {
			t.Errorf("key %x, %v; want %s", key, err, v.KeyHex)
		}
		if got := ProjectTag(key); got != v.TagHex {
			t.Errorf("tag %s, want %s", got, v.TagHex)
		}
	}
}

func TestParseInviteCanonicalizes(t *testing.T) {
	v := loadVectors(t)[0]
	for _, in := range []string{
		strings.ToLower(v.Invite),
		"  " + v.Invite + "\n",
		v.Invite[:10] + " \t" + v.Invite[10:30] + "\r\n" + v.Invite[30:],
	} {
		if inv, err := ParseInvite(in); err != nil || FormatInvite(inv.ProjectID, inv.Epoch, inv.Secret) != v.Invite {
			t.Errorf("ParseInvite(%q) = %+v, %v", in, inv, err)
		}
	}
}

func TestParseInviteRejects(t *testing.T) {
	v := loadVectors(t)[0]
	parts := strings.Split(v.Invite, ".")
	join := func(p ...string) string { return strings.Join(p, ".") }
	typo := []byte(v.Invite)
	typo[12] = 'Z' // one character of the pid
	bad := map[string]string{
		"empty":            "",
		"prefix":           join("ALP2", parts[1], parts[2], parts[3], parts[4]),
		"missing part":     join(parts[0], parts[1], parts[2], parts[3]),
		"extra part":       v.Invite + ".AAAA",
		"epoch 2":          join(parts[0], parts[1], "2", parts[3], inviteCheck(parts[1], "2", parts[3])),
		"epoch 01":         join(parts[0], parts[1], "01", parts[3], inviteCheck(parts[1], "01", parts[3])),
		"short pid":        join(parts[0], parts[1][:25], parts[2], parts[3], inviteCheck(parts[1][:25], "1", parts[3])),
		"long secret":      join(parts[0], parts[1], parts[2], parts[3]+"A", inviteCheck(parts[1], "1", parts[3]+"A")),
		"padding":          join(parts[0], parts[1]+"======", parts[2], parts[3], parts[4]),
		"non-alphabet":     join(parts[0], "0"+parts[1][1:], parts[2], parts[3], inviteCheck("0"+parts[1][1:], "1", parts[3])),
		"dash":             join(parts[0], parts[1][:13]+"-"+parts[1][14:], parts[2], parts[3], parts[4]),
		"trailing bits":    join(parts[0], parts[1][:25]+"5", parts[2], parts[3], inviteCheck(parts[1][:25]+"5", "1", parts[3])),
		"checksum typo":    string(typo),
		"wrong checksum":   join(parts[0], parts[1], parts[2], parts[3], "AAAA"),
		"legacy code":      "K7Q2-MXPA-4RTB",
		"non-ascii letter": join(parts[0], "Ä"+parts[1][1:], parts[2], parts[3], parts[4]),
	}
	for name, in := range bad {
		if inv, err := ParseInvite(in); !errors.Is(err, ErrInvite) {
			t.Errorf("%s: ParseInvite(%q) = %+v, %v; want ErrInvite", name, in, inv, err)
		}
	}
}

func TestConfigProject(t *testing.T) {
	c := Config{Node: "a", Listen: "127.0.0.1:0", API: "127.0.0.1:0", DataDir: "d", SecretEnv: "S", Project: "legacy"}
	if err := c.Validate(); err == nil {
		t.Fatal("invalid project id accepted")
	}
	c.Project = loadVectors(t)[0].PID
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNewProjectIDs(t *testing.T) {
	a, err := NewProjectID()
	if err != nil || !ValidProjectID(a) {
		t.Fatalf("NewProjectID = %q, %v", a, err)
	}
	s, err := NewProjectSecret()
	if err != nil || !ValidProjectSecret(s) || s == a {
		t.Fatalf("NewProjectSecret = %q, %v", s, err)
	}
	inv, err := ParseInvite(FormatInvite(a, ProjectEpoch, s))
	if err != nil || inv.ProjectID != a || inv.Secret != s {
		t.Fatalf("round trip %+v, %v", inv, err)
	}
	for _, bad := range []string{"", strings.ToLower(a), a[:25], a + "A", "legacy"} {
		if ValidProjectID(bad) {
			t.Errorf("ValidProjectID(%q)", bad)
		}
	}
	if _, err := ProjectKey("bad", ProjectEpoch, s); err == nil {
		t.Error("ProjectKey accepted a bad id")
	}
	if _, err := ProjectKey(a, ProjectEpoch, "bad"); err == nil {
		t.Error("ProjectKey accepted a bad secret")
	}
}
