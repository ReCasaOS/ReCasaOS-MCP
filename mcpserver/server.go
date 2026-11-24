// Package mcpserver is the MCP endpoint: an AI client holding a token searches the box's API,
// reads how to call an action and runs it, as the token's account.
package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/ReCasaOS-MCP/common"
	"github.com/ReCasaOS/ReCasaOS-MCP/service"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

const (
	refreshEvery   = 10 * time.Minute
	retryFailed    = time.Minute
	refreshTimeout = 2 * time.Minute
)

// Deps is what the endpoint is built on.
type Deps struct {
	Tokens    *service.Store
	Catalogue *service.Catalogue
	Box       *service.Box
	Journal   *service.Journal
	// Now is the clock of the rate limit and of the catalogue's age; nil is time.Now.
	Now func() time.Time
}

// Handler serves POST /v2/mcp/server: the guard, then the MCP SDK's stateless JSON handler
// with the tools the token's level allows.
func Handler(deps Deps) http.Handler {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	ts := &tools{deps: deps, refresh: &refresher{cat: deps.Catalogue, now: now}}
	sdk := mcp.NewStreamableHTTPHandler(ts.server, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		// Behind the Gateway a request arrives on loopback with the box's own host name.
		DisableLocalhostProtection: true,
	})
	return &guard{tokens: deps.Tokens, limit: newLimiter(now, maxAddresses), next: sdk}
}

func ptr[T any](v T) *T { return &v }

type toolDef struct {
	name, description string
	schema            string
	annotations       mcp.ToolAnnotations
	fullOnly          bool
}

const actionProperty = `"action":{"type":"string","description":"The action's id as search_actions gives it, e.g. \"GET /v2/app_management/web/appgrid\"."}`

const runSchema = `{"type":"object","properties":{` + actionProperty + `,` +
	`"params":{"type":"object","description":"Path and query parameters by name, as describe_action lists them."},` +
	`"body":{"type":["object","array"],"description":"The JSON body, when describe_action says the action takes one."}},` +
	`"required":["action"],"additionalProperties":false}`

const runResult = ` Returns JSON {"status": the box's HTTP status, "body": its answer, "truncated": whether the answer was cut}. Every action runs as the account that created your token, and the box's own rights apply.`

var toolDefs = []toolDef{
	{
		name: "search_actions",
		description: `Search the actions of this home server's API: apps, storage, files, users, settings, virtual machines and more. ` +
			`Start here. Give a few words of what you want to do; each result names the action, what it does, its class and the tool that runs it. ` +
			`Then call describe_action on the action before running it with that tool.`,
		schema: `{"type":"object","properties":{` +
			`"query":{"type":"string","description":"A few words of what you want to do, e.g. \"list installed apps\"."},` +
			`"limit":{"type":"integer","minimum":1,"maximum":20,"default":10,"description":"How many results to return, 1 to 20."}},` +
			`"required":["query"],"additionalProperties":false}`,
		annotations: mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	},
	{
		name: "describe_action",
		description: `Show how to call one action found with search_actions: its path and query parameters, whether it takes a JSON body and the body's schema, ` +
			`and the tool that runs it. Call it before running an action you have not described yet.`,
		schema:      `{"type":"object","properties":{` + actionProperty + `},"required":["action"],"additionalProperties":false}`,
		annotations: mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	},
	{
		name: "read",
		description: `Run an action that only reads: one whose tool is "read" in search_actions results. ` +
			`Find it with search_actions and check its parameters with describe_action first.` + runResult,
		schema:      runSchema,
		annotations: mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	},
	{
		name: "change",
		description: `Run an action that changes the server without destroying data, such as installing an app or changing a setting: one whose tool is "change" in search_actions results. ` +
			`Find it with search_actions and check its parameters with describe_action first; ask the user before a change they did not request.` + runResult,
		schema:      runSchema,
		annotations: mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(false)},
		fullOnly:    true,
	},
	{
		name: "destroy",
		description: `Run an action that can destroy data or stop services, such as removing an app, deleting files or formatting a disk: one whose tool is "destroy" in search_actions results. ` +
			`Find it with search_actions and check its parameters with describe_action first, and ask the user to confirm every call.` + runResult,
		schema:      runSchema,
		annotations: mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(false)},
		fullOnly:    true,
	},
}

// server builds the tools of one request, bound to its token. The SDK may call it more than
// once per request, so it does nothing else.
func (ts *tools) server(r *http.Request) *mcp.Server {
	t, ok := tokenFrom(r.Context())
	if !ok {
		return nil
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "casaos-mcp", Version: common.Version}, nil)
	for _, d := range toolDefs {
		if d.fullOnly && t.Level != service.LevelFull {
			continue
		}
		annotations := d.annotations
		s.AddTool(&mcp.Tool{Name: d.name, Description: d.description, InputSchema: json.RawMessage(d.schema), Annotations: &annotations}, ts.handler(t, d.name))
	}
	return s
}

// refresher loads the catalogue on use: at the first tool call, then when it is older than
// refreshEvery. Concurrent calls share one refresh.
type refresher struct {
	cat *service.Catalogue
	now func() time.Time

	mu      sync.Mutex
	next    time.Time // zero: never loaded
	running chan struct{}
}

func (r *refresher) ensure(ctx context.Context, account int) {
	r.mu.Lock()
	if r.running != nil {
		wait := r.running
		r.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
		}
		return
	}
	if r.now().Before(r.next) {
		r.mu.Unlock()
		return
	}
	done := make(chan struct{})
	r.running = done
	r.mu.Unlock()

	// detached: the callers waiting on this refresh must not fail with the first one
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	err := r.cat.Refresh(rctx, account)
	cancel()
	logRefresh(err)

	r.mu.Lock()
	r.next = r.now().Add(refreshEvery)
	if err != nil {
		r.next = r.now().Add(retryFailed)
	}
	r.running = nil
	close(done)
	r.mu.Unlock()
}

func logRefresh(err error) {
	if err == nil {
		return
	}
	errs := []error{err}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		errs = j.Unwrap()
	}
	for _, e := range errs {
		logger.Error("a description of the box's API could not be read", zap.Error(e))
	}
}
