package ping

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"openipdata.org/hatchet/publish"
)

const fakeSig = "SECRETSIG123"

// fakeStore is an in-memory R2.
type fakeStore struct {
	mu      sync.Mutex
	objs    map[string][]byte
	puts    []string
	putOpts map[string]publish.PutOptions
	deleted []string // prefixes
	events  []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{objs: map[string][]byte{}, putOpts: map[string]publish.PutOptions{}}
}

func (f *fakeStore) set(key string, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objs[key] = b
}

func (f *fakeStore) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objs[key]
	return ok
}

func (f *fakeStore) Stat(_ context.Context, key string) (publish.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objs[key]; !ok {
		return publish.ObjectInfo{}, fmt.Errorf("stat: %w", publish.ErrNotFound)
	}
	return publish.ObjectInfo{}, nil
}

func (f *fakeStore) Download(_ context.Context, key, path string) (time.Time, error) {
	f.mu.Lock()
	b, ok := f.objs[key]
	f.mu.Unlock()
	if !ok {
		return time.Time{}, fmt.Errorf("download: %w", publish.ErrNotFound)
	}
	return time.Time{}, os.WriteFile(path, b, 0o640)
}

func (f *fakeStore) Put(_ context.Context, key, path string, opts publish.PutOptions) (string, publish.PutStatus, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objs[key] = b
	f.puts = append(f.puts, key)
	f.putOpts[key] = opts
	f.events = append(f.events, "put "+key)
	return "r2://b/" + key, publish.PutUploaded, nil
}

func (f *fakeStore) DeletePrefix(_ context.Context, prefix string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for k := range f.objs {
		if strings.HasPrefix(k, prefix) {
			delete(f.objs, k)
			n++
		}
	}
	f.deleted = append(f.deleted, prefix)
	f.events = append(f.events, "delete "+prefix)
	return n, nil
}

func (f *fakeStore) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://r2.example/" + key + "?X-Amz-Signature=" + fakeSig + "&m=get", nil
}

func (f *fakeStore) PresignPut(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://r2.example/" + key + "?X-Amz-Signature=" + fakeSig + "&m=put", nil
}

// fakeCompute is an in-memory Compute Engine.
type fakeCompute struct {
	mu      sync.Mutex
	vms     map[string]vmRef
	specs   []instanceSpec
	creates map[string]int // attempts per VM name
	deleted []string
	listErr error

	// createErr, when set, may fail an attempt (attempt counts from 1).
	createErr func(name string, attempt int) error
	// lose, when set, makes an accepted creation vanish (no VM appears).
	lose func(name string, attempt int) bool
	// onCreate runs after an accepted creation.
	onCreate func(name string)
}

func newFakeCompute() *fakeCompute {
	return &fakeCompute{vms: map[string]vmRef{}, creates: map[string]int{}}
}

func (c *fakeCompute) UpZone(_ context.Context, region string) (string, error) {
	return region + "-b", nil
}

func (c *fakeCompute) CreateInstance(_ context.Context, zone string, spec instanceSpec) error {
	c.mu.Lock()
	c.creates[spec.Name]++
	attempt := c.creates[spec.Name]
	if c.createErr != nil {
		if err := c.createErr(spec.Name, attempt); err != nil {
			c.mu.Unlock()
			return err
		}
	}
	c.specs = append(c.specs, spec)
	if c.lose == nil || !c.lose(spec.Name, attempt) {
		c.vms[spec.Name] = vmRef{zone, spec.Name}
	}
	hook := c.onCreate
	c.mu.Unlock()
	if hook != nil {
		hook(spec.Name)
	}
	return nil
}

func (c *fakeCompute) ListInstances(context.Context) ([]vmRef, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.listErr != nil {
		return nil, c.listErr
	}
	var out []vmRef
	for _, v := range c.vms {
		out = append(out, v)
	}
	return out, nil
}

func (c *fakeCompute) DeleteInstance(_ context.Context, _, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.vms, name)
	c.deleted = append(c.deleted, name)
	return nil
}

func (c *fakeCompute) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.vms)
}

// logs collects log lines.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, s)
}

func (l *logs) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}
