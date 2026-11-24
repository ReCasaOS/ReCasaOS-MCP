package service

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// The parameters come from a language model that may have read hostile text.
// A value therefore only ever lands inside one escaped path segment or one
// query value: it can neither add a segment nor a query.

const (
	maxEcho        = 80       // longest value an error message repeats
	maxDescription = 32 << 10 // size DescribeAction aims to stay under
	maxDepth       = 6        // nested schema levels shown
)

// shown trims a value an error message repeats.
func shown(s string) string {
	if len(s) > maxEcho {
		s = s[:maxEcho] + "..."
	}
	return strconv.Quote(s)
}

// jsonMediaType returns the JSON media type of a request body, if it has one.
func jsonMediaType(rb *openapi3.RequestBody) *openapi3.MediaType {
	if mt := rb.Content["application/json"]; mt != nil {
		return mt
	}
	keys := make([]string, 0, len(rb.Content))
	for k := range rb.Content {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if m, _, err := mime.ParseMediaType(k); err == nil && strings.HasSuffix(m, "+json") && rb.Content[k] != nil {
			return rb.Content[k]
		}
	}
	return nil
}

// placeholder says whether a path segment is a {name}; ok is false for a
// segment that mixes text and braces.
func placeholder(seg string) (name string, isParam, ok bool) {
	if !strings.ContainsAny(seg, "{}") {
		return "", false, true
	}
	if len(seg) < 3 || seg[0] != '{' || seg[len(seg)-1] != '}' || strings.ContainsAny(seg[1:len(seg)-1], "{}") {
		return "", false, false
	}
	return seg[1 : len(seg)-1], true, true
}

// uncallable says why no parameters could make this action callable, or "".
func uncallable(a Action) string {
	if a.Operation == nil {
		return "the description of this action is missing"
	}
	for _, seg := range strings.Split(a.Path, "/") {
		if _, _, ok := placeholder(seg); !ok {
			return fmt.Sprintf("the path of %s mixes text and a parameter in one segment", a.ID)
		}
	}
	for _, p := range a.Operation.Parameters {
		if p != nil && p.Value != nil && p.Value.Required && (p.Value.In == "header" || p.Value.In == "cookie") {
			return fmt.Sprintf("it requires the %s parameter %s", p.Value.In, shown(p.Value.Name))
		}
	}
	if rb := a.Operation.RequestBody; rb != nil {
		if rb.Value == nil {
			return "its request body is not described"
		}
		if jsonMediaType(rb.Value) == nil {
			return "its request body is not JSON"
		}
	}
	return ""
}

// paramType is the declared type of a parameter's schema, or "".
func paramType(p *openapi3.Parameter) string {
	if p == nil || p.Schema == nil || p.Schema.Value == nil || p.Schema.Value.Type == nil {
		return ""
	}
	if s := p.Schema.Value.Type.Slice(); len(s) > 0 {
		return s[0]
	}
	return ""
}

// scalar renders a string, a whole or decimal number or a bool in its plain form.
func scalar(v any, wholeOnly bool) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case json.Number:
		if _, err := strconv.ParseFloat(x.String(), 64); err != nil {
			return "", false
		}
		if _, err := strconv.ParseInt(x.String(), 10, 64); err != nil && wholeOnly {
			return "", false
		}
		return x.String(), true
	case float64:
		if wholeOnly && x != float64(int64(x)) {
			return "", false
		}
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	}
	return "", false
}

// render turns one value into text, checked against the declared type.
func render(name, typ string, v any, wholeOnly bool) (string, error) {
	s, ok := scalar(v, wholeOnly)
	if !ok {
		return "", fmt.Errorf("parameter %s must be a string, a number or a boolean", shown(name))
	}
	_, isStr := v.(string)
	_, isBool := v.(bool)
	switch typ {
	case "integer":
		if _, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err != nil || isBool {
			return "", fmt.Errorf("parameter %s must be an integer", shown(name))
		}
	case "number":
		if _, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err != nil || isBool {
			return "", fmt.Errorf("parameter %s must be a number", shown(name))
		}
	case "boolean":
		if isStr && s != "true" && s != "false" || !isStr && !isBool {
			return "", fmt.Errorf("parameter %s must be true or false", shown(name))
		}
	}
	return s, nil
}

// BuildRequest makes the escaped path, the query and the body of one call from
// the description of the action. params holds the path and query parameters by
// name; body is the JSON body, or nil.
func BuildRequest(a Action, params map[string]any, body json.RawMessage) (path string, query url.Values, out []byte, err error) {
	if why := uncallable(a); why != "" {
		return "", nil, nil, fmt.Errorf("%s cannot be called through this module: %s", a.ID, why)
	}
	declared := map[string]*openapi3.Parameter{} // by in + name
	for _, p := range a.Operation.Parameters {
		declared[p.Value.In+" "+p.Value.Name] = p.Value
	}

	known := map[string]bool{}
	segs := strings.Split(a.Path, "/")
	for i, seg := range segs {
		name, isParam, _ := placeholder(seg)
		if !isParam {
			continue
		}
		known[name] = true
		p := declared["path "+name]
		v, given := params[name]
		if !given {
			return "", nil, nil, fmt.Errorf("the path parameter %s is required", shown(name))
		}
		s, err := render(name, paramType(p), v, true)
		if err != nil {
			return "", nil, nil, err
		}
		if s == "" || s == "." || s == ".." {
			return "", nil, nil, fmt.Errorf("the path parameter %s must be a name, not empty and not a dot segment", shown(name))
		}
		segs[i] = url.PathEscape(s)
	}
	path = strings.Join(segs, "/")

	query = url.Values{}
	for _, p := range a.Operation.Parameters {
		if p.Value.In != "query" {
			continue
		}
		name := p.Value.Name
		known[name] = true
		v, given := params[name]
		if !given || v == nil {
			if p.Value.Required {
				return "", nil, nil, fmt.Errorf("the query parameter %s is required", shown(name))
			}
			continue
		}
		typ := paramType(p.Value)
		list, isList := v.([]any)
		if !isList {
			list = []any{v}
		}
		if typ == "array" {
			typ = ""
			if it := p.Value.Schema.Value.Items; it != nil && it.Value != nil && it.Value.Type != nil && len(it.Value.Type.Slice()) > 0 {
				typ = it.Value.Type.Slice()[0]
			}
		}
		for _, e := range list {
			s, err := render(name, typ, e, false)
			if err != nil {
				return "", nil, nil, err
			}
			query.Add(name, s)
		}
	}

	var unknown []string
	for name := range params {
		if !known[name] {
			unknown = append(unknown, shown(name))
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return "", nil, nil, fmt.Errorf("unknown parameter %s: only the path and query parameters this action describes can be given", strings.Join(unknown, ", "))
	}

	has := len(strings.TrimSpace(string(body))) > 0
	switch rb := a.Operation.RequestBody; {
	case rb == nil && has:
		return "", nil, nil, fmt.Errorf("%s takes no body", a.ID)
	case rb != nil && !has && rb.Value.Required:
		return "", nil, nil, fmt.Errorf("%s requires a JSON body", a.ID)
	case has && !json.Valid(body):
		return "", nil, nil, fmt.Errorf("the body is not valid JSON")
	}
	if has {
		out = body
	}
	return path, query, out, nil
}

// DescribeAction says what a caller needs to know to call the action.
func DescribeAction(a Action) map[string]any {
	d := map[string]any{"action": a.ID, "summary": a.Summary, "class": string(a.Class)}
	if why := uncallable(a); why != "" {
		d["callable"], d["why"] = false, why
	} else {
		d["callable"] = true
	}
	op := a.Operation
	if op == nil {
		return d
	}
	if op.Description != "" {
		d["description"] = op.Description
	}
	params := []map[string]any{}
	for _, in := range []string{"path", "query"} {
		for _, p := range op.Parameters {
			if p == nil || p.Value == nil || p.Value.In != in {
				continue
			}
			typ := paramType(p.Value)
			if typ == "" {
				typ = "any"
			}
			params = append(params, map[string]any{"name": p.Value.Name, "in": in, "required": p.Value.Required, "type": typ, "description": p.Value.Description})
		}
	}
	d["parameters"] = params

	if rb := op.RequestBody; rb != nil && rb.Value != nil {
		body := map[string]any{"required": rb.Value.Required}
		if mt := jsonMediaType(rb.Value); mt != nil {
			body["schema"] = schemaMap(mt.Schema, 0, true, map[*openapi3.Schema]bool{})
		}
		d["body"] = body
		if size(d) > maxDescription {
			body["schema"] = schemaMap(jsonMediaType(rb.Value).Schema, 0, false, map[*openapi3.Schema]bool{})
		}
		if size(d) > maxDescription {
			body["schema"] = map[string]any{"note": "the body schema is too large to show"}
		}
	}
	return d
}

func size(d map[string]any) int {
	b, _ := json.Marshal(d)
	return len(b)
}

// schemaMap is the JSON schema of a body with its references resolved. A
// schema nested deeper than maxDepth, or already being shown above it (a
// cycle), is cut short.
func schemaMap(ref *openapi3.SchemaRef, depth int, desc bool, open map[*openapi3.Schema]bool) map[string]any {
	if ref == nil || ref.Value == nil {
		return map[string]any{}
	}
	s := ref.Value
	m := map[string]any{}
	if s.Type != nil && len(s.Type.Slice()) > 0 {
		m["type"] = s.Type.Slice()[0]
	}
	if depth >= maxDepth || open[s] {
		m["note"] = "nested too deep to show"
		return m
	}
	open[s] = true
	defer delete(open, s)

	if s.Format != "" {
		m["format"] = s.Format
	}
	if desc && s.Description != "" {
		m["description"] = s.Description
	}
	if len(s.Enum) > 0 {
		m["enum"] = s.Enum
	}
	if s.Default != nil {
		m["default"] = s.Default
	}
	if len(s.Required) > 0 {
		m["required"] = s.Required
	}
	if len(s.Properties) > 0 {
		props := map[string]any{}
		for k, v := range s.Properties {
			props[k] = schemaMap(v, depth+1, desc, open)
		}
		m["properties"] = props
	}
	if s.Items != nil {
		m["items"] = schemaMap(s.Items, depth+1, desc, open)
	}
	if s.AdditionalProperties.Schema != nil {
		m["additionalProperties"] = schemaMap(s.AdditionalProperties.Schema, depth+1, desc, open)
	}
	for k, list := range map[string]openapi3.SchemaRefs{"allOf": s.AllOf, "oneOf": s.OneOf, "anyOf": s.AnyOf} {
		if len(list) == 0 {
			continue
		}
		var l []any
		for _, e := range list {
			l = append(l, schemaMap(e, depth+1, desc, open))
		}
		m[k] = l
	}
	return m
}
