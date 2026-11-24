package service

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"

	"gopkg.in/ini.v1"
)

// DefaultGatewayINI is where a box's Gateway keeps its configuration.
const DefaultGatewayINI = "/etc/casaos/gateway.ini"

// gatewayAddress is the loopback URL of the Gateway's public listener, read from
// its ini file the way the box's core does. The port can change when the owner
// changes it in the dashboard, so callers read it again after a failed connection.
func gatewayAddress(path string) (*url.URL, error) {
	u, _, err := gatewayListener(path)
	return u, err
}

// gatewayListener is gatewayAddress that also says whether the Gateway listens on all
// addresses, not on the box's loopback alone.
func gatewayListener(path string) (*url.URL, bool, error) {
	f, err := ini.LoadSources(ini.LoadOptions{Insensitive: true}, path)
	if err != nil {
		return nil, false, err
	}
	gw := f.Section("gateway")
	port, err := strconv.Atoi(gw.Key("port").String())
	if err != nil || port < 1 || port > 65535 {
		return nil, false, errors.New("the gateway has no port yet")
	}
	host, all := "127.0.0.1", true
	if address := gw.Key("address").String(); address != "" {
		ip, err := netip.ParseAddr(address)
		if err != nil || !(ip.IsUnspecified() || ip.IsLoopback()) {
			return nil, false, fmt.Errorf("the gateway listens on %s, not on this box's loopback", address)
		}
		all = ip.IsUnspecified()
		if ip == netip.MustParseAddr("::1") {
			host = "[::1]"
		}
	}
	if gw.Key("tlscert").String() != "" && gw.Key("tlskey").String() != "" {
		// Only HTTPS is served; talking to it needs a certificate policy this module does not have yet.
		return nil, false, errors.New("the gateway serves HTTPS only, which is not supported yet")
	}
	return &url.URL{Scheme: "http", Host: host + ":" + strconv.Itoa(port)}, all, nil
}
