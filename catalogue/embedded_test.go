package catalogue_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/ReCasaOS/ReCasaOS-MCP/catalogue"
	"github.com/ReCasaOS/ReCasaOS-MCP/service"
)

func TestEmbeddedDescriptionsParse(t *testing.T) {
	files := catalogue.Embedded()
	if len(files) == 0 {
		t.Fatal("no embedded description")
	}
	for name, raw := range files {
		as, err := service.ParseActions(raw)
		if err != nil || len(as) == 0 {
			t.Errorf("%s: %d actions, %v", name, len(as), err)
		}
	}
}

func TestEveryEmbeddedActionIsClassed(t *testing.T) {
	classes := catalogue.Classes()
	var missing []string
	for id := range embeddedActions(t) {
		if !classes[id].Valid() {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("%d embedded actions have no class:\n%s", len(missing), strings.Join(missing, "\n"))
	}
}

func TestEmbeddedActionIsARoute(t *testing.T) {
	routes := map[string]bool{}
	for _, l := range lines(t, "../testdata/routes.txt") {
		routes[norm(l)] = true
	}
	notYet := map[string]bool{}
	for _, l := range lines(t, "v1/not-in-routes.txt") {
		notYet[norm(l)] = true
		if routes[norm(l)] {
			t.Errorf("not-in-routes.txt lists %s, which routes.txt knows", l)
		}
	}
	for id := range embeddedActions(t) {
		if !routes[norm(id)] && !notYet[norm(id)] {
			t.Errorf("%s is in neither routes.txt nor v1/not-in-routes.txt", id)
		}
	}
}

func TestEmbeddedNeverDuplicatesABoxDescription(t *testing.T) {
	box := described(t)
	for id := range embeddedActions(t) {
		if _, dup := box[id]; dup {
			t.Errorf("%s is described by the box too: it would be offered by nobody", id)
		}
	}
}

func TestUncoveredLosesWhatIsDescribed(t *testing.T) {
	emb := map[string]bool{}
	for id := range embeddedActions(t) {
		emb[norm(id)] = true
	}
	for _, l := range lines(t, "uncovered.txt") {
		if emb[norm(l)] {
			t.Errorf("uncovered.txt lists %s, which is embedded", l)
		}
	}
}
