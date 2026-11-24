package service

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func loadActions(t *testing.T, file string) []Action {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", "specs", file))
	if err != nil {
		t.Fatal(err)
	}
	as, err := ParseActions(data)
	if err != nil {
		t.Fatal(err)
	}
	return as
}

func pick(t *testing.T, as []Action, id string) Action {
	t.Helper()
	for _, a := range as {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no action %q", id)
	return Action{}
}

// inline parses a description with one path whose operations are ops.
func inline(t *testing.T, path, ops string) []Action {
	t.Helper()
	as, err := ParseActions([]byte("openapi: 3.0.3\ninfo: {title: t, version: '1'}\nservers: [{url: /v2/x}]\npaths:\n  " + path + ":\n" + ops))
	if err != nil {
		t.Fatal(err)
	}
	return as
}

const getCompose = "GET /v2/app_management/compose/{id}"

func TestPathParamIsOneEscapedSegment(t *testing.T) {
	a := pick(t, loadActions(t, "app_management.yaml"), getCompose)
	for _, v := range []string{"a/b", "../../v1/users", "x?y=1", "%2F", "a b", "a#b", "a\\b", "a;b"} {
		path, query, _, err := BuildRequest(a, map[string]any{"id": v}, nil)
		if err != nil {
			t.Errorf("%q: %v", v, err)
			continue
		}
		if got, want := strings.Count(path, "/"), strings.Count(a.Path, "/"); got != want {
			t.Errorf("%q: %d slashes in %q, want %d", v, got, path, want)
		}
		last := path[strings.LastIndex(path, "/")+1:]
		if back, err := url.PathUnescape(last); err != nil || back != v {
			t.Errorf("%q: last segment %q unescapes to %q (%v)", v, last, back, err)
		}
		if len(query) != 0 {
			t.Errorf("%q: query %v", v, query)
		}
		// as Box.Do sets it: the host and the number of segments cannot move
		un, err := url.PathUnescape(path)
		if err != nil {
			t.Fatal(err)
		}
		u := url.URL{Scheme: "http", Host: "127.0.0.1:1", Path: un, RawPath: path}
		re, err := url.Parse(u.String())
		if err != nil {
			t.Fatal(err)
		}
		if re.Host != "127.0.0.1:1" || re.RawQuery != "" || re.Fragment != "" {
			t.Errorf("%q: re-parsed %s", v, re)
		}
		if got, want := strings.Count(re.EscapedPath(), "/"), strings.Count(a.Path, "/"); got != want {
			t.Errorf("%q: re-parsed %q has %d slashes, want %d", v, re.EscapedPath(), got, want)
		}
	}
	if _, _, _, err := BuildRequest(a, map[string]any{"id": ""}, nil); err == nil || !strings.Contains(err.Error(), `"id"`) {
		t.Errorf("empty value: %v", err)
	}
}

func TestDotSegmentsAreRefused(t *testing.T) {
	a := pick(t, loadActions(t, "app_management.yaml"), getCompose)
	for _, v := range []string{".", ".."} {
		if _, _, _, err := BuildRequest(a, map[string]any{"id": v}, nil); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
	if _, _, _, err := BuildRequest(a, map[string]any{"id": "..."}, nil); err != nil {
		t.Errorf("three dots are a plain name: %v", err)
	}
}

func TestPathParamTypes(t *testing.T) {
	a := pick(t, loadActions(t, "app_management.yaml"), getCompose)
	for v, want := range map[any]string{float64(42): "42", json.Number("7"): "7", true: "true", "x": "x"} {
		path, _, _, err := BuildRequest(a, map[string]any{"id": v}, nil)
		if err != nil || !strings.HasSuffix(path, "/"+want) {
			t.Errorf("%v: %q %v", v, path, err)
		}
	}
	for _, v := range []any{map[string]any{"a": 1}, []any{"a"}, nil, float64(1.5), json.Number("1.5")} {
		if _, _, _, err := BuildRequest(a, map[string]any{"id": v}, nil); err == nil || !strings.Contains(err.Error(), `"id"`) {
			t.Errorf("%#v: %v", v, err)
		}
	}
}

func TestUnknownParamIsRefused(t *testing.T) {
	a := pick(t, loadActions(t, "app_management.yaml"), getCompose)
	_, _, _, err := BuildRequest(a, map[string]any{"id": "x", "sneaky": "1"}, nil)
	if err == nil || !strings.Contains(err.Error(), `"sneaky"`) {
		t.Fatalf("got %v", err)
	}
}

func TestMissingRequiredParamIsRefused(t *testing.T) {
	a := pick(t, loadActions(t, "app_management.yaml"), getCompose)
	_, _, _, err := BuildRequest(a, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `"id"`) {
		t.Fatalf("got %v", err)
	}
}

const queryOps = `    get:
      operationId: q
      parameters:
        - {name: id, in: query, required: true, schema: {type: string}}
        - {name: n, in: query, schema: {type: integer}}
        - {name: on, in: query, schema: {type: boolean}}
        - {name: tags, in: query, schema: {type: array, items: {type: string}}}
        - {name: any, in: query}
      responses: {'200': {description: ok}}
`

func TestQueryParamsAreEncoded(t *testing.T) {
	a := inline(t, "/q", queryOps)[0]
	_, q, _, err := BuildRequest(a, map[string]any{"id": "a&b=c", "any": "x y"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 2 || q.Get("id") != "a&b=c" || q.Get("any") != "x y" {
		t.Errorf("query %v", q)
	}
	if got := q.Encode(); strings.Count(got, "&") != 1 {
		t.Errorf("encoded %q", got)
	}
	_, q, _, err = BuildRequest(a, map[string]any{"id": "x", "n": float64(3), "on": "true"}, nil)
	if err != nil || q.Get("n") != "3" || q.Get("on") != "true" {
		t.Errorf("%v %v", q, err)
	}
	for _, bad := range []map[string]any{{"id": "x", "n": "abc"}, {"id": "x", "n": true}, {"id": "x", "on": "yes"}, {"id": "x", "on": float64(1)}, {"id": "x", "any": map[string]any{}}} {
		if _, _, _, err := BuildRequest(a, bad, nil); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestArrayQueryParamRepeatsTheKey(t *testing.T) {
	a := inline(t, "/q", queryOps)[0]
	_, q, _, err := BuildRequest(a, map[string]any{"id": "x", "tags": []any{"b", "a", float64(2)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(q["tags"], ","); got != "b,a,2" {
		t.Errorf("tags %q", got)
	}
	if _, _, _, err := BuildRequest(a, map[string]any{"id": "x", "tags": []any{[]any{"a"}}}, nil); err == nil {
		t.Error("nested array accepted")
	}
}

const bodyOps = `    post:
      operationId: b
      requestBody:
        required: true
        content:
          application/json: {schema: {type: object}}
      responses: {'200': {description: ok}}
    put:
      operationId: c
      requestBody:
        content:
          application/vnd.api+json: {schema: {type: object}}
      responses: {'200': {description: ok}}
    get:
      operationId: d
      responses: {'200': {description: ok}}
`

func TestBodyMustBeJSON(t *testing.T) {
	as := inline(t, "/b", bodyOps)
	post, put := pick(t, as, "POST /v2/x/b"), pick(t, as, "PUT /v2/x/b")
	if _, _, out, err := BuildRequest(post, nil, json.RawMessage(` {"a": 1} `)); err != nil || string(out) != ` {"a": 1} ` {
		t.Errorf("%q %v", out, err)
	}
	if _, _, _, err := BuildRequest(post, nil, json.RawMessage(`{"a": `)); err == nil {
		t.Error("broken JSON accepted")
	}
	if _, _, _, err := BuildRequest(post, nil, nil); err == nil {
		t.Error("required body missing accepted")
	}
	if _, _, out, err := BuildRequest(put, nil, nil); err != nil || len(out) != 0 {
		t.Errorf("optional body: %q %v", out, err)
	}
	if _, _, _, err := BuildRequest(put, nil, json.RawMessage(`[1]`)); err != nil {
		t.Errorf("+json type: %v", err)
	}
}

func TestBodyRefusedWhenTheActionTakesNone(t *testing.T) {
	get := pick(t, inline(t, "/b", bodyOps), "GET /v2/x/b")
	if _, _, _, err := BuildRequest(get, nil, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "body") {
		t.Fatalf("got %v", err)
	}
}

func TestRequiredHeaderParamMakesTheActionUncallable(t *testing.T) {
	as := inline(t, "/h", `    get:
      operationId: h
      parameters:
        - {name: X-Need, in: header, required: true, schema: {type: string}}
      responses: {'200': {description: ok}}
    post:
      operationId: o
      parameters:
        - {name: X-Opt, in: header, schema: {type: string}}
        - {name: sess, in: cookie, schema: {type: string}}
      responses: {'200': {description: ok}}
`)
	get, post := pick(t, as, "GET /v2/x/h"), pick(t, as, "POST /v2/x/h")
	if _, _, _, err := BuildRequest(get, nil, nil); err == nil || !strings.Contains(err.Error(), "cannot be called") {
		t.Errorf("got %v", err)
	}
	if d := DescribeAction(get); d["callable"] != false || d["why"] == nil {
		t.Errorf("describe %v", d)
	}
	if _, _, _, err := BuildRequest(post, nil, nil); err != nil {
		t.Errorf("optional header/cookie: %v", err)
	}
	if _, _, _, err := BuildRequest(post, map[string]any{"X-Opt": "1"}, nil); err == nil || !strings.Contains(err.Error(), `"X-Opt"`) {
		t.Errorf("supplying a header: %v", err)
	}
	if d := DescribeAction(post); d["callable"] != true {
		t.Errorf("describe %v", d)
	}
}

func TestNonJSONBodyMakesTheActionUncallable(t *testing.T) {
	a := inline(t, "/m", `    post:
      operationId: m
      requestBody:
        content:
          multipart/form-data: {schema: {type: object}}
      responses: {'200': {description: ok}}
`)[0]
	if _, _, _, err := BuildRequest(a, nil, nil); err == nil || !strings.Contains(err.Error(), "cannot be called") {
		t.Errorf("got %v", err)
	}
	if d := DescribeAction(a); d["callable"] != false {
		t.Errorf("describe %v", d)
	}
}

func TestAMixedSegmentIsRefused(t *testing.T) {
	a := inline(t, "/f/{a}.{b}", `    get:
      operationId: x
      parameters:
        - {name: a, in: path, required: true, schema: {type: string}}
        - {name: b, in: path, required: true, schema: {type: string}}
      responses: {'200': {description: ok}}
`)[0]
	_, _, _, err := BuildRequest(a, map[string]any{"a": "1", "b": "2"}, nil)
	if err == nil || !strings.Contains(err.Error(), a.ID) {
		t.Errorf("got %v", err)
	}
	if d := DescribeAction(a); d["callable"] != false {
		t.Errorf("describe %v", d)
	}
}

func TestDescribeNamesEveryParam(t *testing.T) {
	a := pick(t, loadActions(t, "virt_management.yaml"), "DELETE /v2/virt_management/vms/{id}/snapshots/{snapshot}")
	d := DescribeAction(a)
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Action     string
		Parameters []struct {
			Name, In string
			Required bool
		}
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Action != a.ID || len(back.Parameters) < 2 {
		t.Fatalf("%s", raw)
	}
	if p := back.Parameters; p[0].In != "path" || p[0].Name != "id" || !p[0].Required || p[1].Name != "snapshot" {
		t.Errorf("%s", raw)
	}
	if _, has := d["body"]; has {
		t.Error("a DELETE without body has a body")
	}
	q := DescribeAction(inline(t, "/q", queryOps)[0])
	ps := q["parameters"].([]map[string]any)
	if len(ps) != 5 || ps[0]["name"] != "id" || ps[1]["type"] != "integer" || ps[3]["type"] != "array" {
		t.Errorf("%v", ps)
	}
}

func TestDescribeResolvesReferencesAndStops(t *testing.T) {
	as, err := ParseActions([]byte(`openapi: 3.0.3
info: {title: t, version: '1'}
servers: [{url: /v2/x}]
components:
  schemas:
    Node:
      type: object
      properties:
        name: {type: string}
        next: {$ref: '#/components/schemas/Node'}
        kids: {type: array, items: {$ref: '#/components/schemas/Node'}}
paths:
  /n:
    post:
      operationId: n
      requestBody:
        required: true
        content:
          application/json: {schema: {$ref: '#/components/schemas/Node'}}
      responses: {'200': {description: ok}}
`))
	if err != nil {
		t.Fatal(err)
	}
	d := DescribeAction(as[0])
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"name"`) || !strings.Contains(string(raw), "nested too deep") {
		t.Errorf("%s", raw)
	}
	if body := d["body"].(map[string]any); body["required"] != true {
		t.Errorf("%v", body)
	}
}

func TestEveryRealActionCanBeDescribed(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("..", "testdata", "specs", "*.yaml"))
	var total, uncallable, biggest int
	var biggestID string
	for _, f := range files {
		for _, a := range loadActions(t, filepath.Base(f)) {
			total++
			d := DescribeAction(a)
			raw, err := json.Marshal(d)
			if err != nil {
				t.Errorf("%s: %v", a.ID, err)
				continue
			}
			if len(raw) > 33<<10 {
				t.Errorf("%s: %d bytes", a.ID, len(raw))
			}
			if len(raw) > biggest {
				biggest, biggestID = len(raw), a.ID
			}
			if d["callable"] == false {
				uncallable++
				t.Logf("uncallable: %s: %v", a.ID, d["why"])
			}
			// with no parameters: a result or a clean error, never a panic
			if _, _, _, err := BuildRequest(a, nil, nil); err != nil && err.Error() == "" {
				t.Errorf("%s: empty error", a.ID)
			}
		}
	}
	t.Logf("%d actions, %d uncallable, largest description %d bytes (%s)", total, uncallable, biggest, biggestID)
	if total == 0 {
		t.Fatal("no action")
	}
}

func describedSize(t *testing.T, a Action) (map[string]any, int) {
	t.Helper()
	d := DescribeAction(a)
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return d, len(raw)
}

func TestALargeActionWithoutJSONBodyIsDescribed(t *testing.T) {
	a := inline(t, "/big", "    post:\n      operationId: big\n      description: "+strings.Repeat("x", 40<<10)+"\n"+`      requestBody:
        content:
          multipart/form-data: {schema: {type: object}}
      responses: {'200': {description: ok}}
`)[0]
	d, n := describedSize(t, a)
	if n > 32<<10 || d["callable"] != false {
		t.Errorf("%d bytes, callable %v", n, d["callable"])
	}
}

func TestALargeActionWithJSONBodyStaysUnderTheBound(t *testing.T) {
	a := inline(t, "/big", "    post:\n      operationId: big\n      description: "+strings.Repeat("x", 40<<10)+"\n"+`      requestBody:
        content:
          application/json: {schema: {type: object}}
      responses: {'200': {description: ok}}
`)[0]
	d, n := describedSize(t, a)
	if n > 32<<10 || d["callable"] != true || d["action"] != a.ID {
		t.Errorf("%d bytes, %v", n, d)
	}
}

func TestManyLargeParametersStayUnderTheBound(t *testing.T) {
	var b strings.Builder
	b.WriteString("    get:\n      operationId: many\n      parameters:\n")
	for i := 0; i < 200; i++ {
		b.WriteString("        - {name: p" + strconv.Itoa(i) + ", in: query, description: " + strings.Repeat("y", 1<<10) + ", schema: {type: string}}\n")
	}
	b.WriteString("      responses: {'200': {description: ok}}\n")
	a := inline(t, "/many", b.String())[0]
	d, n := describedSize(t, a)
	if n > 32<<10 || d["action"] != a.ID || d["note"] == nil {
		t.Errorf("%d bytes, note %v", n, d["note"])
	}
}

func TestNilOperationDoesNotPanic(t *testing.T) {
	a := Action{ID: "GET /x", Method: "GET", Path: "/x"}
	if _, _, _, err := BuildRequest(a, nil, nil); err == nil {
		t.Error("want an error")
	}
	if d := DescribeAction(a); d["callable"] != false {
		t.Errorf("%v", d)
	}
}
