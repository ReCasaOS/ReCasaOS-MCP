package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const maxReply = 256 << 10

var ErrBinaryReply = errors.New("the box answered with something that is not text or JSON")

type Reply struct {
	Status      int
	ContentType string
	Body        []byte
	Truncated   bool
}

// Box calls the box's own API through the Gateway, as the account of the token.
// It carries the account's access token and nothing else: the box's rights
// system judges every call.
type Box struct {
	gatewayINI string
	identity   *Identity
	client     *http.Client
	timeout    time.Duration // for the whole call, body included

	mu   sync.Mutex
	base *url.URL // nil: to be read from the ini
}

// runtimePath is kept in the signature for the caller's symmetry with NewIdentity; Box reads nothing from it.
func NewBox(runtimePath, gatewayINI string, identity *Identity) *Box {
	return &Box{gatewayINI: gatewayINI, identity: identity, client: loopbackClient(), timeout: 60 * time.Second}
}

func (b *Box) baseURL(reread bool) (*url.URL, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.base == nil || reread {
		u, err := gatewayAddress(b.gatewayINI)
		if err != nil {
			b.base = nil
			return nil, err
		}
		b.base = u
	}
	return b.base, nil
}

// Do sends one request; path is the escaped path and starts with "/".
func (b *Box) Do(ctx context.Context, account int, method, path string, query url.Values, body []byte) (Reply, error) {
	return b.DoUpTo(ctx, account, method, path, query, body, maxReply)
}

// DoUpTo is Do with its own limit on the reply's size.
func (b *Box) DoUpTo(ctx context.Context, account int, method, path string, query url.Values, body []byte, limit int) (Reply, error) {
	if !strings.HasPrefix(path, "/") {
		return Reply{}, errors.New("the path must start with /")
	}
	plain, err := url.PathUnescape(path)
	if err != nil {
		return Reply{}, fmt.Errorf("bad path: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	rep, err := b.once(ctx, account, method, path, plain, query, body, limit)
	if err == nil && rep.Status == http.StatusUnauthorized {
		b.identity.Forget(account)
		return b.once(ctx, account, method, path, plain, query, body, limit)
	}
	return rep, err
}

// once gets a token, sends the request, and reads the gateway's address again
// when it could not connect (the owner may have changed the port).
func (b *Box) once(ctx context.Context, account int, method, path, plain string, query url.Values, body []byte, limit int) (Reply, error) {
	token, err := b.identity.AccessToken(ctx, account)
	if err != nil {
		return Reply{}, err
	}
	base, err := b.baseURL(false)
	if err != nil {
		return Reply{}, err
	}
	rep, err := b.send(ctx, base, token, method, path, plain, query, body, limit)
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		if base, err = b.baseURL(true); err != nil {
			return Reply{}, err
		}
		rep, err = b.send(ctx, base, token, method, path, plain, query, body, limit)
	}
	return rep, err
}

func (b *Box) send(ctx context.Context, base *url.URL, token, method, path, plain string, query url.Values, body []byte, limit int) (Reply, error) {
	u := *base
	u.Path, u.RawPath, u.RawQuery = plain, path, query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return Reply{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return Reply{}, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return Reply{}, err
	}
	rep := Reply{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: data}
	if len(data) > limit {
		rep.Body, rep.Truncated = data[:limit], true
	}
	if len(rep.Body) > 0 && isBinary(rep.ContentType, rep.Body) {
		return Reply{}, ErrBinaryReply
	}
	return rep, nil
}

func isBinary(contentType string, body []byte) bool {
	if contentType == "" {
		// shortcut: a body cut in the middle of a character counts as binary; rare without a type.
		return !utf8.Valid(body)
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return true
	}
	return !(mt == "application/json" || strings.HasSuffix(mt, "+json") || strings.HasPrefix(mt, "text/"))
}
