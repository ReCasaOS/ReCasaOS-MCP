package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/ReCasaOS-MCP/catalogue"
	"github.com/ReCasaOS/ReCasaOS-MCP/service"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

type tools struct {
	deps    Deps
	refresh *refresher
}

// outcome is one tool call's answer and what the journal says of it.
type outcome struct {
	text    string
	isError bool
	action  string // "-" unless the action was found and may be seen
	status  string
}

func failure(action, text string) outcome {
	return outcome{text: text, isError: true, action: action, status: "error"}
}

func (ts *tools) handler(t service.Token, tool string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ts.refresh.ensure(ctx, t.Account)
		var o outcome
		switch tool {
		case "search_actions":
			o = ts.search(t, req.Params.Arguments)
		case "describe_action":
			o = ts.describe(t, req.Params.Arguments)
		default:
			o = ts.run(ctx, t, tool, req.Params.Arguments)
		}
		ts.deps.Journal.Write(t, tool, o.action, o.status)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: o.text}}, IsError: o.isError}, nil
	}
}

// decode reads the arguments strictly: an unknown field or a wrong type is refused.
func decode(raw json.RawMessage, into any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("invalid arguments: %v", err)
	}
	return nil
}

// toolFor is the tool that runs an action of class c.
func toolFor(c catalogue.Class) string {
	switch c {
	case catalogue.Read, catalogue.Secret:
		return "read"
	case catalogue.Change:
		return "change"
	case catalogue.Danger:
		return "destroy"
	}
	return ""
}

// visible finds an action the level may see. A token whose level is not exactly full sees
// class read only; anything else looks exactly like an action that does not exist.
func (ts *tools) visible(id string, level service.Level) (service.Action, bool) {
	a, ok := ts.deps.Catalogue.Get(id)
	if !ok || toolFor(a.Class) == "" || (level != service.LevelFull && a.Class != catalogue.Read) {
		return service.Action{}, false
	}
	return a, true
}

func unknown(id string) outcome {
	if r := []rune(id); len(r) > 200 {
		id = string(r[:200])
	}
	return failure("-", fmt.Sprintf("unknown action %q: use search_actions to find one", id))
}

func asJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `"the answer could not be written"`
	}
	return string(b)
}

func (ts *tools) search(t service.Token, raw json.RawMessage) outcome {
	var in struct {
		Query *string `json:"query"`
		Limit *int    `json:"limit"`
	}
	if err := decode(raw, &in); err != nil {
		return failure("-", err.Error())
	}
	if in.Query == nil {
		return failure("-", "invalid arguments: query is required")
	}
	limit := 10
	if in.Limit != nil {
		limit = *in.Limit
	}
	if limit < 1 || limit > 20 {
		return failure("-", "invalid arguments: limit must be between 1 and 20")
	}
	type hit struct {
		Action  string `json:"action"`
		Summary string `json:"summary"`
		Class   string `json:"class"`
		Tool    string `json:"tool"`
	}
	hits := []hit{}
	for _, a := range ts.deps.Catalogue.Search(*in.Query, t.Level, limit) {
		if _, ok := ts.visible(a.ID, t.Level); ok {
			hits = append(hits, hit{a.ID, a.Summary, string(a.Class), toolFor(a.Class)})
		}
	}
	return outcome{text: asJSON(hits), action: "-", status: "ok"}
}

func (ts *tools) describe(t service.Token, raw json.RawMessage) outcome {
	var in struct {
		Action *string `json:"action"`
	}
	if err := decode(raw, &in); err != nil {
		return failure("-", err.Error())
	}
	if in.Action == nil {
		return failure("-", "invalid arguments: action is required")
	}
	a, ok := ts.visible(*in.Action, t.Level)
	if !ok {
		return unknown(*in.Action)
	}
	d := service.DescribeAction(a)
	d["tool"] = toolFor(a.Class)
	return outcome{text: asJSON(d), action: a.ID, status: "ok"}
}

func (ts *tools) run(ctx context.Context, t service.Token, tool string, raw json.RawMessage) outcome {
	var in struct {
		Action *string         `json:"action"`
		Params map[string]any  `json:"params"`
		Body   json.RawMessage `json:"body"`
	}
	if err := decode(raw, &in); err != nil {
		return failure("-", err.Error())
	}
	if in.Action == nil {
		return failure("-", "invalid arguments: action is required")
	}
	if b := bytes.TrimSpace(in.Body); len(b) > 0 && b[0] != '{' && b[0] != '[' {
		return failure("-", "invalid arguments: body must be a JSON object or array")
	}

	// the level before the class: a read token is never told which tool an action needs
	a, ok := ts.visible(*in.Action, t.Level)
	if !ok {
		return unknown(*in.Action)
	}
	if want := toolFor(a.Class); want != tool {
		return failure(a.ID, fmt.Sprintf("use the %q tool for this action", want))
	}

	path, query, body, err := service.BuildRequest(a, in.Params, in.Body)
	if err != nil {
		return failure(a.ID, err.Error())
	}
	rep, err := ts.deps.Box.Do(ctx, t.Account, a.Method, path, query, body)
	if err != nil {
		return failure(a.ID, callError(a, err))
	}

	result := struct {
		Status    int  `json:"status"`
		Body      any  `json:"body"`
		Truncated bool `json:"truncated"`
	}{Status: rep.Status, Body: string(rep.Body), Truncated: rep.Truncated}
	if !rep.Truncated && isJSON(rep.ContentType) && json.Valid(rep.Body) {
		result.Body = json.RawMessage(rep.Body)
	}
	return outcome{text: asJSON(result), isError: rep.Status >= 400, action: a.ID, status: strconv.Itoa(rep.Status)}
}

func isJSON(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && (mt == "application/json" || strings.HasSuffix(mt, "+json"))
}

// callError is the sentence an AI client gets when the box gave no answer. It never carries
// the error itself, which may hold an address or a parameter's value.
func callError(a service.Action, err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, service.ErrIdentityUnavailable):
		return "the box's account service is not answering; try again in a moment"
	case errors.Is(err, service.ErrAccountGone):
		return "the account this token belongs to no longer exists"
	case errors.Is(err, service.ErrBinaryReply):
		return "this action answers with a file or binary data, which this tool cannot return"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "the box did not answer within 60 seconds"
	}
	// a *url.Error holds the full URL, query included: log what is under it
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	logger.Error("an MCP tool call could not be made", zap.String("action", a.ID), zap.Error(err))
	return "the call could not be made"
}
