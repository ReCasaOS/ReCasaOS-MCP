package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/ReCasaOS-MCP/catalogue"
	v2Route "github.com/ReCasaOS/ReCasaOS-MCP/route/v2"
	"github.com/ReCasaOS/ReCasaOS-MCP/service"
	"github.com/labstack/echo/v4"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMain(m *testing.M) {
	logger.LogInitConsoleOnly()
	os.Exit(m.Run())
}

const (
	testSecret = "s3cret-of-this-boot"
	source     = "/doc/v2/app_management/openapi.yaml"
)

const fullSpec = `openapi: 3.0.3
info: {title: t, version: '1'}
servers: [{url: /v2/app_management}]
paths:
  /apps:
    get: {summary: List the apps, operationId: listApps, responses: {'200': {description: ok}}}
    post:
      summary: Install an app
      operationId: installApp
      parameters: [{name: name, in: query, schema: {type: string}}]
      requestBody: {content: {application/json: {schema: {type: object}}}}
      responses: {'200': {description: ok}}
  /apps/{id}:
    delete:
      summary: Remove an app
      operationId: removeApp
      parameters: [{name: id, in: path, required: true, schema: {type: string}}]
      responses: {'200': {description: ok}}
  /secrets:
    get: {summary: Show the app secrets, operationId: showSecrets, responses: {'200': {description: ok}}}
  /hidden:
    get: {summary: Internal app thing, operationId: hidden, responses: {'200': {description: ok}}}
  /unknown:
    get: {summary: Nobody classified this app thing, operationId: unknownThing, responses: {'200': {description: ok}}}
  /file:
    get: {summary: Download an app file, operationId: appFile, responses: {'200': {description: ok}}}
`

var testClasses = map[string]catalogue.Class{
	"GET /v2/app_management/apps":         catalogue.Read,
	"GET /v2/app_management/file":         catalogue.Read,
	"GET /v2/app_management/secrets":      catalogue.Secret,
	"POST /v2/app_management/apps":        catalogue.Change,
	"DELETE /v2/app_management/apps/{id}": catalogue.Danger,
	"GET /v2/app_management/hidden":       catalogue.Never,
	"GET /v1/sys/utilization":             catalogue.Read,
	"PUT /v1/sys/update-channel":          catalogue.Change,
	"PUT /v1/sys/state/{state}":           catalogue.Danger,
}

// fixture is the endpoint in front of a fake Gateway and a fake accounts service.
type fixture struct {
	t         *testing.T
	srv       *httptest.Server
	guard     *guard
	tokens    *service.Store
	identity  *service.Identity
	catalogue *service.Catalogue
	journal   string
	clock     *clock

	accountCalls atomic.Int32
	actionHits   atomic.Int32
	docHits      atomic.Int32
	gatewayHits  atomic.Int32

	mu      sync.Mutex
	spec    string
	docWait chan struct{}               // when set, the description is served once it is closed
	status  map[int]int                 // of the accounts service, by account; 200 when absent
	actions map[string]http.HandlerFunc // by "METHOD /path"; JSON {"ok":true} when absent
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	tokens, err := service.OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	return newFixtureWith(t, tokens)
}

func newFixtureWith(t *testing.T, tokens *service.Store) *fixture {
	t.Helper()
	f := &fixture{t: t, tokens: tokens, clock: newClock(), spec: fullSpec, status: map[int]int{}, actions: map[string]http.HandlerFunc{}}

	accounts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.accountCalls.Add(1)
		var in struct{ ID int }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if r.Header.Get("Authorization") != "Internal "+testSecret {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.mu.Lock()
		status := f.status[in.ID]
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"access_token": fmt.Sprintf("tok-%d", in.ID)}})
	}))
	t.Cleanup(accounts.Close)

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gatewayHits.Add(1)
		f.mu.Lock()
		spec, action, docWait := f.spec, f.actions[r.Method+" "+r.URL.Path], f.docWait
		f.mu.Unlock()
		switch {
		case r.URL.Path == source:
			f.docHits.Add(1)
			if docWait != nil {
				<-docWait
			}
			w.Header().Set("Content-Type", "application/yaml")
			_, _ = w.Write([]byte(spec))
		case strings.Contains(r.URL.Path, "openapi"):
			http.NotFound(w, r)
		case action != nil:
			f.actionHits.Add(1)
			action(w, r)
		default:
			f.actionHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(gateway.Close)

	runtime := t.TempDir()
	for name, content := range map[string]string{external.UserServiceAddressFilename: accounts.URL, external.InternalSecretFilename: testSecret} {
		if err := os.WriteFile(filepath.Join(runtime, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	u, _ := url.Parse(gateway.URL)
	ini := filepath.Join(runtime, "gateway.ini")
	if err := os.WriteFile(ini, []byte("[gateway]\nport="+u.Port()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f.identity = service.NewIdentity(runtime)
	box := service.NewBox(runtime, ini, f.identity)
	f.catalogue = service.NewCatalogueWithClasses(box, testClasses)
	f.journal = filepath.Join(t.TempDir(), "mcp-calls.log")
	journal := service.NewJournal(f.journal)
	t.Cleanup(func() { _ = journal.Close() })

	f.guard = Handler(Deps{Tokens: tokens, Catalogue: f.catalogue, Box: box, Journal: journal, Now: f.clock.now}).(*guard)
	f.srv = httptest.NewServer(f.guard)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) token(level service.Level, account int) (plain string, t service.Token) {
	f.t.Helper()
	plain, t, err := f.tokens.Create("laptop", account, level)
	if err != nil {
		f.t.Fatal(err)
	}
	return plain, t
}

func (f *fixture) setStatus(account, status int) {
	f.mu.Lock()
	f.status[account] = status
	f.mu.Unlock()
}

func (f *fixture) setAction(key string, h http.HandlerFunc) {
	f.mu.Lock()
	f.actions[key] = h
	f.mu.Unlock()
}

// headers adds fixed headers, and a Host, to every request of the SDK client.
type headers struct {
	h    http.Header
	host string
}

func (a headers) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range a.h {
		r.Header[k] = v
	}
	if a.host != "" {
		r.Host = a.host
	}
	return http.DefaultTransport.RoundTrip(r)
}

func bearer(plain string) http.Header { return http.Header{"Authorization": {"Bearer " + plain}} }

func connectTo(t *testing.T, endpoint string, rt http.RoundTripper) (*mcp.ClientSession, error) {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: &http.Client{Transport: rt}, MaxRetries: -1, DisableStandaloneSSE: true,
	}, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

func (f *fixture) connect(plain string) *mcp.ClientSession {
	f.t.Helper()
	cs, err := connectTo(f.t, f.srv.URL, headers{h: bearer(plain)})
	if err != nil {
		f.t.Fatal(err)
	}
	return cs
}

// call runs a tool and gives the text of its one content.
func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (text string, isError bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error %v", tool, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s: %d contents", tool, len(res.Content))
	}
	return res.Content[0].(*mcp.TextContent).Text, res.IsError
}

func toolNames(t *testing.T, cs *mcp.ClientSession) (string, map[string]*mcp.Tool) {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	byName := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		byName[tool.Name] = tool
	}
	sort.Strings(names)
	return strings.Join(names, ","), byName
}

func unknownText(id string) string {
	return fmt.Sprintf("unknown action %q: use search_actions to find one", id)
}

func TestReadTokenListsThreeTools(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelRead, 7)
	if names, _ := toolNames(t, f.connect(plain)); names != "describe_action,read,search_actions" {
		t.Fatal(names)
	}
}

func TestFullTokenListsFiveTools(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	names, tools := toolNames(t, f.connect(plain))
	if names != "change,describe_action,destroy,read,search_actions" {
		t.Fatal(names)
	}
	for name, want := range map[string]string{
		"search_actions":  `{"idempotentHint":false,"openWorldHint":false,"readOnlyHint":true}`,
		"describe_action": `{"idempotentHint":false,"openWorldHint":false,"readOnlyHint":true}`,
		"read":            `{"idempotentHint":false,"openWorldHint":false,"readOnlyHint":true}`,
		"change":          `{"destructiveHint":false,"idempotentHint":false,"openWorldHint":false,"readOnlyHint":false}`,
		"destroy":         `{"destructiveHint":true,"idempotentHint":false,"openWorldHint":false,"readOnlyHint":false}`,
	} {
		got, _ := json.Marshal(tools[name].Annotations)
		if string(got) != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
		schema, _ := json.Marshal(tools[name].InputSchema)
		if !strings.Contains(string(schema), `"additionalProperties":false`) {
			t.Errorf("%s: schema %s", name, schema)
		}
	}
}

// Any level other than exactly full is read.
func TestUnknownLevelIsTreatedAsRead(t *testing.T) {
	dir := t.TempDir()
	plain := "rcm_someoddlevel"
	sum := sha256Hex(plain)
	file := fmt.Sprintf(`{"version":1,"tokens":[{"id":"odd1","name":"odd","account":7,"level":"admin","created":"2026-11-24T00:00:00Z","last_used":"0001-01-01T00:00:00Z","hash":%q}]}`, sum)
	if err := os.WriteFile(filepath.Join(dir, "tokens.json"), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	tokens, err := service.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil || len(tokens.List()) != 1 {
		t.Fatalf("%v %v", err, tokens.Notes())
	}
	f := newFixtureWith(t, tokens)
	cs := f.connect(plain)
	if names, _ := toolNames(t, cs); names != "describe_action,read,search_actions" {
		t.Fatal(names)
	}
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "change", Arguments: map[string]any{"action": "POST /v2/app_management/apps"}}); err == nil {
		t.Fatal("the change tool answered a token of level admin")
	}
	if text, isErr := call(t, cs, "read", map[string]any{"action": "POST /v2/app_management/apps"}); !isErr || text != unknownText("POST /v2/app_management/apps") {
		t.Fatal(text)
	}
	if text, _ := call(t, cs, "search_actions", map[string]any{"query": "app"}); strings.Contains(text, `"change"`) || strings.Contains(text, "secret") || !strings.Contains(text, "GET /v2/app_management/apps") {
		t.Fatal(text)
	}
	if f.actionHits.Load() != 0 {
		t.Fatal("the box was called")
	}
}

func TestSearchGivesTheToolOfEachAction(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	text, isErr := call(t, f.connect(plain), "search_actions", map[string]any{"query": "app", "limit": 20})
	var hits []map[string]string
	if err := json.Unmarshal([]byte(text), &hits); err != nil || isErr {
		t.Fatalf("%v %s", err, text)
	}
	got := map[string]string{}
	for _, h := range hits {
		got[h["action"]] = h["class"] + "/" + h["tool"]
	}
	want := map[string]string{
		"GET /v2/app_management/apps":         "read/read",
		"GET /v2/app_management/file":         "read/read",
		"GET /v2/app_management/secrets":      "secret/read",
		"POST /v2/app_management/apps":        "change/change",
		"DELETE /v2/app_management/apps/{id}": "danger/destroy",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%v\nwant %v", got, want)
	}
}

func TestSearchRefusesABadLimit(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	cs := f.connect(plain)
	for _, args := range []map[string]any{{"query": "x", "limit": 21}, {"query": "x", "limit": 0}, {"limit": 3}, {"query": "x", "other": 1}} {
		if text, isErr := call(t, cs, "search_actions", args); !isErr || !strings.HasPrefix(text, "invalid arguments") {
			t.Errorf("%v: %s", args, text)
		}
	}
}

func TestReadToolRefusesAChangeAction(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	text, isErr := call(t, f.connect(plain), "read", map[string]any{"action": "POST /v2/app_management/apps"})
	if !isErr || text != `use the "change" tool for this action` || f.actionHits.Load() != 0 {
		t.Fatal(text)
	}
}

func TestChangeToolRefusesADangerAction(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	text, isErr := call(t, f.connect(plain), "change", map[string]any{"action": "DELETE /v2/app_management/apps/{id}", "params": map[string]any{"id": "x"}})
	if !isErr || text != `use the "destroy" tool for this action` || f.actionHits.Load() != 0 {
		t.Fatal(text)
	}
}

func TestReadTokenCannotCallASecretAction(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelRead, 7)
	cs := f.connect(plain)
	if text, isErr := call(t, cs, "read", map[string]any{"action": "GET /v2/app_management/secrets"}); !isErr || text != unknownText("GET /v2/app_management/secrets") {
		t.Fatal(text)
	}
	if text, _ := call(t, cs, "search_actions", map[string]any{"query": "secrets"}); text != "[]" {
		t.Fatal(text)
	}
	if f.actionHits.Load() != 0 {
		t.Fatal("the box was called")
	}

	full, _ := f.token(service.LevelFull, 7)
	if text, isErr := call(t, f.connect(full), "read", map[string]any{"action": "GET /v2/app_management/secrets"}); isErr || !strings.HasPrefix(text, `{"status":200`) {
		t.Fatalf("a full token cannot read a secret action: %s", text)
	}
}

func TestReadTokenDescribeOfAChangeActionLooksUnknown(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelRead, 7)
	cs := f.connect(plain)
	for _, id := range []string{"POST /v2/app_management/apps", "GET /v2/app_management/secrets", "GET /v2/nothing"} {
		if text, isErr := call(t, cs, "describe_action", map[string]any{"action": id}); !isErr || text != unknownText(id) {
			t.Errorf("%s: %s", id, text)
		}
	}
	text, isErr := call(t, cs, "describe_action", map[string]any{"action": "GET /v2/app_management/apps"})
	if isErr || !strings.Contains(text, `"tool":"read"`) || !strings.Contains(text, `"callable":true`) {
		t.Fatal(text)
	}
}

func TestNeverActionIsUnknown(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	cs := f.connect(plain)
	for _, tool := range []string{"read", "change", "destroy"} {
		if text, isErr := call(t, cs, tool, map[string]any{"action": "GET /v2/app_management/hidden"}); !isErr || text != unknownText("GET /v2/app_management/hidden") {
			t.Fatalf("%s: %s", tool, text)
		}
	}
}

func TestUnclassedActionIsUnknown(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	if text, isErr := call(t, f.connect(plain), "read", map[string]any{"action": "GET /v2/app_management/unknown"}); !isErr || text != unknownText("GET /v2/app_management/unknown") {
		t.Fatal(text)
	}
}

func TestUnknownActionIdIsCut(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	long := strings.Repeat("é", 500)
	if text, _ := call(t, f.connect(plain), "read", map[string]any{"action": long}); text != unknownText(strings.Repeat("é", 200)) {
		t.Fatal(len(text))
	}
}

func TestActionGoneAfterRefreshIsUnknown(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	cs := f.connect(plain)
	if text, _ := call(t, cs, "search_actions", map[string]any{"query": "list apps"}); !strings.Contains(text, "GET /v2/app_management/apps") {
		t.Fatal(text)
	}
	f.mu.Lock()
	f.spec = strings.Replace(fullSpec, "    get: {summary: List the apps, operationId: listApps, responses: {'200': {description: ok}}}\n", "", 1)
	f.mu.Unlock()

	// within ten minutes the catalogue is not read again
	f.clock.add(9 * time.Minute)
	if _, isErr := call(t, cs, "read", map[string]any{"action": "GET /v2/app_management/apps"}); isErr {
		t.Fatal("the catalogue was refreshed before ten minutes")
	}
	f.clock.add(2 * time.Minute)
	// the stale catalogue still serves this call and starts a refresh behind it
	call(t, cs, "read", map[string]any{"action": "GET /v2/app_management/apps"})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		text, isErr := call(t, cs, "read", map[string]any{"action": "GET /v2/app_management/apps"})
		if isErr && text == unknownText("GET /v2/app_management/apps") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the action is still served: %s", text)
		}
	}
}

// callWithin runs a tool with a deadline of its own and reports how long it took.
func callWithin(t *testing.T, cs *mcp.ClientSession, d time.Duration, tool string, args map[string]any) (*mcp.CallToolResult, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	start := time.Now()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	return res, time.Since(start), err
}

func TestASlowRefreshDoesNotHoldUpCalls(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	cs := f.connect(plain)
	if _, isErr := call(t, cs, "read", map[string]any{"action": "GET /v2/app_management/apps"}); isErr {
		t.Fatal("first call")
	}
	release := make(chan struct{})
	f.mu.Lock()
	f.docWait = release
	f.spec = strings.Replace(fullSpec, "    get: {summary: List the apps, operationId: listApps, responses: {'200': {description: ok}}}\n", "", 1)
	f.mu.Unlock()
	t.Cleanup(func() { close(release) }) // before the fake Gateway closes: it waits for its requests

	f.clock.add(11 * time.Minute)
	res, took, err := callWithin(t, cs, 5*time.Second, "read", map[string]any{"action": "GET /v2/app_management/apps"})
	if err != nil || res.IsError || took > 2*time.Second {
		t.Fatalf("%v %+v after %v", err, res, took)
	}
	for deadline := time.Now().Add(5 * time.Second); f.docHits.Load() != 2; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no refresh was started")
		}
	}
	// a second call while the refresh hangs starts no other one
	if res, _, err := callWithin(t, cs, 5*time.Second, "read", map[string]any{"action": "GET /v2/app_management/apps"}); err != nil || res.IsError || f.docHits.Load() != 2 {
		t.Fatalf("%v %+v, %d description reads", err, res, f.docHits.Load())
	}
}

func TestASlowActionDoesNotHoldUpOtherCalls(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	cs := f.connect(plain)
	release, started := make(chan struct{}), make(chan struct{})
	f.setAction("GET /v2/app_management/file", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "read", Arguments: map[string]any{"action": "GET /v2/app_management/file"}})
	}()
	defer func() { close(release); <-done }()
	<-started

	res, took, err := callWithin(t, cs, 5*time.Second, "read", map[string]any{"action": "GET /v2/app_management/apps"})
	if err != nil || res.IsError || took > 2*time.Second {
		t.Fatalf("%v %+v after %v", err, res, took)
	}
}

func TestFirstCallWaitsForTheFirstRefresh(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	release := make(chan struct{})
	f.mu.Lock()
	f.docWait = release
	f.mu.Unlock()
	go func() { time.Sleep(100 * time.Millisecond); close(release) }()
	if text, isErr := call(t, f.connect(plain), "read", map[string]any{"action": "GET /v2/app_management/apps"}); isErr || text != `{"status":200,"body":{"ok":true},"truncated":false}` {
		t.Fatal(text)
	}
}

func TestNotLoadedYetIsSaidPlainly(t *testing.T) {
	f := newFixture(t)
	f.setStatus(7, http.StatusInternalServerError)
	plain, _ := f.token(service.LevelFull, 7)
	cs := f.connect(plain)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"search_actions", map[string]any{"query": "apps"}},
		{"read", map[string]any{"action": "GET /v2/app_management/apps"}},
		{"describe_action", map[string]any{"action": "GET /v2/app_management/apps"}},
	} {
		if text, isErr := call(t, cs, c.tool, c.args); !isErr || text != notLoaded {
			t.Errorf("%s: %s", c.tool, text)
		}
	}
	if n := f.gatewayHits.Load(); n != 0 {
		t.Fatalf("%d requests reached the Gateway", n)
	}
}

func TestConcurrentCallsShareOneRefresh(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	cs := f.connect(plain)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "read", Arguments: map[string]any{"action": "GET /v2/app_management/apps"}})
			if err != nil || res.IsError {
				t.Errorf("%v %+v", err, res)
			}
		}()
	}
	wg.Wait()
	if n := f.docHits.Load(); n != 1 {
		t.Fatalf("the description was read %d times, want 1", n)
	}
}

func TestBoxErrorIsAToolResult(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	f.setAction("POST /v2/app_management/apps", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Query().Get("name") != "nginx" || string(body) != `{"x":1}` {
			t.Errorf("query %q body %s", r.URL.RawQuery, body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"already installed","id":12345678901234567890}`))
	})
	text, isErr := call(t, f.connect(plain), "change", map[string]any{"action": "POST /v2/app_management/apps", "params": map[string]any{"name": "nginx"}, "body": map[string]any{"x": 1}})
	if !isErr || text != `{"status":409,"body":{"message":"already installed","id":12345678901234567890},"truncated":false}` {
		t.Fatal(text)
	}
}

func TestTextAndCutRepliesAreText(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	f.setAction("GET /v2/app_management/apps", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("123"))
	})
	cs := f.connect(plain)
	if text, _ := call(t, cs, "read", map[string]any{"action": "GET /v2/app_management/apps"}); text != `{"status":200,"body":"123","truncated":false}` {
		t.Fatal(text)
	}
	f.setAction("GET /v2/app_management/apps", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`["` + strings.Repeat("a", 300<<10) + `"]`))
	})
	text, _ := call(t, cs, "read", map[string]any{"action": "GET /v2/app_management/apps"})
	var got struct {
		Body      any
		Truncated bool
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil || !got.Truncated {
		t.Fatalf("%v %v", err, got.Truncated)
	}
	if s, ok := got.Body.(string); !ok || !strings.HasPrefix(s, `["aaa`) {
		t.Fatal("a cut JSON reply is not given as text")
	}
}

func TestIdentityDownIsAToolError(t *testing.T) {
	f := newFixture(t)
	loader, _ := f.token(service.LevelFull, 7)
	down, _ := f.token(service.LevelFull, 8)
	f.setStatus(8, http.StatusInternalServerError)
	// the catalogue is loaded by a working account first: without it every action is unknown
	call(t, f.connect(loader), "search_actions", map[string]any{"query": "apps"})
	hits := f.actionHits.Load()

	text, isErr := call(t, f.connect(down), "read", map[string]any{"action": "GET /v2/app_management/apps"})
	if !isErr || text != "the box's account service is not answering; try again in a moment" || f.actionHits.Load() != hits {
		t.Fatalf("%s, gateway hits %d", text, f.actionHits.Load()-hits)
	}
}

func TestToolResultCarriesNoSecrets(t *testing.T) {
	f := newFixture(t)
	loader, _ := f.token(service.LevelFull, 7)
	f.setStatus(8, http.StatusInternalServerError)
	f.setStatus(9, http.StatusNotFound)
	down, _ := f.token(service.LevelFull, 8)
	gone, _ := f.token(service.LevelFull, 9)
	f.setAction("GET /v2/app_management/file", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte{0, 1, 2})
	})
	f.setAction("POST /v2/app_management/apps", func(w http.ResponseWriter, r *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close() // no answer at all
	})
	cs := f.connect(loader)

	var texts []string
	add := func(cs *mcp.ClientSession, tool string, args map[string]any, want string) {
		text, isErr := call(t, cs, tool, args)
		if !isErr || (want != "" && text != want) {
			t.Errorf("%s %v: %s", tool, args, text)
		}
		texts = append(texts, text)
	}
	add(cs, "read", map[string]any{"action": "GET /v2/app_management/file"}, "this action answers with a file or binary data, which this tool cannot return")
	add(cs, "change", map[string]any{"action": "POST /v2/app_management/apps", "params": map[string]any{"name": "x"}}, "the call could not be made")
	add(cs, "destroy", map[string]any{"action": "DELETE /v2/app_management/apps/{id}"}, "")
	add(cs, "read", map[string]any{"action": "GET /v2/nothing"}, unknownText("GET /v2/nothing"))
	add(cs, "destroy", map[string]any{"action": "DELETE /v2/app_management/apps/{id}", "params": map[string]any{"id": "x", "extra": 1}}, "")
	add(f.connect(down), "read", map[string]any{"action": "GET /v2/app_management/apps"}, "the box's account service is not answering; try again in a moment")
	add(f.connect(gone), "read", map[string]any{"action": "GET /v2/app_management/apps"}, "the account this token belongs to no longer exists")

	for _, text := range texts {
		for _, secret := range []string{testSecret, loader, down, gone, "tok-"} {
			if strings.Contains(text, secret) {
				t.Errorf("%q carries %q", text, secret)
			}
		}
	}
}

func TestJournalHasNoBodyAndNoToken(t *testing.T) {
	f := newFixture(t)
	plain, tok := f.token(service.LevelFull, 7)
	cs := f.connect(plain)
	call(t, cs, "search_actions", map[string]any{"query": "QUERYMARK"})
	call(t, cs, "change", map[string]any{"action": "POST /v2/app_management/apps", "params": map[string]any{"name": "PARAMMARK"}, "body": map[string]any{"x": "BODYMARK"}})
	call(t, cs, "read", map[string]any{"action": "GET /v2/IDMARK\tforged"})

	raw, err := os.ReadFile(f.journal)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"QUERYMARK", "PARAMMARK", "BODYMARK", "IDMARK", plain, testSecret, "tok-"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the journal holds %q", leak)
		}
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	want := [][]string{
		{tok.ID, "laptop", "search_actions", "-", "ok"},
		{tok.ID, "laptop", "change", "POST /v2/app_management/apps", "200"},
		{tok.ID, "laptop", "read", "-", "error"},
	}
	if len(lines) != len(want) {
		t.Fatalf("%q", raw)
	}
	for i, l := range lines {
		fields := strings.Split(l, "\t")
		if _, err := time.Parse(time.RFC3339, fields[0]); err != nil || fmt.Sprint(fields[1:]) != fmt.Sprint(want[i]) {
			t.Errorf("line %d: %q", i, l)
		}
	}
}

// del revokes a token the way the owner's DELETE /v2/mcp/tokens/{id} does.
func (f *fixture) del(id string) {
	f.t.Helper()
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodDelete, "/v2/mcp/tokens/"+id, nil), rec)
	if err := v2Route.NewMCP(f.tokens, f.catalogue, f.identity, nil).DeleteToken(c, id); err != nil || rec.Code != http.StatusNoContent {
		f.t.Fatalf("%v %d", err, rec.Code)
	}
}

func TestRevokedTokenStopsAtOnce(t *testing.T) {
	f := newFixture(t)
	first, a := f.token(service.LevelFull, 7)
	cs := f.connect(first)
	if _, isErr := call(t, cs, "read", map[string]any{"action": "GET /v2/app_management/apps"}); isErr {
		t.Fatal("first call")
	}
	asked := f.accountCalls.Load()

	f.del(a.ID)
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "read", Arguments: map[string]any{"action": "GET /v2/app_management/apps"}}); err == nil {
		t.Fatal("the revoked token still works")
	}
	if code := post(t, f.srv.URL, bearer(first), listBody).StatusCode; code != http.StatusUnauthorized {
		t.Fatalf("%d, want 401", code)
	}

	// it was the account's last token: its access token was forgotten, so a new token asks again
	second, _ := f.token(service.LevelFull, 7)
	call(t, f.connect(second), "read", map[string]any{"action": "GET /v2/app_management/apps"})
	if n := f.accountCalls.Load() - asked; n != 1 {
		t.Fatalf("%d access tokens asked after the last token went, want 1", n)
	}

	// with another token left on the account, nothing is forgotten
	_, third := f.token(service.LevelFull, 7)
	asked = f.accountCalls.Load()
	f.del(third.ID)
	call(t, f.connect(second), "read", map[string]any{"action": "GET /v2/app_management/apps"})
	if n := f.accountCalls.Load() - asked; n != 0 {
		t.Fatalf("%d access tokens asked while the account kept a token, want 0", n)
	}
}

func TestStatusCountsActions(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelRead, 7)
	call(t, f.connect(plain), "search_actions", map[string]any{"query": "apps"})

	e := echo.New()
	rec := httptest.NewRecorder()
	if err := v2Route.NewMCP(f.tokens, f.catalogue, f.identity, nil).GetStatus(e.NewContext(httptest.NewRequest(http.MethodGet, "/v2/mcp/status", nil), rec)); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Actions      map[string]int
		Unclassified int
		Tokens       int
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got.Actions) != "map[change:2 danger:2 read:3 secret:1]" || got.Unclassified != 55 || got.Tokens != 1 { // 55 = the fixture's /unknown + the 54 embedded system actions its testClasses leaves out
		t.Fatalf("%s", rec.Body)
	}
}

func TestReadTokenDoesNotSeeAnEmbeddedDangerAction(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelRead, 7)
	cs := f.connect(plain)
	const id = "PUT /v1/sys/state/{state}"
	if text, _ := call(t, cs, "search_actions", map[string]any{"query": "shut down the box"}); strings.Contains(text, id) {
		t.Fatal(text)
	}
	for _, tool := range []string{"read", "describe_action"} {
		if text, isErr := call(t, cs, tool, map[string]any{"action": id}); !isErr || text != unknownText(id) {
			t.Fatalf("%s: %s", tool, text)
		}
	}
	if f.actionHits.Load() != 0 {
		t.Fatal("the box was called")
	}
}

// Before any description of the box is read, an embedded action is found and an other one is not loaded yet.
func TestEmbeddedActionIsFoundBeforeTheBoxDescriptionsLoad(t *testing.T) {
	f := newFixture(t)
	f.setStatus(7, http.StatusInternalServerError)
	plain, _ := f.token(service.LevelFull, 7)
	cs := f.connect(plain)
	if text, isErr := call(t, cs, "describe_action", map[string]any{"action": "GET /v1/sys/utilization"}); isErr || !strings.Contains(text, `"tool":"read"`) {
		t.Fatalf("%s", text)
	}
	if text, isErr := call(t, cs, "describe_action", map[string]any{"action": "GET /v2/app_management/apps"}); !isErr || text != notLoaded {
		t.Fatalf("%s", text)
	}
}

func TestEmbeddedActionOnAnOlderBoxIsTheBoxes404(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	f.setAction("PUT /v1/sys/update-channel", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	text, isErr := call(t, f.connect(plain), "change", map[string]any{"action": "PUT /v1/sys/update-channel", "body": map[string]any{"channel": "release"}})
	if !isErr || !strings.HasPrefix(text, `{"status":404,`) {
		t.Fatal(text)
	}
}

func TestEmbeddedActionIsNotRetried(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	f.setAction("PUT /v1/sys/update-channel", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	text, isErr := call(t, f.connect(plain), "change", map[string]any{"action": "PUT /v1/sys/update-channel", "body": map[string]any{"channel": "release"}})
	if !isErr || !strings.HasPrefix(text, `{"status":500,`) {
		t.Fatal(text)
	}
	if n := f.actionHits.Load(); n != 1 {
		t.Fatalf("the box received %d requests, want 1", n)
	}
}

func TestSearchFindsEmbeddedActionsBeforeTheBoxLoads(t *testing.T) {
	f := newFixture(t)
	f.setStatus(7, http.StatusInternalServerError)
	for level, wantOnly := range map[service.Level]bool{service.LevelFull: false, service.LevelRead: true} {
		plain, _ := f.token(level, 7)
		cs := f.connect(plain)
		text, isErr := call(t, cs, "search_actions", map[string]any{"query": "utilization"})
		var got struct {
			Actions []map[string]string
			Note    string
		}
		if err := json.Unmarshal([]byte(text), &got); err != nil || isErr {
			t.Fatalf("%s: %v %s", level, err, text)
		}
		if len(got.Actions) != 1 || got.Actions[0]["action"] != "GET /v1/sys/utilization" || got.Note != notLoadedNote {
			t.Fatalf("%s: %s", level, text)
		}
		// the danger and change samples: a full token finds them, a read token never
		text, _ = call(t, cs, "search_actions", map[string]any{"query": "state update-channel"})
		if sees := strings.Contains(text, "PUT /v1/sys/"); sees == wantOnly {
			t.Fatalf("%s sees them: %v: %s", level, sees, text)
		}
	}
	if n := f.gatewayHits.Load(); n != 0 {
		t.Fatalf("%d requests reached the Gateway", n)
	}
}

func TestReadTokenDoesNotSeeAnEmbeddedDangerActionBeforeTheBoxLoads(t *testing.T) {
	f := newFixture(t)
	f.setStatus(7, http.StatusInternalServerError)
	plain, _ := f.token(service.LevelRead, 7)
	if text, isErr := call(t, f.connect(plain), "read", map[string]any{"action": "PUT /v1/sys/state/{state}"}); !isErr || text != notLoaded {
		t.Fatal(text)
	}
}

func TestSearchWithNoHitBeforeTheBoxLoadsSaysNotLoaded(t *testing.T) {
	f := newFixture(t)
	f.setStatus(7, http.StatusInternalServerError)
	plain, _ := f.token(service.LevelFull, 7)
	if text, isErr := call(t, f.connect(plain), "search_actions", map[string]any{"query": "zzzzz"}); !isErr || text != notLoaded {
		t.Fatal(text)
	}
}

func TestSearchAfterLoadHasNoNote(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	text, isErr := call(t, f.connect(plain), "search_actions", map[string]any{"query": "utilization"})
	if isErr || strings.Contains(text, "note") || !strings.HasPrefix(text, `[{"action":"GET /v1/sys/utilization"`) {
		t.Fatal(text)
	}
}

// A route the catalogue does not describe is reached by no tool: the id is unknown
// and the box is not called. Named here, what writes, tries or previews an API widget.
func TestNoToolReachesAnAPIWidgetWrite(t *testing.T) {
	f := newFixture(t)
	plain, _ := f.token(service.LevelFull, 7)
	cs := f.connect(plain)
	for _, id := range []string{
		"POST /v1/sys/widgets",
		"PUT /v1/sys/widgets/{id}",
		"PUT /v1/sys/widgets/abcdefgh",
		"DELETE /v1/sys/widgets/{id}",
		"DELETE /v1/sys/widgets/abcdefgh",
		"POST /v1/sys/widgets/test",
		"POST /v1/sys/widgets/preview",
	} {
		for _, tool := range []string{"describe_action", "read", "change", "destroy"} {
			if text, isErr := call(t, cs, tool, map[string]any{"action": id}); !isErr || text != unknownText(id) {
				t.Errorf("%s through %s: %s", id, tool, text)
			}
		}
	}
	if f.actionHits.Load() != 0 {
		t.Fatal("the box was called")
	}
}
