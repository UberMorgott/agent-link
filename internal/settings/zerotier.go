package settings

import (
	"net"
	"strconv"
	"strings"

	"github.com/UberMorgott/agent-link/internal/config"
)

// Iface is the part of a network interface that listen detection needs.
type Iface struct {
	Name  string // on Windows the adapter's friendly name, e.g. "ZeroTier One [8056c2e21c000001]"
	Up    bool
	Addrs []net.IP
}

// SystemIfaces lists this machine's interfaces.
func SystemIfaces() []Iface {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]Iface, 0, len(ifs))
	for _, i := range ifs {
		it := Iface{Name: i.Name, Up: i.Flags&net.FlagUp != 0}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				it.Addrs = append(it.Addrs, ipn.IP)
			}
		}
		out = append(out, it)
	}
	return out
}

// ZeroTierIP returns the address of the first running ZeroTier adapter,
// preferring IPv4 over global IPv6; link-local addresses never count.
func ZeroTierIP(ifaces []Iface) (net.IP, bool) {
	var v6 net.IP
	for _, i := range ifaces {
		if !i.Up || !strings.Contains(strings.ToLower(i.Name), "zerotier") {
			continue
		}
		for _, ip := range i.Addrs {
			switch {
			case ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified():
			case ip.To4() != nil:
				return ip, true
			case v6 == nil:
				v6 = ip
			}
		}
	}
	return v6, v6 != nil
}

// LANIP returns the first IPv4 address of a running interface that is
// neither loopback nor link-local.
func LANIP(ifaces []Iface) (net.IP, bool) {
	for _, i := range ifaces {
		if !i.Up {
			continue
		}
		for _, ip := range i.Addrs {
			if ip.To4() != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified() {
				return ip, true
			}
		}
	}
	return nil, false
}

// BindAddr is the peer listener address: the configured Listen (default port
// added), else every interface on the default port, so members reach this
// node over ZeroTier, the LAN or an external address alike. The control API
// is separate and stays on loopback.
func (s Settings) BindAddr() string {
	if s.Listen != "" {
		if a, err := config.WithDefaultPort(s.Listen); err == nil {
			return a
		}
		return s.Listen
	}
	return ":" + strconv.Itoa(config.DefaultPort)
}

// AdvertiseAddr is the address to give the others: the configured Listen,
// else the ZeroTier IP, else a LAN IP, else loopback, with the default port.
// zeroTier is false only when detection found no ZeroTier adapter.
func (s Settings) AdvertiseAddr(ifaces []Iface) (addr string, zeroTier bool) {
	if s.Listen != "" {
		return s.BindAddr(), true
	}
	port := strconv.Itoa(config.DefaultPort)
	if ip, ok := ZeroTierIP(ifaces); ok {
		return net.JoinHostPort(ip.String(), port), true
	}
	if ip, ok := LANIP(ifaces); ok {
		return net.JoinHostPort(ip.String(), port), false
	}
	return net.JoinHostPort("127.0.0.1", port), false
}
