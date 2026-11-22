// Runtime path of the Bedrock adapters (gateway contract §1, M3-A5): afs-gateway forwards a call to the
// team's application inference profile, signed with credentials from the team's Bedrock role, over TLS
// to the Bedrock Runtime endpoint (the VPC endpoint in production).
//
// The request body goes through unchanged and the response comes back unchanged: the agent talks the
// real wire format, so the adapter signs and forwards bytes instead of translating through SDK types.
// Whatever the agent signed or sent in headers never reaches this package.
package bedrock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/smithy-go/encoding/httpbinding"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Operation is a Bedrock Runtime operation path segment.
type Operation string

// The operations the gateway forwards.
const (
	OpConverse                 Operation = "converse"
	OpConverseStream           Operation = "converse-stream"
	OpInvoke                   Operation = "invoke"
	OpInvokeWithResponseStream Operation = "invoke-with-response-stream"
)

// MaxResponseBytes bounds a buffered (non-stream) response.
const MaxResponseBytes = 64 << 20

// CredentialSource yields credentials for a team's Bedrock role (aws/sts.Assumer).
type CredentialSource interface {
	Credentials(ctx context.Context, roleARN string) (awssdk.Credentials, error)
}

// RuntimeConfig configures a Runtime.
type RuntimeConfig struct {
	// Endpoint is the base URL of Bedrock Runtime, https only: AFS_BEDROCK_VPCE_URL.
	Endpoint    string
	Region      string
	Credentials CredentialSource
	// HTTPClient replaces the default client; tests pass a TLS test server's.
	HTTPClient *http.Client
}

// Runtime forwards signed calls to Bedrock Runtime.
type Runtime struct {
	base   *url.URL
	region string
	creds  CredentialSource
	hc     *http.Client
	signer *v4.Signer
	now    func() time.Time
}

// ErrInsecureEndpoint is returned for an endpoint that is not https.
var ErrInsecureEndpoint = errors.New("bedrock: the Bedrock endpoint must use https")

// NewRuntime returns a Runtime. A plain-http endpoint is refused: nothing between the gateway and
// Bedrock may be unencrypted.
func NewRuntime(cfg RuntimeConfig) (*Runtime, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bedrock: invalid endpoint %q", cfg.Endpoint)
	}
	if u.Scheme != "https" {
		return nil, ErrInsecureEndpoint
	}
	if cfg.Region == "" {
		return nil, errors.New("bedrock: a region is required")
	}
	if cfg.Credentials == nil {
		return nil, errors.New("bedrock: credentials are required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			MaxIdleConns:        512,
			MaxIdleConnsPerHost: 256,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		}}
	}
	// A redirect would carry the signature elsewhere.
	c := *hc
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Runtime{base: u, region: cfg.Region, creds: cfg.Credentials, hc: &c, signer: v4.NewSigner(), now: time.Now}, nil
}

// Call is one forwarded request.
type Call struct {
	Op Operation
	// ProfileARN is the team's inference profile; it replaces the AFS model id the agent used.
	ProfileARN string
	// RoleARN is the team's Bedrock role, assumed for the signature.
	RoleARN string
	Body    []byte
}

// Response is a complete upstream answer.
type Response struct {
	Header http.Header
	Body   []byte
}

// Stream is an open upstream event stream; the caller closes Body.
type Stream struct {
	Header http.Header
	Body   io.ReadCloser
}

func (r *Runtime) request(ctx context.Context, c Call) (*http.Request, error) {
	if c.ProfileARN == "" {
		return nil, domain.Internal(errors.New("bedrock: no inference profile for the call"))
	}
	creds, err := r.creds.Credentials(ctx, c.RoleARN)
	if err != nil {
		// Reservation released by the caller; the agent sees 503 (gateway contract §7).
		return nil, &domain.Error{Kind: domain.KindUnavailable, Message: "the team's Bedrock role could not be assumed", Cause: err}
	}
	u := *r.base
	esc := "/model/" + httpbinding.EscapePath(c.ProfileARN, true) + "/" + string(c.Op)
	u.Path = "/model/" + c.ProfileARN + "/" + string(c.Op)
	u.RawPath = esc
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(c.Body))
	if err != nil {
		return nil, domain.Internal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Op == OpConverseStream || c.Op == OpInvokeWithResponseStream {
		req.Header.Set("Accept", "application/vnd.amazon.eventstream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	sum := sha256.Sum256(c.Body)
	if err := r.signer.SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), "bedrock", r.region, r.now()); err != nil {
		return nil, domain.Internal(fmt.Errorf("bedrock: sign: %w", err))
	}
	return req, nil
}

// Do forwards a non-stream call and returns the whole response. A non-2xx answer is returned as a
// domain error (Throttled, Upstream, Validation); a failure to assume the role is Unavailable.
func (r *Runtime) Do(ctx context.Context, c Call) (*Response, error) {
	req, err := r.request(ctx, c)
	if err != nil {
		return nil, err
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return nil, statusError(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
	if err != nil {
		return nil, transportError(ctx, err)
	}
	return &Response{Header: resp.Header, Body: body}, nil
}

// Open forwards a stream call and returns once the response headers arrive. An error before the
// first event is returned here, so the agent gets a JSON error (gateway contract §7).
func (r *Runtime) Open(ctx context.Context, c Call) (*Stream, error) {
	req, err := r.request(ctx, c)
	if err != nil {
		return nil, err
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if resp.StatusCode/100 != 2 {
		defer func() { _ = resp.Body.Close() }()
		return nil, statusError(resp)
	}
	return &Stream{Header: resp.Header, Body: resp.Body}, nil
}

func transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return &domain.Error{Kind: domain.KindUpstream, Message: "the model provider could not be reached", Cause: err}
}

// statusError maps a non-2xx Bedrock answer to the gateway's error (gateway contract §7): throttling
// passes through with Retry-After, a request Bedrock refuses is a ValidationError with Bedrock's
// message, and everything else, including 5xx and permission errors, is an UpstreamError that does not
// expose the provider's text.
func statusError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(b, &body)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		after := 1
		if n, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && n > 0 {
			after = n
		}
		return &domain.Error{Kind: domain.KindThrottled, Message: "the model provider is throttling requests", RetryAfterSeconds: after}
	case resp.StatusCode == http.StatusBadRequest:
		msg := body.Message
		if msg == "" {
			msg = "the model provider refused the request"
		}
		return &domain.Error{Kind: domain.KindValidation, Message: msg, Fields: []domain.FieldError{{Field: "body", Reason: msg}}}
	}
	return &domain.Error{Kind: domain.KindUpstream, Message: "the model provider returned an error",
		Cause: fmt.Errorf("bedrock: status %d (%s)", resp.StatusCode, resp.Header.Get("X-Amzn-Errortype"))}
}
