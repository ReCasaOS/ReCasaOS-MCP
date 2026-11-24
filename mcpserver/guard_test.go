package mcpserver

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ReCasaOS/ReCasaOS-MCP/service"
)

func sha256Hex(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

const listBody = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"t","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}}}`

// post sends what Claude Code 2.1.330 sends on a tools/list, with the given headers on top;
// an empty value removes a header.
func post(t *testing.T, endpoint string, h http.Header, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/list")
	for k, v := range h {
		if k == "Host" {
			req.Host = v[0]
			continue
		}
		req.Header.Del(k)
		if v[0] != "" {
			req.Header[k] = v
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func with(h http.Header, k, v string) http.Header {
	h = h.Clone()
	h[http.CanonicalHeaderKey(k)] = []string{v}
	return h
}

func TestNotFromLoopbackIsRefused(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	for _, xff := range []string{"", "127.0.0.1"} {
		req := httptest.NewRequest(http.MethodPost, "/v2/mcp/server", strings.NewReader(listBody))
		req.RemoteAddr = "192.168.1.9:40000"
		req.Header.Set("Authorization", "Bearer "+plain)
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		rec := httptest.NewRecorder()
		f.guard.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("X-Forwarded-For %q: %d, want 403", xff, rec.Code)
		}
	}
}

func TestForeignOriginIsRefused(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	for _, origin := range []string{"http://evil.example", "http://127.0.0.1:1", "http://evil.example/" + strings.TrimPrefix(f.srv.URL, "http://")} {
		if code := post(t, f.srv.URL, with(bearer(plain), "Origin", origin), listBody).StatusCode; code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", origin, code)
		}
	}
}

func TestOriginNullIsRefused(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	for _, origin := range []string{"null", "::bad", "box"} {
		if code := post(t, f.srv.URL, with(bearer(plain), "Origin", origin), listBody).StatusCode; code != http.StatusForbidden {
			t.Errorf("%q: %d, want 403", origin, code)
		}
	}
}

func TestSameOriginPasses(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelRead, 7)
	h := with(bearer(plain), "Origin", strings.ToUpper(f.srv.URL))
	cs, err := connectTo(t, f.srv.URL, headers{h: h})
	if err != nil {
		t.Fatal(err)
	}
	if names, _ := toolNames(t, cs); names != "describe_action,read,search_actions" {
		t.Fatal(names)
	}
}

// Claude Code 2.1.330 sends no Origin.
func TestNoOriginPasses(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelRead, 7)
	if names, _ := toolNames(t, f.connect(plain)); names != "describe_action,read,search_actions" {
		t.Fatal(names)
	}
}

func TestBadTokenIs401(t *testing.T) {
	f := newFixture(t)
	resp := post(t, f.srv.URL, bearer("rcm_notatoken"), listBody)
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("%d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	if n := f.guard.limit.failures("127.0.0.1"); n != 1 {
		t.Fatalf("%d failures counted, want 1", n)
	}
	if _, err := connectTo(t, f.srv.URL, headers{h: bearer("rcm_notatoken")}); err == nil {
		t.Fatal("the SDK client connected with a bad token")
	}
}

func TestMalformedAuthorizationIs401(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	for _, auth := range []string{"", plain, "Basic " + plain, "Bearer " + plain + " " + plain, "Bearer  " + plain, "Bearer"} {
		resp := post(t, f.srv.URL, http.Header{"Authorization": {auth}}, listBody)
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%q: %d", auth, resp.StatusCode)
		}
	}
	if code := post(t, f.srv.URL, http.Header{"Authorization": {"bEaReR " + plain}}, listBody).StatusCode; code != http.StatusOK {
		t.Fatalf("the scheme in another case: %d, want 200", code)
	}
}

func TestElevenBadTokensIs429(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 10; i++ {
		if code := post(t, f.srv.URL, bearer("rcm_bad"), listBody).StatusCode; code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i+1, code)
		}
	}
	resp := post(t, f.srv.URL, bearer("rcm_bad"), listBody)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "60" {
		t.Fatalf("%d Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

// Bad tokens from an address never lock out a valid token from the same address.
func TestAValidTokenPassesFromABlockedAddress(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelRead, 7)
	for i := 0; i < 11; i++ {
		resp := post(t, f.srv.URL, bearer("rcm_bad"), listBody)
		if want := map[bool]int{true: http.StatusTooManyRequests, false: http.StatusUnauthorized}[i == 10]; resp.StatusCode != want {
			t.Fatalf("bad token %d: %d, want %d", i+1, resp.StatusCode, want)
		}
	}
	if code := post(t, f.srv.URL, bearer(plain), listBody).StatusCode; code != http.StatusOK {
		t.Fatalf("the valid token: %d, want 200", code)
	}
	if names, _ := toolNames(t, f.connect(plain)); names != "describe_action,read,search_actions" {
		t.Fatal(names)
	}
	if resp := post(t, f.srv.URL, bearer("rcm_bad"), listBody); resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("a bad token after the valid one: %d", resp.StatusCode)
	}
}

func TestLimitUsesTheLastForwardedAddress(t *testing.T) {
	f := newFixture(t)
	h := with(bearer("rcm_bad"), "X-Forwarded-For", "1.2.3.4, 192.168.1.9")
	for i := 0; i < 10; i++ {
		post(t, f.srv.URL, h, listBody)
	}
	if f.guard.limit.failures("192.168.1.9") != 10 || f.guard.limit.failures("1.2.3.4") != 0 || f.guard.limit.failures("127.0.0.1") != 0 {
		t.Fatal("the failures were not counted for the last address")
	}
	if code := post(t, f.srv.URL, with(bearer("rcm_bad"), "X-Forwarded-For", "5.6.7.8,192.168.1.9"), listBody).StatusCode; code != http.StatusTooManyRequests {
		t.Fatalf("a new first element opened a new count: %d", code)
	}
	if code := post(t, f.srv.URL, with(bearer("rcm_bad"), "X-Forwarded-For", "192.168.1.9, not-an-ip"), listBody).StatusCode; code != http.StatusUnauthorized {
		t.Fatalf("an invalid last element: %d, want 401 counted for the remote address", code)
	}
	if f.guard.limit.failures("127.0.0.1") != 1 {
		t.Fatal("an invalid last element is not counted for the remote address")
	}
}

// Claude Code 2.1.330 posts notifications/cancelled without Mcp-Method; the SDK refuses it.
// That is not an authentication failure.
func TestSDKBadRequestIsNotAnAuthFailure(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	body := `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
	resp := post(t, f.srv.URL, with(bearer(plain), "Mcp-Method", ""), body)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests {
		t.Fatalf("%d", resp.StatusCode)
	}
	t.Logf("the SDK answered %d", resp.StatusCode)
	if n := f.guard.limit.failures("127.0.0.1"); n != 0 {
		t.Fatalf("%d failures counted, want 0", n)
	}
}

func TestRequestBodyOverOneMiBIsRefused(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	big := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"pad":"` + strings.Repeat("a", 1<<20) + `"}}`
	if code := post(t, f.srv.URL, bearer(plain), big).StatusCode; code != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d, want 413", code)
	}
}

func TestReachedThroughTheGateway(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelRead, 7)
	target, _ := url.Parse(f.srv.URL)
	proxy := httptest.NewServer(httputil.NewSingleHostReverseProxy(target))
	t.Cleanup(proxy.Close)

	cs, err := connectTo(t, proxy.URL, headers{h: with(bearer(plain), "X-Forwarded-For", "1.2.3.4"), host: "box.local"})
	if err != nil {
		t.Fatal(err)
	}
	if names, _ := toolNames(t, cs); names != "describe_action,read,search_actions" {
		t.Fatal(names)
	}
	if text, isErr := call(t, cs, "read", map[string]any{"action": "GET /v2/app_management/apps"}); isErr {
		t.Fatal(text)
	}

	h := with(with(bearer("rcm_bad"), "X-Forwarded-For", "1.2.3.4"), "Host", "box.local")
	if code := post(t, proxy.URL, h, listBody).StatusCode; code != http.StatusUnauthorized {
		t.Fatalf("%d, want 401", code)
	}
	// the proxy appended the address it saw: that one is counted, not what the client claimed
	if f.guard.limit.failures("127.0.0.1") != 1 || f.guard.limit.failures("1.2.3.4") != 0 {
		t.Fatal("the failure was not counted under the proxy-appended address")
	}
}

type loggedLine struct {
	msg, addr string
	count     int
}

// noted collects what the guard writes to the general log.
type noted struct {
	mu    sync.Mutex
	lines []loggedLine
}

func (n *noted) note(msg, addr string, count int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lines = append(n.lines, loggedLine{msg, addr, count})
}

func (n *noted) got() []loggedLine {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]loggedLine(nil), n.lines...)
}

func fixtureWithLog(t *testing.T) (*fixture, *noted) {
	f := newFixture(t)
	n := &noted{}
	f.guard.note = n.note
	return f, n
}

func TestFirstBadTokenIsLoggedOnce(t *testing.T) {
	f, n := fixtureWithLog(t)
	for i := 0; i < 10; i++ {
		post(t, f.srv.URL, bearer("rcm_bad"), listBody)
	}
	got := n.got()
	if len(got) != 1 || got[0].addr != "127.0.0.1" || got[0].count != 1 {
		t.Fatalf("%+v, want one line for the first failure", got)
	}
}

func TestReachingTheLimitIsLoggedOnce(t *testing.T) {
	f, n := fixtureWithLog(t)
	for i := 0; i < 25; i++ {
		post(t, f.srv.URL, bearer("rcm_bad"), listBody)
	}
	got := n.got()
	if len(got) != 2 || got[0].count != 1 || got[1].count != 11 || got[1].addr != "127.0.0.1" {
		t.Fatalf("%+v, want the first failure then the 11th", got)
	}
}

func TestANewWindowLogsAgain(t *testing.T) {
	f, n := fixtureWithLog(t)
	for i := 0; i < 12; i++ {
		post(t, f.srv.URL, bearer("rcm_bad"), listBody)
	}
	f.clock.add(failWindow + time.Second)
	for i := 0; i < 12; i++ {
		post(t, f.srv.URL, bearer("rcm_bad"), listBody)
	}
	if got := n.got(); len(got) != 4 {
		t.Fatalf("%+v, want 4 lines over two windows", got)
	}
}

func TestForeignOriginIsLogged(t *testing.T) {
	f, n := fixtureWithLog(t)
	plain, _ := f.token(service.LevelFull, 7)
	for i := 0; i < 3; i++ {
		post(t, f.srv.URL, with(bearer(plain), "Origin", "http://evil.example/secret?x=1"), listBody)
	}
	got := n.got()
	if len(got) != 1 || got[0].addr != "127.0.0.1" {
		t.Fatalf("%+v, want one line for the address", got)
	}
	// not from loopback: one line as well, for its own address
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v2/mcp/server", strings.NewReader(listBody))
		req.RemoteAddr = "192.168.1.9:40000"
		f.guard.ServeHTTP(httptest.NewRecorder(), req)
	}
	got = n.got()
	if len(got) != 2 || got[1].addr != "192.168.1.9" {
		t.Fatalf("%+v, want a second line for 192.168.1.9", got)
	}
}

func TestLoggedLinesCarryNoToken(t *testing.T) {
	f, n := fixtureWithLog(t)
	h := with(bearer("rcm_presentedsecret"), "Origin", "")
	for i := 0; i < 12; i++ {
		post(t, f.srv.URL, h, listBody)
	}
	post(t, f.srv.URL, with(h, "Origin", "http://evil.example/rcm_presentedsecret"), listBody)
	for _, l := range n.got() {
		if all := l.msg + l.addr; strings.Contains(all, "rcm_") || strings.Contains(all, "Bearer") || strings.Contains(all, "evil") {
			t.Fatalf("a logged line carries request data: %+v", l)
		}
	}
}

func TestAValidTokenLogsNothing(t *testing.T) {
	f, n := fixtureWithLog(t)
	plain, _ := f.token(service.LevelFull, 7)
	for i := 0; i < 3; i++ {
		post(t, f.srv.URL, bearer(plain), listBody)
	}
	if got := n.got(); len(got) != 0 {
		t.Fatalf("%+v, want nothing", got)
	}
}
