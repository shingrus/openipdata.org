package ping

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	doc, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "proj-1", "client_email": "svc@proj-1.iam.gserviceaccount.com",
		"private_key": pemText, "token_uri": "https://oauth2.googleapis.com/token",
	})
	return priv, string(doc)
}

func TestParseGCPKey(t *testing.T) {
	priv, doc := testKey(t)
	for name, enc := range map[string]*base64.Encoding{
		"std": base64.StdEncoding, "raw std": base64.RawStdEncoding, "url": base64.URLEncoding, "raw url": base64.RawURLEncoding,
	} {
		k, err := parseGCPKey(enc.EncodeToString([]byte(doc)))
		if err != nil || k.ProjectID != "proj-1" || k.ClientEmail != "svc@proj-1.iam.gserviceaccount.com" || !k.private.Equal(priv) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// White space (a wrapped value) is fine.
	wrapped := base64.StdEncoding.EncodeToString([]byte(doc))
	if _, err := parseGCPKey(wrapped[:40] + "\n" + wrapped[40:] + "\n"); err != nil {
		t.Errorf("wrapped: %v", err)
	}
}

func TestParseGCPKeyBad(t *testing.T) {
	_, doc := testKey(t)
	replace := func(old, new string) string {
		return base64.StdEncoding.EncodeToString([]byte(strings.Replace(doc, old, new, 1)))
	}
	bad := map[string]string{
		"empty":        "",
		"not base64":   "not base64 !!!",
		"not json":     base64.StdEncoding.EncodeToString([]byte("hello")),
		"no email":     replace("svc@proj-1.iam.gserviceaccount.com", ""),
		"no pem":       replace("BEGIN PRIVATE KEY", "BEGIN PUBLIC KEY X"),
		"other host":   replace("oauth2.googleapis.com", "evil.example"),
		"http":         replace("https://oauth2", "http://oauth2"),
		"userinfo":     replace("https://oauth2", "https://u:p@oauth2"),
		"bad key body": base64.StdEncoding.EncodeToString([]byte(`{"client_email":"a@b","private_key":"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"}`)),
	}
	for name, in := range bad {
		_, err := parseGCPKey(in)
		if !errors.Is(err, ErrInvalidGCPKey) {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if strings.Contains(err.Error(), in) && in != "" || strings.Contains(err.Error(), "PRIVATE") {
			t.Errorf("%s: the error repeats the key", name)
		}
	}
}

func TestJWT(t *testing.T) {
	priv, doc := testKey(t)
	key, err := parseGCPKey(base64.StdEncoding.EncodeToString([]byte(doc)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	ts := newTokenSource(key, http.DefaultClient)
	ts.now = func() time.Time { return now }
	tok, err := ts.jwt()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatal("not a JWT")
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&priv.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Errorf("signature: %v", err)
	}
	var h map[string]string
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	json.Unmarshal(hb, &h)
	if h["alg"] != "RS256" || h["typ"] != "JWT" {
		t.Errorf("header %v", h)
	}
	var c map[string]any
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	json.Unmarshal(cb, &c)
	if c["iss"] != "svc@proj-1.iam.gserviceaccount.com" || c["scope"] != computeScope || c["aud"] != "https://oauth2.googleapis.com/token" ||
		c["iat"] != float64(1_800_000_000) || c["exp"] != float64(1_800_003_600) {
		t.Errorf("claims %v", c)
	}
}

func TestTokenRefresh(t *testing.T) {
	_, doc := testKey(t)
	key, _ := parseGCPKey(base64.StdEncoding.EncodeToString([]byte(doc)))
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		if form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || strings.Count(form.Get("assertion"), ".") != 2 {
			http.Error(w, "bad", 400)
			return
		}
		n := calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok" + string(rune('0'+n)), "expires_in": 3600})
	}))
	defer srv.Close()
	now := time.Unix(1_800_000_000, 0)
	ts := newTokenSource(key, srv.Client())
	ts.url = srv.URL
	ts.now = func() time.Time { return now }
	ctx := context.Background()
	if tok, err := ts.Token(ctx); err != nil || tok != "tok1" {
		t.Fatalf("%q %v", tok, err)
	}
	now = now.Add(50 * time.Minute) // 10 minutes left: still valid
	if tok, _ := ts.Token(ctx); tok != "tok1" || calls.Load() != 1 {
		t.Errorf("not cached: %q %d", tok, calls.Load())
	}
	now = now.Add(7 * time.Minute) // 3 minutes left: refreshed
	if tok, _ := ts.Token(ctx); tok != "tok2" || calls.Load() != 2 {
		t.Errorf("not refreshed: %q %d", tok, calls.Load())
	}
}

func TestTokenErrorHasStatusOnly(t *testing.T) {
	_, doc := testKey(t)
	key, _ := parseGCPKey(base64.StdEncoding.EncodeToString([]byte(doc)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, "SECRET-BODY assertion")
	}))
	defer srv.Close()
	ts := newTokenSource(key, srv.Client())
	ts.url = srv.URL
	_, err := ts.Token(context.Background())
	if err == nil || statusOf(err) != 400 || strings.Contains(err.Error(), "SECRET-BODY") || strings.Contains(err.Error(), srv.URL) {
		t.Errorf("%v", err)
	}
}

func fakeComputeServer(t *testing.T, h http.HandlerFunc) *computeClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ts := &tokenSource{token: "T", expiry: time.Now().Add(time.Hour), now: time.Now}
	return &computeClient{base: srv.URL + "/projects/p", tokens: ts, client: srv.Client()}
}

func TestUpZone(t *testing.T) {
	c := fakeComputeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer T" || r.URL.Path != "/projects/p/zones" {
			http.Error(w, "bad", 400)
			return
		}
		if r.URL.Query().Get("pageToken") == "" {
			io.WriteString(w, `{"items":[{"name":"europe-west1-d","status":"UP"},{"name":"europe-west10-a","status":"UP"}],"nextPageToken":"n"}`)
			return
		}
		io.WriteString(w, `{"items":[{"name":"europe-west1-b","status":"UP"},{"name":"europe-west1-a","status":"DOWN"}]}`)
	})
	z, err := c.UpZone(context.Background(), "europe-west1")
	if err != nil || z != "europe-west1-b" {
		t.Errorf("%q %v", z, err)
	}
	if _, err := c.UpZone(context.Background(), "mars-north1"); err == nil {
		t.Error("no zone must be an error")
	}
}

func TestComputeCalls(t *testing.T) {
	var created instanceSpec
	c := fakeComputeServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/projects/p/zones/us-east1-b/instances":
			json.NewDecoder(r.Body).Decode(&created)
			io.WriteString(w, `{}`)
		case r.Method == "GET" && r.URL.Path == "/projects/p/aggregated/instances":
			if r.URL.Query().Get("filter") != "labels.openipdata = hosting-ping" {
				http.Error(w, "bad filter", 400)
				return
			}
			io.WriteString(w, `{"items":{"zones/us-east1-b":{"instances":[
				{"name":"hosting-ping-us-east1","zone":"https://x/zones/us-east1-b","labels":{"openipdata":"hosting-ping"}},
				{"name":"hosting-ping-foreign","zone":"https://x/zones/us-east1-b","labels":{"other":"x"}},
				{"name":"someone-else","zone":"https://x/zones/us-east1-b","labels":{"openipdata":"hosting-ping"}}]},
				"zones/us-west1-a":{"warning":{"code":"NO_RESULTS_ON_PAGE"}}}}`)
		case r.Method == "DELETE" && r.URL.Path == "/projects/p/zones/us-east1-b/instances/gone":
			w.WriteHeader(404)
			io.WriteString(w, "SECRET-BODY")
		case r.Method == "DELETE":
			w.WriteHeader(403)
			io.WriteString(w, "SECRET-BODY")
		default:
			http.Error(w, "bad", 400)
		}
	})
	ctx := context.Background()
	spec := newInstanceSpec("us-east1-b", "hosting-ping-us-east1", "script")
	if err := c.CreateInstance(ctx, "us-east1-b", spec); err != nil || created.Name != "hosting-ping-us-east1" || len(created.ServiceAccounts) != 0 {
		t.Errorf("create: %v %+v", err, created)
	}
	list, err := c.ListInstances(ctx)
	if err != nil || len(list) != 1 || list[0] != (vmRef{"us-east1-b", "hosting-ping-us-east1"}) {
		t.Errorf("list: %v %v", list, err)
	}
	if err := c.DeleteInstance(ctx, "us-east1-b", "gone"); err != nil {
		t.Errorf("a missing VM is not an error: %v", err)
	}
	err = c.DeleteInstance(ctx, "us-east1-b", "x")
	if err == nil || statusOf(err) != 403 || strings.Contains(err.Error(), "SECRET-BODY") {
		t.Errorf("delete error: %v", err)
	}
}
