package config

import "testing"

func TestIsZeroTier(t *testing.T) {
	for name, want := range map[string]bool{"ZeroTier One [8056c2e21c000001]": true, "zerotier0": true, "Ethernet": false, "": false} {
		if IsZeroTier(name) != want {
			t.Errorf("IsZeroTier(%q) = %v", name, !want)
		}
	}
}
