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
	t.Skip("the classification of the described actions is Task 9; this test comes back on there")
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
	for id := range described(t) {
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
	for id := range described(t) {
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
