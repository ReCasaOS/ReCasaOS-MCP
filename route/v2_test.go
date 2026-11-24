package route

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-Common/utils/jwt"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/ReCasaOS-MCP/pkg/config"
)

// The common library keeps user-service's public key for ten seconds whatever the runtime
// path, so every test of this binary signs with the same key, served by the same stand-in.
var signer = sync.OnceValues(func() (address string, sign func() (string, error)) {
	privateKey, publicKey, err := jwt.GenerateKeyPair()
	if err != nil {
		panic(err)
	}
	jwks, err := jwt.GenerateJwksJSON(publicKey)
	if err != nil {
		panic(err)
	}

	return httptest.NewServer(jwt.JWKSHandler(jwks)).URL, func() (string, error) { return jwt.GetAccessToken("someone", privateKey, 1) }
})

// box is a runtime path with an internal secret and a user-service that signs tokens; it
// returns a valid token of account 1. The role of that account is what the test says.
func box(t *testing.T, role string, known bool) (token string) {
	t.Helper()
	logger.LogInitConsoleOnly()
	runtime := t.TempDir()
	previous := config.CommonInfo.RuntimePath
	config.CommonInfo.RuntimePath = runtime
	previousAccount := account
	account = func(string, string) (string, bool, bool) { return role, false, known }
	t.Cleanup(func() { config.CommonInfo.RuntimePath = previous; account = previousAccount })

	if err := os.WriteFile(filepath.Join(runtime, external.InternalSecretFilename), []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	address, sign := signer()
	if err := os.WriteFile(filepath.Join(runtime, external.UserServiceAddressFilename), []byte(address), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := sign()
	if err != nil {
		t.Fatal(err)
	}

	return token
}

func answer(deps Deps, method, path, authorization string) (int, string) {
	request := httptest.NewRequest(method, path, nil)
	request.RemoteAddr = "203.0.113.5:40000"
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	InitV2Router(deps).ServeHTTP(recorder, request)

	return recorder.Code, strings.TrimSpace(recorder.Body.String())
}

func TestStatusNeedsAToken(t *testing.T) {
	box(t, "admin", true)
	if code, _ := answer(Deps{}, http.MethodGet, "/v2/mcp/status", ""); code != http.StatusUnauthorized {
		t.Fatalf("%d, want 401", code)
	}
}

func TestStatusIsTheOwnersAlone(t *testing.T) {
	for _, c := range []struct {
		role  string
		known bool
		code  int
	}{
		{"user", true, http.StatusForbidden},
		{"user", false, http.StatusServiceUnavailable},
		{"admin", true, http.StatusOK},
	} {
		token := box(t, c.role, c.known)
		code, body := answer(Deps{}, http.MethodGet, "/v2/mcp/status", token)
		if code != c.code {
			t.Errorf("role %q known %v: %d, want %d", c.role, c.known, code, c.code)
		}
		if c.code == http.StatusOK && !strings.Contains(body, `"endpoint":"/v2/mcp/server"`) {
			t.Errorf("owner's status: %s, want the endpoint /v2/mcp/server", body)
		}
	}
}

// /server is mounted apart from the owner's checks: it answers to its own tokens, so it
// refuses these methods whatever the Authorization header holds.
func TestServerRefusesGetAndDelete(t *testing.T) {
	box(t, "admin", true)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if code, _ := answer(Deps{}, method, "/v2/mcp/server", ""); code != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d, want 405", method, code)
		}
	}
}
