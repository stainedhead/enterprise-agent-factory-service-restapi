// Package routeup is the upstream HTTP client of a team gateway's model routes (gateway contract §12):
// one client per endpoint, with the endpoint's CA bundle and timeout, no redirects (a redirect would
// carry the route credential elsewhere) and no proxy from the environment for in-namespace endpoints.
package routeup

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/gatewayhttp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// DefaultTimeout is the timeout of an endpoint that sets none.
const DefaultTimeout = 300 * time.Second

// Client implements gatewayhttp.RouteUpstream.
type Client struct {
	mu      sync.Mutex
	clients map[string]cached
	// Base is the transport cloned per endpoint; nil is http.DefaultTransport.
	Base *http.Transport
}

type cached struct {
	key string
	hc  *http.Client
}

// New returns a Client.
func New() *Client { return &Client{clients: map[string]cached{}} }

var _ gatewayhttp.RouteUpstream = (*Client)(nil)

func (c *Client) client(ep gateway.RouteEndpoint) (*http.Client, error) {
	timeout := ep.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	key := ep.CABundle + "|" + timeout.String() + "|" + string(ep.Location)
	c.mu.Lock()
	defer c.mu.Unlock()
	if x, ok := c.clients[ep.ID]; ok && x.key == key {
		return x.hc, nil
	}
	base := c.Base
	if base == nil {
		base = http.DefaultTransport.(*http.Transport)
	}
	tr := base.Clone()
	tr.ResponseHeaderTimeout = timeout
	if ep.Location == domain.LocationInNamespace {
		tr.Proxy = nil // in-namespace servers are reached directly, never through an egress proxy
	}
	if ep.CABundle != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(ep.CABundle)) {
			return nil, errors.New("routeup: the endpoint's CA bundle holds no certificate")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	hc := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	c.clients[ep.ID] = cached{key: key, hc: hc}
	return hc, nil
}

// Do sends req; a timeout is gatewayhttp.ErrUpstreamTimeout.
func (c *Client) Do(ctx context.Context, ep gateway.RouteEndpoint, req *http.Request) (*http.Response, error) {
	hc, err := c.client(ep)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req.WithContext(ctx)) //nolint:gosec // the upstream is the admitted route's base URL, set by an admin and limited by egress policy
	if err != nil {
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			return nil, errors.Join(gatewayhttp.ErrUpstreamTimeout, err)
		}
		return nil, err
	}
	return resp, nil
}
