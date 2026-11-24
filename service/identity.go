package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/external"
)

// The accounts service signs tokens for 5 minutes; one is kept 4.
const tokenKept = 4 * time.Minute

var (
	ErrIdentityUnavailable = errors.New("the accounts service cannot give an access token")
	ErrAccountGone         = errors.New("the account does not exist any more")
)

type cachedToken struct {
	value   string
	expires time.Time
}

type flight struct {
	done    chan struct{}
	token   string
	err     error
	waiters int
}

// Identity obtains the short access tokens the box's API accepts, one per account.
// It is the only place that uses the internal secret, and only for the accounts service.
type Identity struct {
	runtimePath string
	client      *http.Client
	now         func() time.Time

	mu      sync.Mutex
	cache   map[int]cachedToken
	flights map[int]*flight
	forgot  map[int]int // bumped by Forget: a token requested before it is not stored
}

func NewIdentity(runtimePath string) *Identity {
	return &Identity{
		runtimePath: runtimePath,
		client:      loopbackClient(),
		now:         time.Now,
		cache:       map[int]cachedToken{},
		flights:     map[int]*flight{},
		forgot:      map[int]int{},
	}
}

// loopbackClient never follows a redirect and ignores proxy settings: what it
// carries is for the address it was given and nowhere else.
func loopbackClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (i *Identity) AccessToken(ctx context.Context, account int) (string, error) {
	i.mu.Lock()
	if c, ok := i.cache[account]; ok && i.now().Before(c.expires) {
		i.mu.Unlock()
		return c.value, nil
	}
	f := i.flights[account]
	if f == nil {
		f = &flight{done: make(chan struct{})}
		i.flights[account] = f
		// Detached from this caller: the others waiting on the flight must not fail with it.
		go i.fetchInto(f, account, i.forgot[account])
	} else {
		f.waiters++
	}
	i.mu.Unlock()

	select {
	case <-f.done:
		return f.token, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// waiters is for tests: how many callers joined the running request of account.
func (i *Identity) waiters(account int) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	if f := i.flights[account]; f != nil {
		return f.waiters
	}
	return 0
}

func (i *Identity) fetchInto(f *flight, account, gen int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	token, err := i.fetch(ctx, account)

	i.mu.Lock()
	defer i.mu.Unlock()
	f.token, f.err = token, err
	if err == nil && i.forgot[account] == gen {
		i.cache[account] = cachedToken{value: token, expires: i.now().Add(tokenKept)}
	}
	delete(i.flights, account)
	close(f.done)
}

func (i *Identity) Forget(account int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.cache, account)
	i.forgot[account]++
}

func (i *Identity) fetch(ctx context.Context, account int) (string, error) {
	unavailable := func(format string, a ...any) (string, error) {
		return "", fmt.Errorf("%w: %s", ErrIdentityUnavailable, fmt.Sprintf(format, a...))
	}
	raw, err := os.ReadFile(filepath.Join(i.runtimePath, external.UserServiceAddressFilename))
	if err != nil {
		return unavailable("%v", err)
	}
	address := strings.TrimSpace(string(raw))
	authorization := external.InternalAuthorization(i.runtimePath)
	if address == "" || authorization == "" {
		return unavailable("no address or no internal secret")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(address, "/")+"/v1/users/internal/access-token",
		bytes.NewReader([]byte(`{"id":`+strconv.Itoa(account)+`}`)))
	if err != nil {
		return unavailable("%v", err)
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-Type", "application/json")
	resp, err := i.client.Do(req)
	if err != nil {
		return unavailable("%v", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", ErrAccountGone
	default:
		return unavailable("status %d", resp.StatusCode)
	}
	var out struct {
		Data struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return unavailable("unreadable reply")
	}
	if out.Data.AccessToken == "" {
		return unavailable("empty token")
	}
	return out.Data.AccessToken, nil
}
