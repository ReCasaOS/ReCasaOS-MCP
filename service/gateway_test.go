package service

import (
	"os"
	"path/filepath"
	"testing"
)

func writeINI(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.ini")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGatewayAddressFromINI(t *testing.T) {
	for ini, want := range map[string]string{
		"[gateway]\nport=80\naddress=\n":            "http://127.0.0.1:80",
		"[gateway]\nport=8080\naddress=0.0.0.0\n":   "http://127.0.0.1:8080",
		"[gateway]\nport=8080\naddress=127.0.0.1\n": "http://127.0.0.1:8080",
		"[gateway]\nport=8080\naddress=::1\n":       "http://[::1]:8080",
		"[Gateway]\nPort=81\nhttpsport=8443\n":      "http://127.0.0.1:81",
	} {
		got, err := gatewayAddress(writeINI(t, ini))
		if err != nil || got.String() != want {
			t.Errorf("%q: %v %v, want %s", ini, got, err, want)
		}
	}
}

func TestGatewayAddressFailures(t *testing.T) {
	for name, ini := range map[string]string{
		"no port":      "[gateway]\nport=\n",
		"bad port":     "[gateway]\nport=70000\n",
		"off loopback": "[gateway]\nport=80\naddress=192.168.1.5\n",
		"https only":   "[gateway]\nport=8443\ntlscert=/c\ntlskey=/k\n",
	} {
		if got, err := gatewayAddress(writeINI(t, ini)); err == nil {
			t.Errorf("%s: accepted as %v", name, got)
		}
	}
	if _, err := gatewayAddress(filepath.Join(t.TempDir(), "missing.ini")); err == nil {
		t.Error("missing file accepted")
	}
}
