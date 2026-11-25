package catalogue_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ReCasaOS/ReCasaOS-MCP/catalogue"
	"github.com/ReCasaOS/ReCasaOS-MCP/service"
)

var params = regexp.MustCompile(`:[A-Za-z_]\w*|\{[^}]*\}`)

// norm makes ":id" and "{anything}" the same, so routes.txt compares to descriptions.
func norm(s string) string { return params.ReplaceAllString(s, "{}") }

// lines reads a data file: one entry per line, "#" comments and blanks skipped.
func lines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// described returns every action of testdata/specs, by id.
func described(t *testing.T) map[string]service.Action {
	t.Helper()
	files, _ := filepath.Glob("../testdata/specs/*.yaml")
	if len(files) != 7 {
		t.Fatalf("%d descriptions in testdata/specs, want 7", len(files))
	}
	out := map[string]service.Action{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		as, err := service.ParseActions(raw)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for _, a := range as {
			if _, dup := out[a.ID]; dup {
				t.Fatalf("%s: %s is described twice", f, a.ID)
			}
			out[a.ID] = a
		}
	}
	return out
}

// embeddedActions returns every action of the embedded descriptions, by id.
func embeddedActions(t *testing.T) map[string]service.Action {
	t.Helper()
	out := map[string]service.Action{}
	for name, raw := range catalogue.Embedded() {
		as, err := service.ParseActions(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, a := range as {
			if _, dup := out[a.ID]; dup {
				t.Fatalf("%s: %s is described twice", name, a.ID)
			}
			out[a.ID] = a
		}
	}
	return out
}

// everyDescribed is the box's descriptions and the embedded ones.
func everyDescribed(t *testing.T) map[string]service.Action {
	t.Helper()
	out := described(t)
	for id, a := range embeddedActions(t) {
		out[id] = a
	}
	return out
}

var ownRoutes = []string{
	"POST /v2/mcp/server", "GET /v2/mcp/status", "GET /v2/mcp/tokens", "POST /v2/mcp/tokens",
	"DELETE /v2/mcp/tokens/{id}", "POST /v1/users/internal/access-token",
}

func TestEveryClassIsKnown(t *testing.T) {
	for id, c := range catalogue.Classes() {
		if !c.Valid() {
			t.Errorf("%s: %q is not a class", id, c)
		}
	}
}

func TestEveryDescribedActionIsClassedOrCounted(t *testing.T) {
	classes := catalogue.Classes()
	var missing []string
	for id := range described(t) {
		if _, ok := classes[id]; !ok {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("%d described actions have no class:\n%s", len(missing), strings.Join(missing, "\n"))
	}
}

func TestNoClassWithoutARoute(t *testing.T) {
	known := map[string]bool{}
	for id := range everyDescribed(t) {
		known[id] = true
	}
	for _, l := range lines(t, "../testdata/routes.txt") {
		known[norm(l)] = true
	}
	for _, l := range ownRoutes {
		known[norm(l)] = true
	}
	for id := range catalogue.Classes() {
		if !known[id] && !known[norm(id)] {
			t.Errorf("%s is neither described, nor a route of the box, nor the module's own: a typo?", id)
		}
	}
}

func TestEveryRouteIsDescribedOrListedUncovered(t *testing.T) {
	desc := map[string]bool{}
	for id := range everyDescribed(t) {
		desc[norm(id)] = true
	}
	routes := map[string]bool{}
	for _, l := range lines(t, "../testdata/routes.txt") {
		routes[norm(l)] = true
	}
	uncovered := map[string]bool{}
	for _, l := range lines(t, "uncovered.txt") {
		uncovered[norm(l)] = true
		if desc[norm(l)] {
			t.Errorf("uncovered.txt lists %s, which is described", l)
		}
		if !routes[norm(l)] {
			t.Errorf("uncovered.txt lists %s, which is not in routes.txt", l)
		}
	}
	for r := range routes {
		if !desc[r] && !uncovered[r] {
			t.Errorf("%s is neither described nor in uncovered.txt", r)
		}
	}
}

func TestOwnRoutesAreNever(t *testing.T) {
	classes := catalogue.Classes()
	for _, id := range ownRoutes {
		if classes[id] != catalogue.Never {
			t.Errorf("%s is %q, want never", id, classes[id])
		}
	}
	for id, c := range classes {
		if strings.Contains(id, " /v2/mcp/") && c != catalogue.Never {
			t.Errorf("%s is %q, want never", id, c)
		}
	}
}

func TestSourcesAreTheSevenDescriptions(t *testing.T) {
	want := []string{
		"/doc/v2/casaos/openapi.yaml", "/doc/v2/app_management/openapi.yaml", "/v1doc/v1/app_management/openapi_v1.yaml",
		"/doc/v2/local_storage/openapi.yaml", "/doc/v2/users/openapi.yaml", "/doc/v2/virt_management/openapi.yaml",
		"/doc/v2/vuln_scan/openapi.yaml",
	}
	if got := catalogue.Sources(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v", got)
	}
}

// Actions known to answer with a secret (or to take one): never plain reads.
func TestKnownSecretsAreNotRead(t *testing.T) {
	known := []string{
		"GET /v1/container/{id}",
		"GET /v1/container/{id}/compose",
		"GET /v2/app_management/backup/destinations/{name}/restic/password",
		"GET /v2/app_management/backup/destinations/{name}/folders",
		"GET /v2/app_management/backup/destinations/{name}/runs",
		"GET /v2/app_management/backup/destinations/{name}/runs/{app}/{stamp}/files",
		"GET /v2/app_management/git/{app}/tags",
		"GET /v2/app_management/appstore",
		"GET /v2/app_management/compose",
		"GET /v2/app_management/compose/{id}",
		"GET /v2/app_management/compose/{id}/containers",
		"GET /v2/app_management/compose/{id}/containers/{containerId}/health",
		"GET /v2/app_management/compose/{id}/env",
		"GET /v2/app_management/compose/{id}/logs",
		"GET /v2/app_management/container/{id}/detail",
		"GET /v2/app_management/container/{id}/files/content",
		"GET /v2/app_management/git/{app}",
		"GET /v2/app_management/global",
		"GET /v2/app_management/global/{key}",
		"GET /v2/app_management/logs",
		"GET /v2/casaos/zt/info",
		"GET /v2/virt_management/images/downloads",
		"PUT /v2/app_management/backup/destinations/{name}/restic",
	}
	classes := catalogue.Classes()
	for _, id := range known {
		c, ok := classes[id]
		if !ok {
			t.Errorf("%s has no class", id)
		} else if c == catalogue.Read {
			t.Errorf("%s answers with a secret, it cannot be read", id)
		}
	}
}

// A deliberate, visible edit is needed to change the class of any described action,
// the box's and the embedded ones alike.
func TestClassCounts(t *testing.T) {
	want := map[catalogue.Class]int{
		catalogue.Read: 59, catalogue.Secret: 22, catalogue.Change: 62, catalogue.Danger: 30, catalogue.Never: 16,
	}
	got := map[catalogue.Class]int{}
	classes := catalogue.Classes()
	for id := range everyDescribed(t) {
		got[classes[id]]++
	}
	for c, n := range want {
		if got[c] != n {
			t.Errorf("%s: %d described actions, want %d", c, got[c], n)
		}
	}
	if len(got) != len(want) {
		t.Errorf("classes in use %v, want %v", got, want)
	}
}

func TestNoActingReadThroughACallerChosenHost(t *testing.T) {
	const id = "GET /v2/app_management/web/frame-check"
	if c := catalogue.Classes()[id]; c != catalogue.Change {
		t.Errorf("%s makes the box call a host the caller chooses: class %q, want change", id, c)
	}
}

func TestAReadThatContactsARemoteIsNotAPlainRead(t *testing.T) {
	want := map[string]catalogue.Class{
		"POST /v2/app_management/backup/destinations/{name}/check":                   catalogue.Change,
		"GET /v2/app_management/backup/destinations/{name}/folders":                  catalogue.Secret,
		"GET /v2/app_management/backup/destinations/{name}/runs":                     catalogue.Secret,
		"GET /v2/app_management/backup/destinations/{name}/runs/{app}/{stamp}/files": catalogue.Secret,
		"GET /v2/app_management/git/{app}/tags":                                      catalogue.Secret,
	}
	classes := catalogue.Classes()
	for id, c := range want {
		if classes[id] != c {
			t.Errorf("%s is %q, want %q", id, classes[id], c)
		}
	}
}
