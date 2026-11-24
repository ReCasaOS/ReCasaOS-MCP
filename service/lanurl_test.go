package service

import (
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
)

// withOutboundIP makes the lookup of the box's address answer ip ("" for a failure).
func withOutboundIP(t *testing.T, ip string) {
	t.Helper()
	previous := outboundIPv4
	outboundIPv4 = func() (netip.Addr, error) {
		if ip == "" {
			return netip.Addr{}, errors.New("no route")
		}
		return netip.MustParseAddr(ip), nil
	}
	t.Cleanup(func() { outboundIPv4 = previous })
}

func TestLANURLWithTheDefaultPort(t *testing.T) {
	withOutboundIP(t, "192.168.1.20")
	got, ok := LANURL(writeINI(t, "[gateway]\nport=80\n"))
	if !ok || got != "http://192.168.1.20/v2/mcp/server" {
		t.Errorf("%q %v", got, ok)
	}
}

func TestLANURLWithAnotherPort(t *testing.T) {
	withOutboundIP(t, "192.168.1.20")
	got, ok := LANURL(writeINI(t, "[gateway]\nport=8080\n"))
	if !ok || got != "http://192.168.1.20:8080/v2/mcp/server" {
		t.Errorf("%q %v", got, ok)
	}
}

func TestLANURLIsAbsentForAPublicAddress(t *testing.T) {
	withOutboundIP(t, "203.0.113.7")
	if got, ok := LANURL(writeINI(t, "[gateway]\nport=80\n")); ok {
		t.Errorf("published %q", got)
	}
}

func TestLANURLIsAbsentWhenTheAddressIsUnknown(t *testing.T) {
	withOutboundIP(t, "")
	if got, ok := LANURL(writeINI(t, "[gateway]\nport=80\n")); ok {
		t.Errorf("published %q", got)
	}
}

func TestLANURLIsAbsentWithoutTheGatewayPort(t *testing.T) {
	withOutboundIP(t, "192.168.1.20")
	if got, ok := LANURL(filepath.Join(t.TempDir(), "missing.ini")); ok {
		t.Errorf("published %q", got)
	}
}

func TestLANURLIsAbsentForAnHTTPSOnlyGateway(t *testing.T) {
	withOutboundIP(t, "192.168.1.20")
	if got, ok := LANURL(writeINI(t, "[gateway]\nport=8443\ntlscert=/c\ntlskey=/k\n")); ok {
		t.Errorf("published %q", got)
	}
}

func TestLANURLAcceptsEachPrivateRange(t *testing.T) {
	for ip, want := range map[string]bool{
		"10.1.2.3": true, "172.16.5.4": true, "192.168.0.9": true, "100.64.0.5": true,
		"172.32.0.1": false, "169.254.1.1": false,
		"172.15.255.255": false, "100.63.255.255": false, "100.128.0.0": false,
	} {
		withOutboundIP(t, ip)
		if _, ok := LANURL(writeINI(t, "[gateway]\nport=80\n")); ok != want {
			t.Errorf("%s: %v, want %v", ip, ok, want)
		}
	}
}

func TestLANURLIsAbsentForALoopbackOnlyGateway(t *testing.T) {
	withOutboundIP(t, "192.168.1.20")
	for _, address := range []string{"127.0.0.1", "::1"} {
		if got, ok := LANURL(writeINI(t, "[gateway]"+"\nport=80"+"\naddress="+address+"\n")); ok {
			t.Errorf("%s: published %q", address, got)
		}
	}
}

func TestLANURLIsGivenForAGatewayOnAllAddresses(t *testing.T) {
	withOutboundIP(t, "192.168.1.20")
	for _, address := range []string{"", "0.0.0.0", "::"} {
		if _, ok := LANURL(writeINI(t, "[gateway]"+"\nport=80"+"\naddress="+address+"\n")); !ok {
			t.Errorf("%q: absent", address)
		}
	}
}
