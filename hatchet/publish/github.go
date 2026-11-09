package publish

import (
	"bytes"
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

// Release replaces one asset on a rolling GitHub release, creating the
// release if needed. Its download URL never changes:
// https://github.com/<repo>/releases/latest/download/<asset>.
type Release struct {
	Token string
	Repo  string // owner/name
	Tag   string
	Name  string

	api  string // API base URL, githubAPI outside tests
	http *http.Client
}

const githubAPI = "https://api.github.com"

// NewRelease returns the rolling "latest" release of repo.
func NewRelease(token, repo string) (*Release, error) {
	if token == "" {
		return nil, errors.New("GITHUB_TOKEN is not set")
	}
	if strings.Count(repo, "/") != 1 {
		return nil, fmt.Errorf("GITHUB_REPO must be owner/name, got %q", repo)
	}
	return &Release{Token: token, Repo: repo, Tag: "latest", Name: "Latest MMDBs",
		api: githubAPI, http: &http.Client{Timeout: 10 * time.Minute}}, nil
}

type release struct {
	ID        int64   `json:"id"`
	UploadURL string  `json:"upload_url"`
	Assets    []asset `json:"assets"`
}

type asset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Replace uploads path as assetName with body as the release text (an empty
// body keeps the current text) and returns the stable download URL. The new file is uploaded under a temporary name, the
// old asset is renamed aside, the new one takes its name, and only then is the
// old one deleted. A failed swap puts the old asset back, so the URL is missing
// only between the two renames.
func (r *Release) Replace(ctx context.Context, path, assetName, body string) (string, error) {
	rel, err := r.ensureRelease(ctx, body)
	if err != nil {
		return "", err
	}
	tmpName, oldName := assetName+".new", assetName+".old"
	byName := map[string]asset{}
	for _, a := range rel.Assets {
		byName[a.Name] = a
	}
	// Clean up after an interrupted run.
	if a, ok := byName[tmpName]; ok {
		if err := r.deleteAsset(ctx, a.ID); err != nil {
			return "", err
		}
	}
	current, hasCurrent := byName[assetName]
	if a, ok := byName[oldName]; ok {
		if hasCurrent {
			if err := r.deleteAsset(ctx, a.ID); err != nil {
				return "", err
			}
		} else {
			current, hasCurrent = a, true // a failed swap left the only copy aside
		}
	}

	uploaded, err := r.uploadAsset(ctx, rel, path, tmpName)
	if err != nil {
		return "", err
	}
	if hasCurrent && current.Name != oldName {
		if err := r.renameAsset(ctx, current.ID, oldName); err != nil {
			return "", err
		}
	}
	if err := r.renameAsset(ctx, uploaded.ID, assetName); err != nil {
		if hasCurrent {
			r.renameAsset(ctx, current.ID, assetName) // best effort; the next run also recovers it
		}
		return "", err
	}
	if hasCurrent {
		r.deleteAsset(ctx, current.ID) // the new file is live; the next run deletes a leftover
	}
	return fmt.Sprintf("https://github.com/%s/releases/latest/download/%s", r.Repo, url.PathEscape(assetName)), nil
}

func (r *Release) renameAsset(ctx context.Context, id int64, name string) error {
	if err := r.do(ctx, http.MethodPatch, r.apiURL(fmt.Sprintf("releases/assets/%d", id)),
		jsonBody(map[string]any{"name": name}), "application/json", -1, nil); err != nil {
		return fmt.Errorf("rename asset %d to %s: %w", id, name, err)
	}
	return nil
}

// ensureRelease creates the release or updates its text, and returns it.
// Several workflows share the release; each passes its own body or none.
func (r *Release) ensureRelease(ctx context.Context, body string) (release, error) {
	var rel release
	err := r.do(ctx, http.MethodGet, r.apiURL("releases/tags/"+url.PathEscape(r.Tag)), nil, "", -1, &rel)
	var httpErr *httpError
	switch {
	case errors.As(err, &httpErr) && httpErr.status == http.StatusNotFound:
		err = r.do(ctx, http.MethodPost, r.apiURL("releases"), jsonBody(map[string]any{
			"tag_name": r.Tag, "name": r.Name, "body": body, "make_latest": "true",
		}), "application/json", -1, &rel)
	case err == nil:
		fields := map[string]any{"name": r.Name, "make_latest": "true"}
		if body != "" {
			fields["body"] = body
		}
		var updated release
		err = r.do(ctx, http.MethodPatch, r.apiURL(fmt.Sprintf("releases/%d", rel.ID)), jsonBody(fields),
			"application/json", -1, &updated)
		if err == nil {
			updated.Assets = rel.Assets
			rel = updated
		}
	}
	if err != nil {
		return release{}, fmt.Errorf("GitHub release %s: %w", r.Tag, err)
	}
	return rel, nil
}

func (r *Release) uploadAsset(ctx context.Context, rel release, path, name string) (asset, error) {
	file, err := os.Open(path)
	if err != nil {
		return asset{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return asset{}, err
	}
	endpoint := strings.SplitN(rel.UploadURL, "{", 2)[0] + "?" + url.Values{"name": {name}}.Encode()
	var uploaded asset
	if err := r.do(ctx, http.MethodPost, endpoint, file, "application/octet-stream", info.Size(), &uploaded); err != nil {
		return asset{}, fmt.Errorf("upload asset %s: %w", name, err)
	}
	return uploaded, nil
}

func (r *Release) deleteAsset(ctx context.Context, id int64) error {
	if err := r.do(ctx, http.MethodDelete, r.apiURL(fmt.Sprintf("releases/assets/%d", id)), nil, "", -1, nil); err != nil {
		return fmt.Errorf("delete asset %d: %w", id, err)
	}
	return nil
}

func (r *Release) apiURL(path string) string {
	return fmt.Sprintf("%s/repos/%s/%s", r.api, r.Repo, path)
}

func jsonBody(v any) io.Reader {
	data, _ := json.Marshal(v)
	return bytes.NewReader(data)
}

type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("GitHub API: HTTP %d: %s", e.status, e.body)
}

// do sends one request; size >= 0 sets Content-Length for streamed bodies.
func (r *Release) do(ctx context.Context, method, endpoint string, body io.Reader, contentType string, size int64, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+r.Token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &httpError{status: resp.StatusCode, body: strings.TrimSpace(string(respBody))}
	}
	if out != nil && len(respBody) > 0 {
		return json.Unmarshal(respBody, out)
	}
	return nil
}
