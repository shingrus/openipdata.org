package publish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeGitHub serves the release endpoints Replace uses and records each call.
type fakeGitHub struct {
	t       *testing.T
	server  *httptest.Server
	exists  bool
	assets  []asset
	calls   []string
	upload  []byte
	renames []string // "<id>:<new name>"
	patched map[string]any
	failNew bool // fail renaming the uploaded asset (ID 99)
}

func newFakeGitHub(t *testing.T, exists bool, assets []asset) *fakeGitHub {
	f := &fakeGitHub{t: t, exists: exists, assets: assets}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tok" {
		f.t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
	}
	call := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		call += "?" + r.URL.RawQuery
	}
	f.calls = append(f.calls, call)
	rel := map[string]any{"id": 7, "upload_url": f.server.URL + "/uploads/7/assets{?name,label}", "assets": f.assets}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/releases/tags/latest":
		if !f.exists {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(rel)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/releases":
		rel["assets"] = []asset{}
		json.NewEncoder(w).Encode(rel)
	case r.Method == http.MethodPatch && r.URL.Path == "/repos/o/r/releases/7":
		json.NewDecoder(r.Body).Decode(&f.patched)
		json.NewEncoder(w).Encode(map[string]any{"id": 7, "upload_url": rel["upload_url"]})
	case r.Method == http.MethodPost && r.URL.Path == "/uploads/7/assets":
		f.upload, _ = io.ReadAll(r.Body)
		if r.ContentLength != int64(len(f.upload)) {
			f.t.Errorf("Content-Length %d for %d bytes", r.ContentLength, len(f.upload))
		}
		json.NewEncoder(w).Encode(asset{ID: 99, Name: r.URL.Query().Get("name")})
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/o/r/releases/assets/"):
		id := strings.TrimPrefix(r.URL.Path, "/repos/o/r/releases/assets/")
		if id == "99" && f.failNew {
			http.Error(w, `{"message":"boom"}`, http.StatusBadGateway)
			return
		}
		var body struct{ Name string }
		json.NewDecoder(r.Body).Decode(&body)
		f.renames = append(f.renames, id+":"+body.Name)
		json.NewEncoder(w).Encode(map[string]any{"name": body.Name})
	default:
		http.Error(w, "unexpected", http.StatusBadRequest)
	}
}

func (f *fakeGitHub) release(t *testing.T) *Release {
	r, err := NewRelease("tok", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	r.api = f.server.URL
	return r
}

func mmdbFile(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "ip2asn.mmdb")
	if err := os.WriteFile(path, []byte("mmdb bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReleaseReplaceExisting(t *testing.T) {
	f := newFakeGitHub(t, true, []asset{
		{ID: 1, Name: "ip2asn-openipdata.mmdb"},
		{ID: 2, Name: "ip2asn-openipdata.mmdb.new"}, // left over from an interrupted run
		{ID: 3, Name: "other.txt"},
	})
	link, replaced, err := f.release(t).Replace(context.Background(), mmdbFile(t), "ip2asn-openipdata.mmdb", "Built: today")
	if err != nil || !replaced {
		t.Fatal(err, replaced)
	}
	if link != "https://github.com/o/r/releases/latest/download/ip2asn-openipdata.mmdb" {
		t.Errorf("link = %q", link)
	}
	want := []string{
		"GET /repos/o/r/releases/tags/latest",
		"PATCH /repos/o/r/releases/7",
		"DELETE /repos/o/r/releases/assets/2",
		"POST /uploads/7/assets?name=ip2asn-openipdata.mmdb.new",
		"PATCH /repos/o/r/releases/assets/1",
		"PATCH /repos/o/r/releases/assets/99",
		"DELETE /repos/o/r/releases/assets/1",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	wantRenames := []string{"1:ip2asn-openipdata.mmdb.old", "99:ip2asn-openipdata.mmdb"}
	if string(f.upload) != "mmdb bytes" || !reflect.DeepEqual(f.renames, wantRenames) {
		t.Errorf("upload %q renames %v", f.upload, f.renames)
	}
}

func TestReleaseReplaceRestoresOnFailedSwap(t *testing.T) {
	f := newFakeGitHub(t, true, []asset{{ID: 1, Name: "ip2asn-openipdata.mmdb"}})
	f.failNew = true
	if _, _, err := f.release(t).Replace(context.Background(), mmdbFile(t), "ip2asn-openipdata.mmdb", "x"); err == nil {
		t.Fatal("want an error when the new asset cannot be renamed")
	}
	want := []string{"1:ip2asn-openipdata.mmdb.old", "1:ip2asn-openipdata.mmdb"}
	if !reflect.DeepEqual(f.renames, want) {
		t.Errorf("renames = %v, want %v", f.renames, want)
	}
	for _, call := range f.calls {
		if call == "DELETE /repos/o/r/releases/assets/1" {
			t.Error("old asset deleted after a failed swap")
		}
	}
}

func TestReleaseReplaceRecoversAsideCopy(t *testing.T) {
	// A previous run failed both the swap and the restore: the only copy is .old.
	f := newFakeGitHub(t, true, []asset{{ID: 1, Name: "ip2asn-openipdata.mmdb.old"}})
	if _, _, err := f.release(t).Replace(context.Background(), mmdbFile(t), "ip2asn-openipdata.mmdb", "x"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /repos/o/r/releases/tags/latest",
		"PATCH /repos/o/r/releases/7",
		"POST /uploads/7/assets?name=ip2asn-openipdata.mmdb.new",
		"PATCH /repos/o/r/releases/assets/99",
		"DELETE /repos/o/r/releases/assets/1",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestReleaseReplaceCreates(t *testing.T) {
	f := newFakeGitHub(t, false, nil)
	if _, _, err := f.release(t).Replace(context.Background(), mmdbFile(t), "ip2asn-openipdata.mmdb", "x"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /repos/o/r/releases/tags/latest",
		"POST /repos/o/r/releases",
		"POST /uploads/7/assets?name=ip2asn-openipdata.mmdb.new",
		"PATCH /repos/o/r/releases/assets/99",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
}

func TestNewReleaseChecks(t *testing.T) {
	if _, err := NewRelease("", "o/r"); err == nil {
		t.Error("want an error without a token")
	}
	if _, err := NewRelease("tok", "nope"); err == nil {
		t.Error("want an error for a repo without owner")
	}
}

func TestPing(t *testing.T) {
	if err := Ping(context.Background(), ""); err != nil {
		t.Errorf("empty URL: %v", err)
	}
	pinged := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { pinged++ }))
	defer server.Close()
	if err := Ping(context.Background(), server.URL+"/ping/uuid"); err != nil || pinged != 1 {
		t.Errorf("err=%v pinged=%d", err, pinged)
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusNotFound)
	}))
	defer down.Close()
	err := Ping(context.Background(), down.URL+"/ping/secret-uuid")
	if err == nil || strings.Contains(err.Error(), "secret-uuid") {
		t.Errorf("err = %v", fmt.Sprint(err))
	}
}

func TestReleaseReplaceKeepsTextWithoutBody(t *testing.T) {
	f := newFakeGitHub(t, true, nil)
	r := &Release{Token: "tok", Repo: "o/r", Tag: "latest", Name: "Latest MMDBs", api: f.server.URL, http: f.server.Client()}
	path := filepath.Join(t.TempDir(), "geo.mmdb")
	os.WriteFile(path, []byte("geo"), 0o644)
	if _, _, err := r.Replace(context.Background(), path, "ip2geo-openipdata.mmdb", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.patched["body"]; ok || f.patched["name"] != "Latest MMDBs" {
		t.Errorf("release update = %v, want no body", f.patched)
	}
}

// The release has the asset with the digest of the file: nothing is uploaded or
// renamed, only the release text is updated.
func TestReleaseReplaceSkipsSameDigest(t *testing.T) {
	sum := sha256.Sum256([]byte("mmdb bytes"))
	f := newFakeGitHub(t, true, []asset{
		{ID: 1, Name: "ip2asn-openipdata.mmdb", Digest: "sha256:" + hex.EncodeToString(sum[:])},
		{ID: 2, Name: "ip2asn-openipdata.mmdb.new"}, // left over from an interrupted run: still cleaned up
	})
	link, replaced, err := f.release(t).Replace(context.Background(), mmdbFile(t), "ip2asn-openipdata.mmdb", "Built: today")
	if err != nil || replaced || link != "https://github.com/o/r/releases/latest/download/ip2asn-openipdata.mmdb" {
		t.Fatalf("link %q replaced %v err %v", link, replaced, err)
	}
	want := []string{
		"GET /repos/o/r/releases/tags/latest",
		"PATCH /repos/o/r/releases/7",
		"DELETE /repos/o/r/releases/assets/2",
	}
	if !reflect.DeepEqual(f.calls, want) || f.upload != nil || f.renames != nil {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
}

// A different digest, none at all, or one that is not sha256 means the file is uploaded.
func TestReleaseReplaceUploadsOtherDigest(t *testing.T) {
	sum := sha256.Sum256([]byte("mmdb bytes"))
	same := hex.EncodeToString(sum[:])
	for name, digest := range map[string]string{
		"other file": "sha256:" + strings.Repeat("0", 64),
		"no digest":  "",
		"sha512":     "sha512:" + same,
		"no prefix":  same,
	} {
		f := newFakeGitHub(t, true, []asset{{ID: 1, Name: "ip2asn-openipdata.mmdb", Digest: digest}})
		_, replaced, err := f.release(t).Replace(context.Background(), mmdbFile(t), "ip2asn-openipdata.mmdb", "x")
		if err != nil || !replaced || string(f.upload) != "mmdb bytes" {
			t.Errorf("%s: replaced %v err %v upload %q", name, replaced, err, f.upload)
		}
	}
	// The digest of the aside copy is not the live asset's: only .old is there, so it is replaced.
	f := newFakeGitHub(t, true, []asset{{ID: 1, Name: "ip2asn-openipdata.mmdb.old", Digest: "sha256:" + same}})
	if _, replaced, err := f.release(t).Replace(context.Background(), mmdbFile(t), "ip2asn-openipdata.mmdb", "x"); err != nil || !replaced {
		t.Errorf("aside copy only: replaced %v err %v", replaced, err)
	}
}
