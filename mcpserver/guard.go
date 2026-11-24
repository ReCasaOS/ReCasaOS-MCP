package mcpserver

import (
	"context"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/ReCasaOS/ReCasaOS-MCP/service"
)

const maxBody = 1 << 20

type tokenKey struct{}

func tokenFrom(ctx context.Context) (service.Token, bool) {
	t, ok := ctx.Value(tokenKey{}).(service.Token)
	return t, ok
}

// guard is the only way in: it lets through a request from the Gateway that carries a known
// token, and counts the failures of each source address.
type guard struct {
	tokens *service.Store
	limit  *limiter
	next   http.Handler
}

func (g *guard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The module listens on loopback and is reached only through the box's Gateway.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if ip, perr := netip.ParseAddr(host); err != nil || perr != nil || !ip.IsLoopback() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	source := sourceAddress(r, host)

	if !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if blocked, wait := g.limit.blocked(source); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(wait.Seconds())))))
		http.Error(w, "too many failed attempts: try again later", http.StatusTooManyRequests)
		return
	}

	t, ok := g.authenticate(r)
	if !ok {
		g.limit.fail(source)
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "a valid token is needed", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	g.next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenKey{}, t)))
}

// sourceAddress is the last X-Forwarded-For element, the one the Gateway's proxy appends
// itself; the elements before it are whatever the client sent.
func sourceAddress(r *http.Request, remoteHost string) string {
	all := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	if ip, err := netip.ParseAddr(strings.TrimSpace(all[len(all)-1])); err == nil {
		return ip.Unmap().String()
	}
	return remoteHost
}

// sameOrigin passes a request without Origin, as the AI client sends none; a browser's
// request must come from the box's own page.
func sameOrigin(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) > 1 {
		return false
	}
	u, err := url.Parse(origins[0])
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

func (g *guard) authenticate(r *http.Request) (service.Token, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return service.Token{}, false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t") {
		return service.Token{}, false
	}
	return g.tokens.Lookup(token)
}
