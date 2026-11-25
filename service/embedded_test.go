package service

import (
	"context"
	"strings"
	"testing"

	"github.com/ReCasaOS/ReCasaOS-MCP/catalogue"
)

var embeddedClasses = map[string]catalogue.Class{
	"GET /v1/sys/utilization":   catalogue.Read,
	"PUT /v1/sys/state/{state}": catalogue.Danger,
}

func embeddedFiles() map[string][]byte {
	return map[string][]byte{"v1/sys.yaml": []byte(spec("/v1/sys",
		"GET|/utilization|Show the load|utilization",
		"PUT|/state/{state}|Shut down|putState",
		"GET|/unlisted|Nobody classified this|unlisted"))}
}

func TestEmbeddedActionsAreThereBeforeAnyRefresh(t *testing.T) {
	c := newCatalogueWith(fakeBox{}, embeddedClasses, embeddedFiles())
	if len(c.EmbeddedErrors()) != 0 || c.HasBoxActions() {
		t.Fatalf("%v %v", c.EmbeddedErrors(), c.HasBoxActions())
	}
	if got := ids(c.Search("", LevelFull, 50)); got != "GET /v1/sys/utilization,PUT /v1/sys/state/{state}" {
		t.Fatalf("got %s", got)
	}
	if got := ids(c.Search("", LevelRead, 50)); got != "GET /v1/sys/utilization" {
		t.Fatalf("a read level sees %s", got)
	}
	if _, un := c.Counts(); un != 1 {
		t.Fatalf("unclassified %d, want 1", un)
	}
	if _, ok := c.Get("GET /v1/sys/unlisted"); ok {
		t.Fatal("an unclassed embedded action is offered")
	}
}

func TestAFailedRefreshKeepsEmbeddedActions(t *testing.T) {
	c := newCatalogueWith(fakeBox{sourceA: {Status: 500}}, embeddedClasses, embeddedFiles())
	if err := c.Refresh(context.Background(), 1); err == nil {
		t.Fatal("no error")
	}
	if _, ok := c.Get("GET /v1/sys/utilization"); !ok {
		t.Fatal("the embedded action is gone")
	}
	if c.HasBoxActions() {
		t.Fatal("a failed source counts as loaded")
	}
}

func TestAnEmbeddedActionTheBoxAlsoDescribesIsOfferedByNobody(t *testing.T) {
	box := fakeBox{sourceA: yamlReply(spec("/v1/sys", "GET|/utilization|The box's own|boxUtilization"))}
	c := newCatalogueWith(box, embeddedClasses, embeddedFiles())
	if err := c.Refresh(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("GET /v1/sys/utilization"); ok {
		t.Fatal("an ambiguous action is offered")
	}
	if _, ok := c.Get("PUT /v1/sys/state/{state}"); !ok {
		t.Fatal("the other embedded action went with it")
	}
}

func TestAnEmbeddedNeverActionIsNotOffered(t *testing.T) {
	c := newCatalogueWith(fakeBox{}, map[string]catalogue.Class{"GET /v1/sys/utilization": catalogue.Never}, embeddedFiles())
	if _, ok := c.Get("GET /v1/sys/utilization"); ok {
		t.Fatal("a never action is offered")
	}
}

func TestAnInvalidEmbeddedFileIsRefusedWhole(t *testing.T) {
	files := embeddedFiles()
	// one good operation and one whose placeholder has no parameter
	files["v1/broken.yaml"] = []byte(spec("/v1/x", "GET|/ok|fine|okOp") +
		"  /a/{id}:\n    get:\n      operationId: noParam\n      responses: {'200': {description: ok}}\n")
	files["v1/garbage.yaml"] = []byte("{{{ not a description")
	cl := map[string]catalogue.Class{"GET /v1/x/ok": catalogue.Read, "GET /v1/sys/utilization": catalogue.Read, "GET /v2/app_management/apps": catalogue.Read}
	box := fakeBox{sourceA: yamlReply(spec("/v2/app_management", "GET|/apps|List the apps|listApps"))}
	c := newCatalogueWith(box, cl, files)
	if err := c.Refresh(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	errs := c.EmbeddedErrors()
	if len(errs) != 2 || !strings.Contains(errs[0].Error(), "v1/broken.yaml") || !strings.Contains(errs[1].Error(), "v1/garbage.yaml") {
		t.Fatalf("errors %v", errs)
	}
	if _, ok := c.Get("GET /v1/x/ok"); ok {
		t.Fatal("an action of a refused file is offered")
	}
	for _, id := range []string{"GET /v1/sys/utilization", "GET /v2/app_management/apps"} {
		if _, ok := c.Get(id); !ok {
			t.Errorf("%s is gone", id)
		}
	}
}
