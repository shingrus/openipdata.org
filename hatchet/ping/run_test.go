package ping

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func testTiming() *timing {
	return &timing{
		Poll: 3 * time.Millisecond, CreateRetry: 15 * time.Millisecond, Lost: 20 * time.Millisecond,
		RegionTimeout: 150 * time.Millisecond, ProgressEvery: 10 * time.Millisecond, Cleanup: time.Second,
	}
}

func regionOf(vm string) string { return strings.TrimPrefix(vm, vmPrefix) }

// newRun makes a pipeline over the fakes. Every created VM answers at once with a result, unless
// silent says that its region never does.
func newRun(t *testing.T, silent map[string]bool) (*Pipeline, *fakeStore, *fakeCompute, *logs) {
	t.Helper()
	store, comp, lg := newFakeStore(), newFakeCompute(), &logs{}
	comp.onCreate = func(name string) {
		if !silent[regionOf(name)] {
			store.set(ResultKey(regionOf(name)), []byte("x"))
		}
	}
	p := &Pipeline{DataDir: t.TempDir(), GCPKey: "k", Progress: lg.add, store: store, compute: comp, timing: testTiming()}
	return p, store, comp, lg
}

func TestPingAllDone(t *testing.T) {
	p, store, comp, lg := newRun(t, nil)
	out, err := p.Ping(context.Background())
	if err != nil || out.RegionsDone != 27 || out.RegionsFailed != 0 || out.TimedOut != 0 || out.CreateRetries != 0 {
		t.Fatalf("%+v %v", out, err)
	}
	if len(comp.specs) != 27 || comp.count() != 0 {
		t.Errorf("created %d, left %d", len(comp.specs), comp.count())
	}
	if len(comp.deleted) != 27 {
		t.Errorf("deleted %d", len(comp.deleted))
	}
	if !strings.Contains(lg.text(), "already have a result") {
		t.Errorf("logs %q", lg.text())
	}
	_ = store
}

func TestPingSpecAndScript(t *testing.T) {
	p, _, comp, _ := newRun(t, nil)
	if _, err := p.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	var spec instanceSpec
	for _, s := range comp.specs {
		if s.Name == "hosting-ping-us-east1" {
			spec = s
		}
	}
	if spec.Name == "" {
		t.Fatal("no VM for us-east1")
	}
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	if sa, ok := m["serviceAccounts"].([]any); !ok || len(sa) != 0 {
		t.Errorf("serviceAccounts must be an empty list: %v", m["serviceAccounts"])
	}
	sched := m["scheduling"].(map[string]any)
	if sched["instanceTerminationAction"] != "DELETE" || sched["maxRunDuration"].(map[string]any)["seconds"] != "14400" {
		t.Errorf("scheduling %v", sched)
	}
	if m["machineType"] != "zones/us-east1-b/machineTypes/e2-micro" || m["labels"].(map[string]any)["openipdata"] != "hosting-ping" {
		t.Errorf("machine %v labels %v", m["machineType"], m["labels"])
	}
	disk := m["disks"].([]any)[0].(map[string]any)
	if disk["autoDelete"] != true || disk["boot"] != true ||
		disk["initializeParams"].(map[string]any)["sourceImage"] != "projects/debian-cloud/global/images/family/debian-12" {
		t.Errorf("disk %v", disk)
	}
	nic := m["networkInterfaces"].([]any)[0].(map[string]any)
	if nic["network"] != "global/networks/default" || nic["accessConfigs"].([]any)[0].(map[string]any)["type"] != "ONE_TO_ONE_NAT" {
		t.Errorf("nic %v", nic)
	}
	script := spec.Metadata.Items[0].Value
	if spec.Metadata.Items[0].Key != "startup-script" {
		t.Error("no startup-script")
	}
	for _, want := range []string{
		"'https://r2.example/" + TargetsKey + "?X-Amz-Signature=" + fakeSig + "&m=get'",
		"'https://r2.example/" + ResultKey("us-east1") + "?X-Amz-Signature=" + fakeSig + "&m=put'",
		"'https://r2.example/" + ProgressKey("us-east1") + "?X-Amz-Signature=" + fakeSig + "&m=put'",
		"fping -C 1 -q -r 0 -t 1000 -i 1", "split -l 200000", "shuf targets.txt",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	// No other secret: the only X-Amz-Signature values are the three presigned URLs.
	if strings.Count(script, "X-Amz-Signature") != 3 || strings.Contains(script, "GCP") || strings.Contains(script, "BEGIN") {
		t.Error("the script holds something else than the three URLs")
	}
	// The script never prints a URL.
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "echo") && strings.Contains(line, "URL") {
			t.Errorf("echo of a URL: %q", line)
		}
	}
}

func TestShQuote(t *testing.T) {
	if got := shQuote("a'b&c"); got != `'a'\''b&c'` {
		t.Errorf("%q", got)
	}
}

func TestPingCreateRetry(t *testing.T) {
	p, _, comp, _ := newRun(t, nil)
	comp.createErr = func(name string, attempt int) error {
		if name == "hosting-ping-us-east1" && attempt == 1 {
			return &httpStatusError{"compute create instance", 403}
		}
		return nil
	}
	out, err := p.Ping(context.Background())
	if err != nil || out.RegionsDone != 27 || out.CreateRetries != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	if comp.creates["hosting-ping-us-east1"] != 2 {
		t.Errorf("attempts %d", comp.creates["hosting-ping-us-east1"])
	}
}

func TestPingLostCreation(t *testing.T) {
	p, _, comp, _ := newRun(t, nil)
	comp.lose = func(name string, attempt int) bool { return name == "hosting-ping-asia-east1" && attempt == 1 }
	comp.onCreate = nil // the result comes only from a VM that exists
	store := p.store.(*fakeStore)
	comp.onCreate = func(name string) {
		comp.mu.Lock()
		_, exists := comp.vms[name]
		comp.mu.Unlock()
		if exists {
			store.set(ResultKey(regionOf(name)), []byte("x"))
		}
	}
	out, err := p.Ping(context.Background())
	if err != nil || out.RegionsDone != 27 || out.CreateRetries != 1 {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestPingTimeout(t *testing.T) {
	p, _, comp, _ := newRun(t, map[string]bool{"asia-east1": true})
	out, err := p.Ping(context.Background())
	if err != nil || out.RegionsDone != 26 || out.RegionsFailed != 1 || out.TimedOut != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	if comp.count() != 0 {
		t.Error("a VM is left")
	}
}

func TestPingTooFewRegions(t *testing.T) {
	silent := map[string]bool{}
	for _, r := range regions[:10] {
		silent[r.Name] = true
	}
	p, _, comp, _ := newRun(t, silent)
	out, err := p.Ping(context.Background())
	if err == nil || out.RegionsDone != 17 || out.TimedOut != 10 {
		t.Fatalf("%+v %v", out, err)
	}
	if comp.count() != 0 {
		t.Error("a VM is left")
	}
}

func TestPingFailedNote(t *testing.T) {
	p, store, comp, lg := newRun(t, map[string]bool{"us-west1": true})
	store.set(ProgressKey("us-west1"), []byte("us-west1 failed: fping did not install"))
	store.set(ProgressKey("us-east1"), []byte("us-east1 pinging, chunk 1 of 9 done"))
	p.timing.RegionTimeout = time.Second
	out, err := p.Ping(context.Background())
	if err != nil || out.RegionsDone != 26 || out.RegionsFailed != 1 || out.TimedOut != 0 {
		t.Fatalf("%+v %v", out, err)
	}
	if comp.count() != 0 || !strings.Contains(lg.text(), "failed 1") {
		t.Errorf("logs %q", lg.text())
	}
}

func TestPingCleansUpOnCancel(t *testing.T) {
	p, _, comp, _ := newRun(t, map[string]bool{"us-east1": true, "us-east4": true})
	ctx, cancel := context.WithCancel(context.Background())
	comp.onCreate = func(name string) { cancel() }
	_, err := p.Ping(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	if comp.count() != 0 {
		t.Errorf("%d VMs left after a cancel", comp.count())
	}
}

func TestPingErrorAtStart(t *testing.T) {
	p, _, comp, _ := newRun(t, nil)
	comp.listErr = errors.New("api down")
	if _, err := p.Ping(context.Background()); err == nil {
		t.Fatal("a failed list must fail the task")
	}
	if len(comp.specs) != 0 {
		t.Error("a VM was created")
	}
}

func TestPingLeftoversAndDoneRegions(t *testing.T) {
	p, store, comp, _ := newRun(t, nil)
	comp.vms["hosting-ping-old"] = vmRef{"us-east1-b", "hosting-ping-old"}
	for _, r := range regions[:5] {
		store.set(ResultKey(r.Name), []byte("x"))
	}
	out, err := p.Ping(context.Background())
	if err != nil || out.Leftovers != 1 || out.RegionsDone != 27 {
		t.Fatalf("%+v %v", out, err)
	}
	if len(comp.specs) != 22 {
		t.Errorf("created %d, want the 22 regions without a result", len(comp.specs))
	}
	if comp.deleted[0] != "hosting-ping-old" {
		t.Errorf("the leftover is not deleted first: %v", comp.deleted[:1])
	}
}

func TestPingSkipsWithoutKey(t *testing.T) {
	p, _, comp, _ := newRun(t, nil)
	p.GCPKey = ""
	out, err := p.Ping(context.Background())
	if err != nil || out.Skipped != "no GCP key" || len(comp.specs) != 0 {
		t.Errorf("%+v %v", out, err)
	}
}

func TestPingLogsHaveNoSecrets(t *testing.T) {
	p, store, comp, lg := newRun(t, map[string]bool{"asia-east1": true})
	comp.createErr = func(name string, attempt int) error {
		if attempt == 1 {
			return errors.New("boom")
		}
		return nil
	}
	_, err := p.Ping(context.Background())
	all := lg.text()
	if err != nil {
		all += err.Error()
	}
	for _, bad := range []string{"X-Amz-Signature", fakeSig, "r2.example", "BEGIN", "10.", "https://"} {
		if strings.Contains(all, bad) {
			t.Errorf("a log line or error holds %q", bad)
		}
	}
	_ = store
}
