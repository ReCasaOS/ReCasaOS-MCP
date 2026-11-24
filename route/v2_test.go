package route

import (
	"encoding/json"
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
	"github.com/ReCasaOS/ReCasaOS-MCP/service"
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

// deps is a router's parts with a token store in a directory of the test's own.
func deps(t *testing.T) Deps {
	t.Helper()
	tokens, err := service.OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}

	return Deps{Tokens: tokens, Catalogue: service.NewCatalogue(nil), Identity: service.NewIdentity(t.TempDir())}
}

func answer(deps Deps, method, path, authorization string) (int, string) {
	return send(deps, method, path, authorization, "")
}

func send(deps Deps, method, path, authorization, body string) (int, string) {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
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
	if code, _ := answer(deps(t), http.MethodGet, "/v2/mcp/status", ""); code != http.StatusUnauthorized {
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
		code, body := answer(deps(t), http.MethodGet, "/v2/mcp/status", token)
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
		if code, _ := answer(deps(t), method, "/v2/mcp/server", ""); code != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d, want 405", method, code)
		}
	}
}

func TestStatusCountsTheTokens(t *testing.T) {
	token := box(t, "admin", true)
	d := deps(t)
	if _, _, err := d.Tokens.Create("a", 1, service.LevelRead); err != nil {
		t.Fatal(err)
	}
	if _, body := answer(d, http.MethodGet, "/v2/mcp/status", token); !strings.Contains(body, `"tokens":1`) {
		t.Fatalf("status: %s, want 1 token", body)
	}
}

func decode(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("%v: %s", err, body)
	}

	return m
}

func TestCreateTokenDefaultsToRead(t *testing.T) {
	token := box(t, "admin", true)
	d := deps(t)
	code, body := send(d, http.MethodPost, "/v2/mcp/tokens", token, `{"name":"laptop"}`)
	if code != http.StatusCreated {
		t.Fatalf("%d: %s", code, body)
	}
	m := decode(t, body)
	if m["level"] != "read" || m["name"] != "laptop" || !strings.HasPrefix(m["token"].(string), "rcm_") {
		t.Fatalf("created: %s", body)
	}
	if got := d.Tokens.List(); len(got) != 1 || got[0].Level != service.LevelRead {
		t.Fatalf("stored: %+v", got)
	}
}

func TestCreateTokenUsesTheCallersAccount(t *testing.T) {
	token := box(t, "admin", true) // the stand-in's token is account 1
	d := deps(t)
	code, body := send(d, http.MethodPost, "/v2/mcp/tokens", token, `{"name":"mine","level":"full"}`)
	if code != http.StatusCreated {
		t.Fatalf("%d: %s", code, body)
	}
	if m := decode(t, body); m["account"] != float64(1) || m["level"] != "full" {
		t.Fatalf("created: %s", body)
	}

	code, _ = send(d, http.MethodPost, "/v2/mcp/tokens", token, `{"name":"theirs","account":7}`)
	if code != http.StatusBadRequest {
		t.Fatalf("a body with an account: %d, want 400", code)
	}
	if len(d.Tokens.List()) != 1 {
		t.Fatal("the refused body created a token")
	}
}

func TestCreateTokenRefusals(t *testing.T) {
	token := box(t, "admin", true)
	d := deps(t)
	for body, want := range map[string]int{
		`{"name":"x","level":"root"}`: http.StatusBadRequest,
		`{"name":"   "}`:              http.StatusBadRequest,
		`{"level":"read"}`:            http.StatusBadRequest,
		`not json`:                    http.StatusBadRequest,
	} {
		if code, _ := send(d, http.MethodPost, "/v2/mcp/tokens", token, body); code != want {
			t.Errorf("%s: %d, want %d", body, code, want)
		}
	}
	for i := 0; i < 20; i++ {
		if code, body := send(d, http.MethodPost, "/v2/mcp/tokens", token, `{"name":"t"}`); code != http.StatusCreated {
			t.Fatalf("token %d: %d %s", i+1, code, body)
		}
	}
	if code, _ := send(d, http.MethodPost, "/v2/mcp/tokens", token, `{"name":"one more"}`); code != http.StatusConflict {
		t.Errorf("21st token: %d, want 409", code)
	}
}

func TestTokensNeverListThePlainValue(t *testing.T) {
	token := box(t, "admin", true)
	d := deps(t)
	_, body := send(d, http.MethodPost, "/v2/mcp/tokens", token, `{"name":"laptop"}`)
	created := decode(t, body)

	code, list := answer(d, http.MethodGet, "/v2/mcp/tokens", token)
	if code != http.StatusOK || strings.Contains(list, created["token"].(string)) || strings.Contains(list, `"token"`) || strings.Contains(list, "hash") {
		t.Fatalf("%d: %s", code, list)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(list), &got); err != nil || len(got) != 1 || got[0]["id"] != created["id"] {
		t.Fatalf("list: %v %s", err, list)
	}
}

func TestTokensAreTheOwnersAlone(t *testing.T) {
	token := box(t, "user", true)
	d := deps(t)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/v2/mcp/tokens", ""},
		{http.MethodPost, "/v2/mcp/tokens", `{"name":"x"}`},
		{http.MethodDelete, "/v2/mcp/tokens/abcd1234", ""},
	} {
		if code, _ := send(d, c.method, c.path, token, c.body); code != http.StatusForbidden {
			t.Errorf("%s %s: %d, want 403", c.method, c.path, code)
		}
	}
	if len(d.Tokens.List()) != 0 {
		t.Fatal("a refused call created a token")
	}
}

func TestDeleteToken(t *testing.T) {
	token := box(t, "admin", true)
	d := deps(t)
	_, body := send(d, http.MethodPost, "/v2/mcp/tokens", token, `{"name":"laptop"}`)
	id := decode(t, body)["id"].(string)

	if code, _ := answer(d, http.MethodDelete, "/v2/mcp/tokens/"+id, token); code != http.StatusNoContent {
		t.Fatalf("delete: %d, want 204", code)
	}
	if code, _ := answer(d, http.MethodDelete, "/v2/mcp/tokens/"+id, token); code != http.StatusNotFound {
		t.Fatalf("delete again: %d, want 404", code)
	}
	if len(d.Tokens.List()) != 0 {
		t.Fatal("the token is still stored")
	}
}

func TestDeleteThatCannotBeSavedIs500(t *testing.T) {
	token := box(t, "admin", true)
	dir := t.TempDir()
	tokens, err := service.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Tokens: tokens, Catalogue: service.NewCatalogue(nil), Identity: service.NewIdentity(t.TempDir())}
	_, body := send(d, http.MethodPost, "/v2/mcp/tokens", token, `{"name":"laptop"}`)
	id := decode(t, body)["id"].(string)

	// the state directory goes away: nothing can be written any more
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if code, body := answer(d, http.MethodDelete, "/v2/mcp/tokens/"+id, token); code != http.StatusInternalServerError || !strings.Contains(body, "still works") {
		t.Fatalf("%d: %s, want 500", code, body)
	}
	if code, _ := answer(d, http.MethodDelete, "/v2/mcp/tokens/00000000", token); code != http.StatusNotFound {
		t.Fatalf("unknown id: %d, want 404", code)
	}
}
