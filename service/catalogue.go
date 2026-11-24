package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/ReCasaOS/ReCasaOS-MCP/catalogue"
	"github.com/getkin/kin-openapi/openapi3"
)

// descriptionLimit is how much of one OpenAPI description Refresh reads.
const descriptionLimit = 4 << 20

// Action is one operation of the box's API. Class is empty when nobody
// classified it; such an action is counted but never offered.
type Action struct {
	ID           string // "GET /v2/app_management/compose"
	Method, Path string // full path, with {param}
	OperationID  string
	Summary      string
	Class        catalogue.Class
	Operation    *openapi3.Operation
}

// fetcher is what the catalogue needs of a Box.
type fetcher interface {
	DoUpTo(ctx context.Context, account int, method, path string, query url.Values, body []byte, limit int) (Reply, error)
}

// state is one complete catalogue; Refresh builds a new one and swaps it in.
type state struct {
	bySource     map[string][]Action // every described action of each source, classed or not
	exposed      []Action            // classed and not never, by id
	byID         map[string]Action   // same actions as exposed
	counts       map[catalogue.Class]int
	unclassified int
}

// Catalogue is the list of actions an AI client may search. It is safe for
// concurrent use.
type Catalogue struct {
	src     fetcher
	classes map[string]catalogue.Class

	mu  sync.RWMutex
	cur *state
}

func NewCatalogue(box *Box) *Catalogue { return newCatalogue(box, catalogue.Classes()) }

// NewCatalogueWithClasses is NewCatalogue with classes of the caller's own, for tests of
// other packages.
func NewCatalogueWithClasses(box *Box, classes map[string]catalogue.Class) *Catalogue {
	return newCatalogue(box, classes)
}

func newCatalogue(src fetcher, classes map[string]catalogue.Class) *Catalogue {
	return &Catalogue{src: src, classes: classes, cur: build(nil, nil)}
}

// ParseActions lists the operations of one OpenAPI description. The full path
// is the first server's path plus the operation's path. It reads nothing
// outside the document and does not validate it beyond what listing needs.
func ParseActions(data []byte) ([]Action, error) {
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = false
	doc, err := loader.LoadFromData(data)
	if err != nil {
		return nil, err
	}
	// Without a server path the full path is unknown: refuse the whole description rather than guess.
	if len(doc.Servers) == 0 {
		return nil, errors.New("the description names no server")
	}
	u, err := url.Parse(doc.Servers[0].URL)
	if err != nil {
		return nil, fmt.Errorf("bad server url: %w", err)
	}
	base := strings.TrimRight(u.Path, "/")

	var out []Action
	for p, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			full := base + p
			id := method + " " + full
			merged, err := mergeParameters(id, full, item.Parameters, op.Parameters)
			if err != nil {
				return nil, err
			}
			cp := *op // the document's operation is shared: never write to it
			cp.Parameters = merged
			out = append(out, Action{ID: id, Method: method, Path: full, OperationID: op.OperationID, Summary: op.Summary, Operation: &cp})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the description holds no operation")
	}
	return out, nil
}

// mergeParameters gives an operation the parameters of its path item first,
// then its own; an own parameter with the same name and place replaces the
// path item's in its position. It refuses an unresolved parameter and a path
// placeholder that no path parameter declares: such an action cannot be called.
func mergeParameters(id, path string, shared, own openapi3.Parameters) (openapi3.Parameters, error) {
	var out openapi3.Parameters
	for _, list := range []openapi3.Parameters{shared, own} {
		for _, p := range list {
			if p == nil || p.Value == nil {
				return nil, fmt.Errorf("%s: a parameter is not resolved", id)
			}
			replaced := false
			for i, q := range out {
				if q.Value.Name == p.Value.Name && q.Value.In == p.Value.In {
					out[i], replaced = p, true
					break
				}
			}
			if !replaced {
				out = append(out, p)
			}
		}
	}
	for _, seg := range strings.Split(path, "/") {
		if !strings.HasPrefix(seg, "{") || !strings.HasSuffix(seg, "}") {
			continue
		}
		name := seg[1 : len(seg)-1]
		if strings.ContainsAny(name, "{}") {
			continue // a segment mixing text and placeholders: BuildRequest refuses the action
		}
		found := false
		for _, p := range out {
			found = found || (p.Value.In == "path" && p.Value.Name == name)
		}
		if !found {
			return nil, fmt.Errorf("%s: the path parameter %q is not declared", id, name)
		}
	}
	return out, nil
}

// Refresh reads every source and replaces the catalogue. A source that fails
// (error, status other than 200, cut reply, unreadable description) keeps its
// previous actions and is reported in the error; a source that answers 404 is
// a module that is not installed and is simply absent.
func (c *Catalogue) Refresh(ctx context.Context, account int) error {
	c.mu.RLock()
	prev := c.cur.bySource
	c.mu.RUnlock()

	next := map[string][]Action{}
	var errs []error
	for _, src := range catalogue.Sources() {
		as, err := c.read(ctx, account, src)
		switch {
		case err == nil:
			next[src] = as
		case errors.Is(err, errNotInstalled):
		default:
			errs = append(errs, fmt.Errorf("%s: %w", src, err))
			if old, ok := prev[src]; ok {
				next[src] = old
			}
		}
	}
	st := build(next, c.classes)
	c.mu.Lock()
	c.cur = st
	c.mu.Unlock()
	return errors.Join(errs...)
}

var errNotInstalled = errors.New("not installed")

func (c *Catalogue) read(ctx context.Context, account int, src string) ([]Action, error) {
	rep, err := c.src.DoUpTo(ctx, account, http.MethodGet, src, nil, nil, descriptionLimit)
	switch {
	case err != nil:
		return nil, err
	case rep.Status == http.StatusNotFound:
		return nil, errNotInstalled
	case rep.Status != http.StatusOK:
		return nil, fmt.Errorf("status %d", rep.Status)
	case rep.Truncated:
		return nil, fmt.Errorf("the description is longer than %d bytes", descriptionLimit)
	}
	return ParseActions(rep.Body)
}

// build joins the descriptions with the classes. Only an action with a known
// class other than never is exposed; an id that two sources both describe is
// ambiguous and left out altogether.
func build(bySource map[string][]Action, classes map[string]catalogue.Class) *state {
	st := &state{bySource: bySource, byID: map[string]Action{}, counts: map[catalogue.Class]int{}}
	seen := map[string]int{}
	for _, as := range bySource {
		for _, a := range as {
			seen[a.ID]++
		}
	}
	for _, as := range bySource {
		for _, a := range as {
			if seen[a.ID] > 1 {
				continue
			}
			if cl := classes[a.ID]; cl.Valid() {
				a.Class = cl
			}
			switch a.Class {
			case "":
				st.unclassified++
			case catalogue.Never:
				st.counts[catalogue.Never]++
			default:
				st.counts[a.Class]++
				st.byID[a.ID] = a
				st.exposed = append(st.exposed, a)
			}
		}
	}
	sort.Slice(st.exposed, func(i, j int) bool { return st.exposed[i].ID < st.exposed[j].ID })
	return st
}

func (c *Catalogue) state() *state {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cur
}

// Get returns an offered action whatever the token's level: the caller applies it.
func (c *Catalogue) Get(id string) (Action, bool) {
	a, ok := c.state().byID[id]
	return a, ok
}

// Search lists the offered actions the level may see, best match first. The
// score of an action is the number of query words found in its summary, full
// path or operationId; an empty query lists by id. limit: 10 when not
// positive, at most 50. Any level other than full counts as read.
func (c *Catalogue) Search(query string, level Level, limit int) []Action {
	if limit <= 0 {
		limit = 10
	}
	limit = min(limit, 50)
	words := strings.Fields(strings.ToLower(query))

	type hit struct {
		a     Action
		score int
	}
	var hits []hit
	for _, a := range c.state().exposed {
		if level != LevelFull && a.Class != catalogue.Read {
			continue
		}
		score := 0
		hay := strings.ToLower(a.Summary + "\n" + a.Path + "\n" + a.OperationID)
		for _, w := range words {
			if strings.Contains(hay, w) {
				score++
			}
		}
		if score > 0 || len(words) == 0 {
			hits = append(hits, hit{a, score})
		}
	}
	// exposed is by id, and the sort is stable: ties stay by id
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	out := make([]Action, 0, min(limit, len(hits)))
	for i := 0; i < len(hits) && i < limit; i++ {
		out = append(out, hits[i].a)
	}
	return out
}

// Counts gives the offered actions and the never ones by class, and the number
// of actions the box serves that have no class.
func (c *Catalogue) Counts() (byClass map[catalogue.Class]int, unclassified int) {
	st := c.state()
	byClass = make(map[catalogue.Class]int, len(st.counts))
	for k, v := range st.counts {
		byClass[k] = v
	}
	return byClass, st.unclassified
}
