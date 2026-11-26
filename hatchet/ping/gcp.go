package ping

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrInvalidGCPKey replaces every problem with the GCP_PING_KEY setting. The value is a private
// key, so errors never repeat any part of it (Hatchet stores returned errors).
var ErrInvalidGCPKey = errors.New("invalid GCP_PING_KEY (want a base64-encoded service-account JSON key)")

const (
	defaultTokenURI = "https://oauth2.googleapis.com/token"
	computeScope    = "https://www.googleapis.com/auth/compute"
	computeBase     = "https://compute.googleapis.com/compute/v1/projects/"
	maxAPIBody      = 8 << 20
)

// gcpKey is the part of a service-account key file the worker uses.
type gcpKey struct {
	ClientEmail string
	ProjectID   string
	TokenURI    string
	private     *rsa.PrivateKey
}

// parseGCPKey reads the base64 (standard or URL-safe, with or without padding) of a service-account
// JSON key. Every failure is ErrInvalidGCPKey.
func parseGCPKey(b64 string) (*gcpKey, error) {
	b64 = strings.Join(strings.Fields(b64), "")
	var raw []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw, err = enc.DecodeString(b64); err == nil {
			break
		}
	}
	if err != nil || len(raw) == 0 {
		return nil, ErrInvalidGCPKey
	}
	var doc struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
		ProjectID   string `json:"project_id"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.ClientEmail == "" {
		return nil, ErrInvalidGCPKey
	}
	block, _ := pem.Decode([]byte(doc.PrivateKey))
	if block == nil {
		return nil, ErrInvalidGCPKey
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, ErrInvalidGCPKey
	}
	priv, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, ErrInvalidGCPKey
	}
	if doc.TokenURI == "" {
		doc.TokenURI = defaultTokenURI
	}
	if u, err := url.Parse(doc.TokenURI); err != nil || u.Scheme != "https" || u.Hostname() != "oauth2.googleapis.com" || u.User != nil {
		return nil, ErrInvalidGCPKey
	}
	return &gcpKey{ClientEmail: doc.ClientEmail, ProjectID: doc.ProjectID, TokenURI: doc.TokenURI, private: priv}, nil
}

// newHTTPClient is the client for Google's fixed hosts; every request also has its own deadline.
func newHTTPClient() *http.Client { return &http.Client{Timeout: 2 * time.Minute} }

// httpStatusError is an HTTP answer that was not a success. Its text holds the status code only:
// a response body can echo the request.
type httpStatusError struct {
	What   string
	Status int
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("%s: HTTP %d", e.What, e.Status) }

func statusOf(err error) int {
	var he *httpStatusError
	if errors.As(err, &he) {
		return he.Status
	}
	return 0
}

// tokenSource makes and refreshes OAuth 2 access tokens for a service account (the JWT bearer flow).
type tokenSource struct {
	key    *gcpKey
	client *http.Client
	url    string // the token endpoint; tests replace it
	now    func() time.Time

	mu     sync.Mutex
	token  string
	expiry time.Time
}

func newTokenSource(key *gcpKey, client *http.Client) *tokenSource {
	return &tokenSource{key: key, client: client, url: key.TokenURI, now: time.Now}
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwt makes the signed assertion (RS256), valid for an hour.
func (t *tokenSource) jwt() (string, error) {
	iat := t.now().Unix()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iss": t.key.ClientEmail, "scope": computeScope, "aud": t.key.TokenURI, "iat": iat, "exp": iat + 3600,
	})
	signing := b64url(header) + "." + b64url(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(nil, t.key.private, crypto.SHA256, sum[:])
	if err != nil {
		return "", errors.New("could not sign the token request")
	}
	return signing + "." + b64url(sig), nil
}

// Token returns a valid access token, asking for a new one when the old one expires within 5 minutes.
func (t *tokenSource) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && t.now().Add(5*time.Minute).Before(t.expiry) {
		return t.token, nil
	}
	assertion, err := t.jwt()
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, strings.NewReader(form.Encode()))
	if err != nil {
		return "", errors.New("google token request failed")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("google token request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", &httpStatusError{"google token request", resp.StatusCode}
	}
	var doc struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil || doc.AccessToken == "" || doc.ExpiresIn <= 0 {
		return "", errors.New("google token answer is not valid")
	}
	t.token, t.expiry = doc.AccessToken, t.now().Add(time.Duration(doc.ExpiresIn)*time.Second)
	return t.token, nil
}

// vmRef names one of our VMs.
type vmRef struct{ Zone, Name string }

// computeAPI is what the ping task needs from Google Compute Engine. Tests replace it.
type computeAPI interface {
	// UpZone returns the first zone (by name) of the region that is UP.
	UpZone(ctx context.Context, region string) (string, error)
	// CreateInstance starts creating a VM (it does not wait for the operation).
	CreateInstance(ctx context.Context, zone string, spec instanceSpec) error
	// ListInstances lists the VMs that carry our label, in every zone.
	ListInstances(ctx context.Context) ([]vmRef, error)
	// DeleteInstance starts deleting a VM; a VM that is already gone is not an error.
	DeleteInstance(ctx context.Context, zone, name string) error
}

// computeClient is the REST client of the Compute Engine API (standard library only).
type computeClient struct {
	project string
	base    string // computeBase + project; tests replace it
	tokens  *tokenSource
	client  *http.Client
}

func newComputeClient(project string, tokens *tokenSource, client *http.Client) *computeClient {
	return &computeClient{project: project, base: computeBase + url.PathEscape(project), tokens: tokens, client: client}
}

// do sends one request and decodes a JSON answer into out (if not nil). Any non-2xx answer is an
// httpStatusError with the status only.
func (c *computeClient) do(ctx context.Context, what, method, path string, body any, out any) error {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return errors.New("compute " + what + ": bad request")
		}
		rd = strings.NewReader(string(b))
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return errors.New("compute " + what + ": bad request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("compute " + what + ": request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &httpStatusError{"compute " + what, resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIBody)).Decode(out); err != nil {
		return errors.New("compute " + what + ": answer is not valid")
	}
	return nil
}

func (c *computeClient) UpZone(ctx context.Context, region string) (string, error) {
	best := ""
	token := ""
	for {
		q := url.Values{"filter": {`status = "UP"`}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page struct {
			Items         []struct{ Name, Status string } `json:"items"`
			NextPageToken string                          `json:"nextPageToken"`
		}
		if err := c.do(ctx, "list zones", http.MethodGet, "/zones?"+q.Encode(), nil, &page); err != nil {
			return "", err
		}
		for _, z := range page.Items {
			if strings.HasPrefix(z.Name, region+"-") && z.Status == "UP" && (best == "" || z.Name < best) {
				best = z.Name
			}
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	if best == "" {
		return "", errors.New("no zone is up in the region")
	}
	return best, nil
}

func (c *computeClient) CreateInstance(ctx context.Context, zone string, spec instanceSpec) error {
	return c.do(ctx, "create instance", http.MethodPost, "/zones/"+url.PathEscape(zone)+"/instances", spec, nil)
}

func (c *computeClient) DeleteInstance(ctx context.Context, zone, name string) error {
	err := c.do(ctx, "delete instance", http.MethodDelete, "/zones/"+url.PathEscape(zone)+"/instances/"+url.PathEscape(name), nil, nil)
	if statusOf(err) == http.StatusNotFound {
		return nil
	}
	return err
}

func (c *computeClient) ListInstances(ctx context.Context) ([]vmRef, error) {
	var out []vmRef
	token := ""
	for {
		q := url.Values{"filter": {"labels." + labelKey + " = " + labelValue}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page struct {
			Items map[string]struct {
				Instances []struct {
					Name   string            `json:"name"`
					Zone   string            `json:"zone"`
					Labels map[string]string `json:"labels"`
				} `json:"instances"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := c.do(ctx, "list instances", http.MethodGet, "/aggregated/instances?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		for _, scoped := range page.Items {
			for _, in := range scoped.Instances {
				// The filter is the API's; the label is checked again so nothing else is ever deleted.
				if in.Labels[labelKey] != labelValue || !strings.HasPrefix(in.Name, vmPrefix) {
					continue
				}
				zone := in.Zone
				if i := strings.LastIndex(zone, "/"); i >= 0 {
					zone = zone[i+1:]
				}
				out = append(out, vmRef{Zone: zone, Name: in.Name})
			}
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	return out, nil
}
