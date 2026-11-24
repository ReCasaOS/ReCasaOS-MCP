package service

import (
	"net"
	"net/netip"
)

// Endpoint is the path an assistant connects to.
const Endpoint = "/v2/mcp/server"

// outboundIPv4 is a variable so a test can say what the box's address is.
var outboundIPv4 = defaultRouteIPv4

// defaultRouteIPv4 is the box's address on the interface of its default route. Connecting a
// UDP socket sends nothing: it only makes the system choose the source address.
func defaultRouteIPv4() (netip.Addr, error) {
	c, err := net.Dial("udp4", "192.0.2.1:9") // TEST-NET-1, never routed
	if err != nil {
		return netip.Addr{}, err
	}
	defer c.Close()
	ip, _ := netip.AddrFromSlice(c.LocalAddr().(*net.UDPAddr).IP)
	return ip.Unmap(), nil
}

var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
}

func isPrivate(ip netip.Addr) bool {
	for _, p := range privateRanges {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func lanURL(ip netip.Addr, port string) string {
	host := ip.String()
	if port != "80" {
		host += ":" + port
	}
	return "http://" + host + Endpoint
}

// LANURL is the address of the MCP endpoint for a computer of the box's network. It is
// false when the box's private address or the Gateway's plain HTTP port is not known.
func LANURL(gatewayINI string) (string, bool) {
	ip, err := outboundIPv4()
	if err != nil || !ip.Is4() || !isPrivate(ip) {
		return "", false
	}
	gw, all, err := gatewayListener(gatewayINI)
	if err != nil || !all { // a Gateway bound to the box's loopback cannot be reached from the network
		return "", false
	}
	return lanURL(ip, gw.Port()), true
}
