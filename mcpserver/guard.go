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
	// refused bounds the log lines of requests refused before the token is looked at.
	refused *limiter
	// note writes one line to the module's general log: never more than the address and a count.
	note func(msg, addr string, count int)
	next http.Handler
}

const (
	msgBadToken = "a request with a bad or missing token"
	msgSlowDown = "this address keeps sending bad tokens; it is now told to slow down"
	msgOrigin   = "a request with a foreign Origin was refused"
	msgNotLocal = "a request that did not come through the box's gateway was refused"
	slowDownAt  = maxFailures + 1
)

// noteRefusal logs a refusal once per address and window.
func (g *guard) noteRefusal(msg, addr string) {
	if n, _, _ := g.refused.fail(addr); n == 1 {
		g.note(msg, addr, n)
	}
}

func (g *guard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The module listens on loopback and is reached only through the box's Gateway.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if ip, perr := netip.ParseAddr(host); err != nil || perr != nil || !ip.IsLoopback() {
		g.noteRefusal(msgNotLocal, host)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	source := sourceAddress(r, host)

	if !sameOrigin(r) {
		g.noteRefusal(msgOrigin, source)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Tokens are 256 random bits: limiting lookups protects nothing. The limit only tells a
	// misconfigured client to slow down, and must never lock the owner's valid token out.
	t, ok := g.authenticate(r)
	if !ok {
		n, over, wait := g.limit.fail(source)
		if n == 1 {
			g.note(msgBadToken, source, n)
		}
		if n == slowDownAt {
			g.note(msgSlowDown, source, n)
		}
		if over {
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(wait.Seconds())))))
			http.Error(w, "too many failed attempts: try again later", http.StatusTooManyRequests)
			return
		}
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
