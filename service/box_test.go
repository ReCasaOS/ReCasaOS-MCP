package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// boxFor wires a Box to a fake accounts service and the given fake Gateway.
func boxFor(t *testing.T, gateway http.Handler) *Box {
	t.Helper()
	var calls atomic.Int32
	accounts := fakeAccounts(t, &calls, 200)
	gw := httptest.NewServer(gateway)
	t.Cleanup(gw.Close)
	return newBoxAt(t, accounts.URL, gw)
}

func newBoxAt(t *testing.T, accountsURL string, gw *httptest.Server) *Box {
	t.Helper()
	ini := writeINI(t, "[gateway]\nport="+mustPort(gw.URL)+"\n")
	rt := runtimeFor(t, accountsURL)
	return NewBox(rt, ini, NewIdentity(rt))
}

func mustPort(raw string) string { u, _ := url.Parse(raw); return u.Port() }

func TestBoxNeverSendsTheInternalSecret(t *testing.T) {
	var seen atomic.Int32
	b := boxFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer tok-") || strings.Contains(auth, "Internal") || strings.Contains(auth, testSecret) {
			t.Errorf("Authorization = %q", auth)
		}
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	rep, err := b.Do(context.Background(), 7, "POST", "/v2/x", url.Values{"a": {"b c"}}, []byte(`{}`))
	if err != nil || rep.Status != 200 || string(rep.Body) != `{"ok":true}` || seen.Load() != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestBoxWithoutIdentitySendsNothing(t *testing.T) {
	var calls, hits atomic.Int32
	accounts := fakeAccounts(t, &calls, 404)
	gw := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer gw.Close()
	b := newBoxAt(t, accounts.URL, gw)
	if _, err := b.Do(context.Background(), 7, "GET", "/v2/x", nil, nil); !errors.Is(err, ErrAccountGone) || hits.Load() != 0 {
		t.Fatalf("%v, gateway hits %d", err, hits.Load())
	}
}

func TestBoxCutsALongReply(t *testing.T) {
	b := boxFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.Repeat("a", 5<<20)))
	}))
	rep, err := b.Do(context.Background(), 7, "GET", "/v2/x", nil, nil)
	if err != nil || len(rep.Body) != 256<<10 || !rep.Truncated {
		t.Fatalf("len %d truncated %v err %v", len(rep.Body), rep.Truncated, err)
	}
}

func TestBoxRefusesABinaryReply(t *testing.T) {
	for ct, binary := range map[string]bool{"application/octet-stream": true, "image/png": true, "application/vnd.api+json": false, "text/plain; charset=utf-8": false, "application/json": false} {
		b := boxFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			_, _ = w.Write([]byte("hello"))
		}))
		_, err := b.Do(context.Background(), 7, "GET", "/v2/x", nil, nil)
		if errors.Is(err, ErrBinaryReply) != binary {
			t.Errorf("%s: %v", ct, err)
		}
	}
	// No Content-Type: text is fine, bytes that are not UTF-8 are binary.
	for body, binary := range map[string]bool{"plain": false, "\xff\xfe\x00": true} {
		b := boxFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header()["Content-Type"] = nil
			_, _ = w.Write([]byte(body))
		}))
		if _, err := b.Do(context.Background(), 7, "GET", "/v2/x", nil, nil); errors.Is(err, ErrBinaryReply) != binary {
			t.Errorf("no content type %q: %v", body, err)
		}
	}
}

func TestBoxEmptyReplyIsFine(t *testing.T) {
	b := boxFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(204)
	}))
	rep, err := b.Do(context.Background(), 7, "DELETE", "/v2/x", nil, nil)
	if err != nil || rep.Status != 204 || len(rep.Body) != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestBoxRetriesOnceOn401(t *testing.T) {
	var gwCalls atomic.Int32
	var tokens, bodies []string
	b := boxFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("Authorization"))
		buf := make([]byte, 16)
		n, _ := r.Body.Read(buf)
		bodies = append(bodies, string(buf[:n]))
		if gwCalls.Add(1) == 1 {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	rep, err := b.Do(context.Background(), 7, "POST", "/v2/x", nil, []byte(`{"a":1}`))
	if err != nil || rep.Status != 200 || len(tokens) != 2 || tokens[0] == tokens[1] || bodies[1] != `{"a":1}` {
		t.Fatalf("%+v %v tokens %v bodies %v", rep, err, tokens, bodies)
	}
}

func TestBoxReturnsASecond401(t *testing.T) {
	var gwCalls atomic.Int32
	b := boxFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gwCalls.Add(1); w.WriteHeader(401) }))
	rep, err := b.Do(context.Background(), 7, "GET", "/v2/x", nil, nil)
	if err != nil || rep.Status != 401 || gwCalls.Load() != 2 {
		t.Fatalf("%+v %v calls %d", rep, err, gwCalls.Load())
	}
}

func TestBoxStopsAtSixtySeconds(t *testing.T) {
	b := boxFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	b.timeout = 50 * time.Millisecond
	if _, err := b.Do(context.Background(), 7, "GET", "/v2/x", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
	if NewBox("", "", nil).timeout != 60*time.Second {
		t.Fatal("default timeout is not 60 s")
	}
}

func TestBoxDoesNotFollowRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	b := boxFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	rep, err := b.Do(context.Background(), 7, "GET", "/v2/x", nil, nil)
	if err != nil || rep.Status != 302 || elsewhere.Load() != 0 {
		t.Fatalf("%+v %v elsewhere %d", rep, err, elsewhere.Load())
	}
}

func TestBoxPathKeepsItsEscapes(t *testing.T) {
	var got, query string
	b := boxFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, query = r.URL.EscapedPath(), r.URL.RawQuery
	}))
	if _, err := b.Do(context.Background(), 7, "GET", "/v2/a%2Fb/c%20d", url.Values{"q": {"x y"}}, nil); err != nil {
		t.Fatal(err)
	}
	if got != "/v2/a%2Fb/c%20d" || query != "q=x+y" {
		t.Fatalf("path %q query %q", got, query)
	}
	if _, err := b.Do(context.Background(), 7, "GET", "v2/x", nil, nil); err == nil {
		t.Error("a path without a leading slash was accepted")
	}
	if _, err := b.Do(context.Background(), 7, "GET", "/v2/%zz", nil, nil); err == nil {
		t.Error("a bad escape was accepted")
	}
}

func TestBoxRereadsTheGatewayWhenItCannotConnect(t *testing.T) {
	var hits atomic.Int32
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer live.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	deadPort := mustPort(dead.URL)
	dead.Close()

	var calls atomic.Int32
	accounts := fakeAccounts(t, &calls, 200)
	ini := writeINI(t, "[gateway]\nport="+deadPort+"\n")
	rt := runtimeFor(t, accounts.URL)
	b := NewBox(rt, ini, NewIdentity(rt))
	if _, err := b.Do(context.Background(), 7, "GET", "/x", nil, nil); err == nil {
		t.Fatal("dead gateway answered")
	}
	// The owner changed the port in the dashboard.
	if err := os.WriteFile(ini, []byte("[gateway]\nport="+mustPort(live.URL)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rep, err := b.Do(context.Background(), 7, "GET", "/x", nil, nil); err != nil || rep.Status != 200 || hits.Load() != 1 {
		t.Fatalf("%+v %v hits %d", rep, err, hits.Load())
	}
}

func TestBoxDoUpToUsesItsOwnLimit(t *testing.T) {
	b := boxFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.Repeat("a", 300<<10)))
	}))
	rep, err := b.DoUpTo(context.Background(), 7, "GET", "/v2/x", nil, nil, 1<<20)
	if err != nil || len(rep.Body) != 300<<10 || rep.Truncated {
		t.Fatalf("1 MiB limit: len %d truncated %v err %v", len(rep.Body), rep.Truncated, err)
	}
	rep, err = b.DoUpTo(context.Background(), 7, "GET", "/v2/x", nil, nil, 100)
	if err != nil || len(rep.Body) != 100 || !rep.Truncated {
		t.Fatalf("100 byte limit: len %d truncated %v err %v", len(rep.Body), rep.Truncated, err)
	}
}

func TestBoxReadsYAMLAsText(t *testing.T) {
	const body = "openapi: 3.0.3\ninfo: {title: t, version: '1'}\n"
	for _, ct := range []string{"application/yaml", "application/x-yaml", "Application/YAML; charset=utf-8", "text/yaml"} {
		b := boxFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			_, _ = w.Write([]byte(body))
		}))
		rep, err := b.DoUpTo(context.Background(), 7, "GET", "/doc/x.yaml", nil, nil, 1<<20)
		if err != nil || string(rep.Body) != body {
			t.Errorf("%s: body %q err %v", ct, rep.Body, err)
		}
	}
}

func TestBoxStillRefusesOctetStreamYAML(t *testing.T) {
	b := boxFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("openapi: 3.0.3\n"))
	}))
	if _, err := b.DoUpTo(context.Background(), 7, "GET", "/doc/x.yaml", nil, nil, 1<<20); !errors.Is(err, ErrBinaryReply) {
		t.Fatalf("err %v", err)
	}
}

func TestBoxReadsYAMLServedWithoutAContentType(t *testing.T) {
	const body = "openapi: 3.0.3\ninfo: {title: t, version: '1'}\n"
	b := boxFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = nil // no header at all: Go's server sniffs
		_, _ = w.Write([]byte(body))
	}))
	rep, err := b.DoUpTo(context.Background(), 7, "GET", "/doc/x.yaml", nil, nil, 1<<20)
	if err != nil || string(rep.Body) != body {
		t.Fatalf("body %q err %v", rep.Body, err)
	}
}
