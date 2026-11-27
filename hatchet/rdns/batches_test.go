package rdns

import (
	"errors"
	"slices"
	"testing"
)

// The lane's batches come before the walk's, and a batch that is not recorded ends everything after it:
// a stop in the lane leaves every walk batch undone.
func TestSampleInBatches(t *testing.T) {
	lane := make([]sampled, sampleBatch+40)
	for i := range lane {
		lane[i].Near = true
	}
	walk := make([]sampled, 2*sampleBatch+1)
	for i := range walk {
		walk[i].Pos = 1000 + i
	}
	run := func(stopAt int) (finished bool, sizes []int, walks []bool, calls int) {
		finished, err := sampleInBatches(lane, walk, func(batch []sampled) (bool, error) {
			calls++
			return calls != stopAt, nil
		}, func(batch []sampled, isWalk bool) {
			for _, s := range batch {
				if s.Near == isWalk {
					t.Fatalf("a batch mixes the lane and the walk")
				}
			}
			sizes, walks = append(sizes, len(batch)), append(walks, isWalk)
		})
		if err != nil {
			t.Fatal(err)
		}
		return finished, sizes, walks, calls
	}
	finished, sizes, walks, _ := run(0)
	if !finished || !slices.Equal(sizes, []int{sampleBatch, 40, sampleBatch, sampleBatch, 1}) ||
		!slices.Equal(walks, []bool{false, false, true, true, true}) {
		t.Fatalf("all: finished %v, sizes %v, walks %v", finished, sizes, walks)
	}
	// A stop in the second lane batch: one lane batch recorded, no walk batch looked up.
	finished, sizes, walks, calls := run(2)
	if finished || len(sizes) != 1 || walks[0] || calls != 2 {
		t.Fatalf("lane stop: finished %v, sizes %v, walks %v, calls %d", finished, sizes, walks, calls)
	}
	// A stop in the second walk batch: the first walk batch is recorded, nothing after it.
	finished, _, walks, calls = run(4)
	if finished || !slices.Equal(walks, []bool{false, false, true}) || calls != 4 {
		t.Fatalf("walk stop: finished %v, walks %v, calls %d", finished, walks, calls)
	}
	// An error ends it the same way and is returned.
	boom := errors.New("boom")
	if finished, err := sampleInBatches(lane, walk, func([]sampled) (bool, error) { return false, boom }, func([]sampled, bool) {
		t.Fatal("recorded after an error")
	}); finished || err != boom {
		t.Fatalf("error: finished %v, err %v", finished, err)
	}
}
