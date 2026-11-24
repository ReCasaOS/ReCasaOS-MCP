package main

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/ReCasaOS-MCP/mcpserver"
	"github.com/ReCasaOS/ReCasaOS-MCP/service"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// Claude Code 2.1.330 keeps a subscriptions/listen POST open for as long as it is connected.
func TestShutdownEndsWithAnOpenListen(t *testing.T) {
	logger.LogInitConsoleOnly()
	tokens, err := service.OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := tokens.Create("laptop", 7, service.LevelFull)
	if err != nil {
		t.Fatal(err)
	}
	journal := service.NewJournal(filepath.Join(t.TempDir(), "mcp-calls.log"))
	defer journal.Close()
	h := mcpserver.Handler(mcpserver.Deps{Tokens: tokens, Catalogue: service.NewCatalogue(nil), Journal: journal})

	var open atomic.Int32
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		open.Add(1)
		defer open.Add(-1)
		h.ServeHTTP(w, r)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httpServer(ctx, counted)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()

	// a client that wants tools/list_changed opens the listen
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ClientOptions{ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {}})
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: "http://" + ln.Addr().String(), HTTPClient: &http.Client{Transport: bearer(plain)}, MaxRetries: -1, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	for deadline := time.Now().Add(5 * time.Second); open.Load() != 1; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d requests open, want the listen alone", open.Load())
		}
	}

	cancel() // what SIGTERM does
	shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	start := time.Now()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v after %v", err, time.Since(start))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("shutdown took %v", d)
	}
}
