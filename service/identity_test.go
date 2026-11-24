package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/external"
)

const testSecret = "s3cret-of-this-boot"

// runtimeFor writes a runtime path that points at address; "" writes no address file.
func runtimeFor(t *testing.T, address string) string {
	t.Helper()
	dir := t.TempDir()
	if address != "" {
		if err := os.WriteFile(filepath.Join(dir, external.UserServiceAddressFilename), []byte(address+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, external.InternalSecretFilename), []byte(testSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeAccounts answers like the accounts service; each call hands out a new token.
func fakeAccounts(t *testing.T, calls *atomic.Int32, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.URL.Path != "/v1/users/internal/access-token" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Internal "+testSecret {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		var in struct{ ID int }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ID != 7 {
			t.Errorf("body id = %d (%v)", in.ID, err)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": 200, "message": "ok", "data": map[string]any{
			"access_token": "tok-" + string(rune('0'+n)), "expires_at": time.Now().Add(5 * time.Minute).Unix()}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAccessTokenIsAskedOncePerFourMinutes(t *testing.T) {
	var calls atomic.Int32
	srv := fakeAccounts(t, &calls, 200)
	id := NewIdentity(runtimeFor(t, srv.URL))
	now := time.Unix(1_000_000, 0)
	id.now = func() time.Time { return now }

	a, err := id.AccessToken(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := id.AccessToken(context.Background(), 7)
	if a != b || calls.Load() != 1 {
		t.Fatalf("tokens %q %q, calls %d", a, b, calls.Load())
	}
	now = now.Add(4 * time.Minute)
	c, _ := id.AccessToken(context.Background(), 7)
	if c == a || calls.Load() != 2 {
		t.Fatalf("token %q after 4 min, calls %d", c, calls.Load())
	}
}

func TestAccessTokenAsksWithTheInternalSecret(t *testing.T) {
	var calls atomic.Int32
	srv := fakeAccounts(t, &calls, 200) // the handler checks the header and the body
	if _, err := NewIdentity(runtimeFor(t, srv.URL)).AccessToken(context.Background(), 7); err != nil || calls.Load() != 1 {
		t.Fatalf("%v, calls %d", err, calls.Load())
	}
}

func TestIdentityDownIsAnError(t *testing.T) {
	var calls atomic.Int32
	srv := fakeAccounts(t, &calls, 200)
	dir := runtimeFor(t, srv.URL)
	srv.Close() // refused connection
	if _, err := NewIdentity(dir).AccessToken(context.Background(), 7); !errors.Is(err, ErrIdentityUnavailable) {
		t.Fatalf("closed: %v", err)
	}
	if _, err := NewIdentity(runtimeFor(t, "")).AccessToken(context.Background(), 7); !errors.Is(err, ErrIdentityUnavailable) {
		t.Fatalf("no address file: %v", err)
	}
	noSecret := runtimeFor(t, "http://127.0.0.1:1")
	os.Remove(filepath.Join(noSecret, external.InternalSecretFilename))
	if _, err := NewIdentity(noSecret).AccessToken(context.Background(), 7); !errors.Is(err, ErrIdentityUnavailable) {
		t.Fatalf("no secret: %v", err)
	}
	for status, want := range map[int]error{404: ErrAccountGone, 403: ErrIdentityUnavailable, 400: ErrIdentityUnavailable, 500: ErrIdentityUnavailable} {
		s := fakeAccounts(t, &calls, status)
		if _, err := NewIdentity(runtimeFor(t, s.URL)).AccessToken(context.Background(), 7); !errors.Is(err, want) {
			t.Errorf("status %d: %v, want %v", status, err, want)
		}
	}
}

func TestIdentityBadBodiesAreUnavailable(t *testing.T) {
	for _, body := range []string{"not json", `{"data":{"access_token":""}}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		_, err := NewIdentity(runtimeFor(t, srv.URL)).AccessToken(context.Background(), 7)
		srv.Close()
		if !errors.Is(err, ErrIdentityUnavailable) {
			t.Errorf("%q: %v", body, err)
		}
	}
}

func TestForgetDropsTheCachedToken(t *testing.T) {
	var calls atomic.Int32
	srv := fakeAccounts(t, &calls, 200)
	id := NewIdentity(runtimeFor(t, srv.URL))
	a, _ := id.AccessToken(context.Background(), 7)
	id.Forget(7)
	b, _ := id.AccessToken(context.Background(), 7)
	if a == b || calls.Load() != 2 {
		t.Fatalf("%q %q calls %d", a, b, calls.Load())
	}
}

// A caller that joined a request started before Forget gets that token, but it is not kept.
func TestConcurrentCallersShareOneRequestAndForgetWins(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		_, _ = w.Write([]byte(`{"data":{"access_token":"tok","expires_at":1}}`))
	}))
	defer srv.Close()
	id := NewIdentity(runtimeFor(t, srv.URL))

	var wg sync.WaitGroup
	got := make([]string, 2)
	wg.Add(1)
	go func() { defer wg.Done(); got[0], _ = id.AccessToken(context.Background(), 7) }()
	<-started
	wg.Add(1)
	go func() { defer wg.Done(); got[1], _ = id.AccessToken(context.Background(), 7) }()
	for id.waiters(7) < 1 {
		time.Sleep(time.Millisecond)
	}
	id.Forget(7)
	close(release)
	wg.Wait()
	if got[0] != "tok" || got[1] != "tok" || calls.Load() != 1 {
		t.Fatalf("%v calls %d", got, calls.Load())
	}
	if _, err := id.AccessToken(context.Background(), 7); err != nil || calls.Load() != 2 {
		t.Fatalf("%v calls %d: a token from before Forget was kept", err, calls.Load())
	}
}
