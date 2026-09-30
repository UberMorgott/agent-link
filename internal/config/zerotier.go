package config

import "strings"

// IsZeroTier reports whether the network adapter named name is a ZeroTier
// one, e.g. "ZeroTier One [8056c2e21c000001]" on Windows.
func IsZeroTier(name string) bool { return strings.Contains(strings.ToLower(name), "zerotier") }
