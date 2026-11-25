package routeauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Token audiences the reconciler projects into the team gateway (PRD §11.1).
const (
	AzureAudience = "api://AzureADTokenExchange"
	AzureScope    = "https://cognitiveservices.azure.com/.default"
	GoogleScope   = "https://www.googleapis.com/auth/cloud-platform"
)

// AzureExchanger exchanges the projected token for an Entra access token (client credentials with a
// federated client assertion).
type AzureExchanger struct {
	TokenFile string
	Authority string // default https://login.microsoftonline.com
	HTTP      *http.Client
}

// Exchange implements Exchanger.
func (x *AzureExchanger) Exchange(ctx context.Context, f Federation) (string, time.Time, error) {
	if f.TenantID == "" || f.ClientID == "" {
		return "", time.Time{}, errors.New("azure federation: tenant and client ids are required")
	}
	assertion, err := readToken(x.TokenFile)
	if err != nil {
		return "", time.Time{}, err
	}
	authority := x.Authority
	if authority == "" {
		authority = "https://login.microsoftonline.com"
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {f.ClientID}, "scope": {AzureScope},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"}, "client_assertion": {assertion}}
	u := strings.TrimSuffix(authority, "/") + "/" + url.PathEscape(f.TenantID) + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doToken(x.HTTP, req, "azure")
}

// GoogleExchanger exchanges the projected token at Google STS for a federated access token.
type GoogleExchanger struct {
	TokenFile string
	STSURL    string // default https://sts.googleapis.com/v1/token
	HTTP      *http.Client
}

// GoogleAudience is the STS audience of a Google federation: its workload identity provider.
func GoogleAudience(f Federation) string {
	return fmt.Sprintf("//iam.googleapis.com/projects/%s/locations/global/workloadIdentityPools/%s/providers/%s", f.ProjectNumber, f.PoolID, f.ProviderID)
}

// Exchange implements Exchanger.
func (x *GoogleExchanger) Exchange(ctx context.Context, f Federation) (string, time.Time, error) {
	if f.ProjectNumber == "" || f.PoolID == "" || f.ProviderID == "" {
		return "", time.Time{}, errors.New("google federation: project number, pool and provider are required")
	}
	subject, err := readToken(x.TokenFile)
	if err != nil {
		return "", time.Time{}, err
	}
	sts := x.STSURL
	if sts == "" {
		sts = "https://sts.googleapis.com/v1/token"
	}
	body, _ := json.Marshal(map[string]string{"grantType": "urn:ietf:params:oauth:grant-type:token-exchange", "audience": GoogleAudience(f),
		"scope": GoogleScope, "requestedTokenType": "urn:ietf:params:oauth:token-type:access_token",
		"subjectToken": subject, "subjectTokenType": "urn:ietf:params:oauth:token-type:jwt"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sts, strings.NewReader(string(body)))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	return doToken(x.HTTP, req, "google")
}

func readToken(path string) (string, error) {
	if path == "" {
		return "", errors.New("federation: no projected token configured")
	}
	b, err := os.ReadFile(path) //nolint:gosec // a configured mount path
	if err != nil {
		return "", errors.New("federation: the projected token cannot be read")
	}
	return strings.TrimSpace(string(b)), nil
}

func doToken(hc *http.Client, req *http.Request, kind string) (string, time.Time, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	start := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%s token exchange: %w", kind, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(b, &t)
	if resp.StatusCode/100 != 2 || t.AccessToken == "" {
		// The provider's error code only: its description may echo the assertion.
		return "", time.Time{}, fmt.Errorf("%s token exchange: status %d %s", kind, resp.StatusCode, t.Error)
	}
	if t.ExpiresIn <= 0 {
		t.ExpiresIn = 3600
	}
	return t.AccessToken, start.Add(time.Duration(t.ExpiresIn) * time.Second), nil
}
