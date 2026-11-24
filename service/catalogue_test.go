package service

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/ReCasaOS/ReCasaOS-MCP/catalogue"
)

type fakeBox map[string]Reply // by description path; a missing path answers 404

func (f fakeBox) DoUpTo(_ context.Context, _ int, _, path string, _ url.Values, _ []byte, _ int) (Reply, error) {
	if r, ok := f[path]; ok {
		return r, nil
	}
	return Reply{Status: 404}, nil
}

func yamlReply(s string) Reply {
	return Reply{Status: 200, ContentType: "application/yaml", Body: []byte(s)}
}

const sourceA = "/doc/v2/app_management/openapi.yaml"
const sourceB = "/doc/v2/vuln_scan/openapi.yaml"

// spec builds a description whose operations are "METHOD|/path|summary|operationId".
func spec(server string, ops ...string) string {
	var b strings.Builder
	b.WriteString("openapi: 3.0.3\ninfo: {title: t, version: '1'}\n")
	if server != "" {
		fmt.Fprintf(&b, "servers:\n  - url: %s\n", server)
	}
	b.WriteString("paths:\n")
	var order []string
	byPath := map[string]string{}
	for _, op := range ops {
		f := strings.Split(op, "|")
		if _, ok := byPath[f[1]]; !ok {
			order = append(order, f[1])
		}
		byPath[f[1]] += fmt.Sprintf("    %s:\n      summary: %q\n      operationId: %s\n      responses: {'200': {description: ok}}\n", strings.ToLower(f[0]), f[2], f[3])
	}
	for _, p := range order {
		fmt.Fprintf(&b, "  %s:\n%s", p, byPath[p])
	}
	return b.String()
}

func newTest(t *testing.T, box fakeBox, classes map[string]catalogue.Class) *Catalogue {
	t.Helper()
	c := newCatalogue(box, classes)
	if err := c.Refresh(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	return c
}

func ids(as []Action) string {
	var s []string
	for _, a := range as {
		s = append(s, a.ID)
	}
	return strings.Join(s, ",")
}

var mixed = fakeBox{sourceA: yamlReply(spec("/v2/app_management",
	"GET|/apps|List the apps|listApps",
	"GET|/secrets|Show the secrets|showSecrets",
	"POST|/apps|Install an app|installApp",
	"DELETE|/apps/{id}|Remove an app|removeApp",
	"GET|/hidden|Internal thing|hiddenThing",
	"GET|/unknown|Nobody classified this|unknownThing"))}

var mixedClasses = map[string]catalogue.Class{
	"GET /v2/app_management/apps":         catalogue.Read,
	"GET /v2/app_management/secrets":      catalogue.Secret,
	"POST /v2/app_management/apps":        catalogue.Change,
	"DELETE /v2/app_management/apps/{id}": catalogue.Danger,
	"GET /v2/app_management/hidden":       catalogue.Never,
}

func TestUnclassedActionIsHidden(t *testing.T) {
	c := newTest(t, mixed, mixedClasses)
	if _, ok := c.Get("GET /v2/app_management/unknown"); ok {
		t.Fatal("Get gave an unclassed action")
	}
	if got := c.Search("unknown", LevelFull, 50); len(got) != 0 {
		t.Fatalf("Search gave %s", ids(got))
	}
	by, un := c.Counts()
	if un != 1 || by[catalogue.Read] != 1 || by[catalogue.Never] != 1 || by[catalogue.Danger] != 1 {
		t.Fatalf("counts %v unclassified %d", by, un)
	}
}

func TestUnknownClassValueIsUnclassed(t *testing.T) {
	c := newTest(t, mixed, map[string]catalogue.Class{"GET /v2/app_management/apps": "reed"})
	if _, ok := c.Get("GET /v2/app_management/apps"); ok {
		t.Fatal("a misspelt class exposed the action")
	}
	if _, un := c.Counts(); un != 6 {
		t.Fatalf("unclassified %d, want 6", un)
	}
}

func TestReadLevelSeesOnlyRead(t *testing.T) {
	c := newTest(t, mixed, mixedClasses)
	for _, lv := range []Level{LevelRead, "", "admin"} {
		if got := ids(c.Search("", lv, 50)); got != "GET /v2/app_management/apps" {
			t.Fatalf("level %q sees %s", lv, got)
		}
	}
}

func TestFullLevelNeverSeesNever(t *testing.T) {
	c := newTest(t, mixed, mixedClasses)
	got := ids(c.Search("", LevelFull, 50))
	want := "DELETE /v2/app_management/apps/{id},GET /v2/app_management/apps,GET /v2/app_management/secrets,POST /v2/app_management/apps"
	if got != want {
		t.Fatalf("got %s", got)
	}
	if _, ok := c.Get("GET /v2/app_management/hidden"); ok {
		t.Fatal("Get gave a never action")
	}
	a, ok := c.Get("POST /v2/app_management/apps")
	if !ok || a.Class != catalogue.Change || a.Method != "POST" || a.Path != "/v2/app_management/apps" ||
		a.OperationID != "installApp" || a.Summary != "Install an app" || a.Operation == nil {
		t.Fatalf("%+v %v", a, ok)
	}
}

func TestSearchFindsByWordsOfSummaryPathAndOperationID(t *testing.T) {
	c := newTest(t, mixed, mixedClasses)
	for q, want := range map[string]string{
		"INSTALL":       "POST /v2/app_management/apps",        // summary, any case
		"secrets":       "GET /v2/app_management/secrets",      // path
		"removeapp":     "DELETE /v2/app_management/apps/{id}", // operationId
		"show nonsense": "GET /v2/app_management/secrets",      // one word is enough
		// two words first, then the one-word matches by id
		"  list   apps ": "GET /v2/app_management/apps,DELETE /v2/app_management/apps/{id},POST /v2/app_management/apps",
	} {
		if got := ids(c.Search(q, LevelFull, 50)); got != want {
			t.Errorf("%q: got %s", q, got)
		}
	}
	if got := c.Search("zzz", LevelFull, 50); len(got) != 0 {
		t.Fatalf("zzz: %s", ids(got))
	}
	// a word found in several fields counts once: "apps" is in the summary, path and operationId of listApps,
	// so "apps listapps" gives listApps 2 and the others 1 (their path holds "apps").
	if got := ids(c.Search("apps listapps", LevelFull, 50)); !strings.HasPrefix(got, "GET /v2/app_management/apps,") {
		t.Fatalf("got %s", got)
	}
	if got := c.Search("", LevelFull, 2); len(got) != 2 {
		t.Fatalf("limit 2 gave %d", len(got))
	}
}

func TestSearchLimits(t *testing.T) {
	var ops []string
	classes := map[string]catalogue.Class{}
	for i := 0; i < 60; i++ {
		ops = append(ops, fmt.Sprintf("GET|/n%02d|thing|op%02d", i, i))
		classes[fmt.Sprintf("GET /v2/x/n%02d", i)] = catalogue.Read
	}
	c := newTest(t, fakeBox{sourceA: yamlReply(spec("/v2/x", ops...))}, classes)
	for limit, want := range map[int]int{0: 10, -3: 10, 5: 5, 50: 50, 500: 50} {
		if got := len(c.Search("thing", LevelRead, limit)); got != want {
			t.Errorf("limit %d: got %d want %d", limit, got, want)
		}
	}
}

func TestRefreshKeepsTheOldCatalogueWhenASourceFails(t *testing.T) {
	cl := map[string]catalogue.Class{"GET /v2/app_management/apps": catalogue.Read, "GET /v2/vuln_scan/status": catalogue.Read}
	box := fakeBox{
		sourceA: yamlReply(spec("/v2/app_management", "GET|/apps|List the apps|listApps")),
		sourceB: yamlReply(spec("/v2/vuln_scan", "GET|/status|Scan state|scanState")),
	}
	c := newTest(t, box, cl)
	fail := map[string]Reply{
		"truncated":   {Status: 200, Body: []byte(spec("/v2/vuln_scan")), Truncated: true},
		"status 500":  {Status: 500, Body: []byte("boom")},
		"not yaml":    yamlReply("{{{ not a description"),
		"no servers":  yamlReply(spec("", "GET|/status|Scan state|scanState")),
		"no document": yamlReply(""),
	}
	for name, bad := range fail {
		box[sourceB] = bad
		box[sourceA] = yamlReply(spec("/v2/app_management", "GET|/apps|List the apps again|listApps"))
		if err := c.Refresh(context.Background(), 1); err == nil {
			t.Errorf("%s: no error", name)
		}
		if _, ok := c.Get("GET /v2/vuln_scan/status"); !ok {
			t.Errorf("%s: the old actions of the failed source are gone", name)
		}
		if a, _ := c.Get("GET /v2/app_management/apps"); a.Summary != "List the apps again" {
			t.Errorf("%s: the good source was not refreshed", name)
		}
	}
}

func TestRefreshDropsASourceThatAnswers404(t *testing.T) {
	cl := map[string]catalogue.Class{"GET /v2/vuln_scan/status": catalogue.Read}
	box := fakeBox{sourceB: yamlReply(spec("/v2/vuln_scan", "GET|/status|Scan state|scanState"))}
	c := newTest(t, box, cl)
	delete(box, sourceB) // module uninstalled
	if err := c.Refresh(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("GET /v2/vuln_scan/status"); ok {
		t.Fatal("an uninstalled module's action is still offered")
	}
}

func TestADuplicateActionIdIsOfferedByNobody(t *testing.T) {
	box := fakeBox{
		sourceA: yamlReply(spec("/v2/x", "GET|/a|first|opA")),
		sourceB: yamlReply(spec("/v2/x/", "GET|/a|second|opB")),
	}
	c := newTest(t, box, map[string]catalogue.Class{"GET /v2/x/a": catalogue.Read})
	if _, ok := c.Get("GET /v2/x/a"); ok {
		t.Fatal("an ambiguous action is offered")
	}
}

func TestAFullURLServerKeepsItsPathOnly(t *testing.T) {
	c := newTest(t, fakeBox{sourceA: yamlReply(spec("http://localhost:8080/v2/x", "GET|/a||opA"))},
		map[string]catalogue.Class{"GET /v2/x/a": catalogue.Read})
	if a, ok := c.Get("GET /v2/x/a"); !ok || a.Summary != "" {
		t.Fatalf("%+v %v", a, ok)
	}
}
