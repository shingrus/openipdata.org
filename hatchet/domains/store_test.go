package domains

import (
	"reflect"
	"testing"
)

func TestPlanPDB(t *testing.T) {
	stored := map[int64]storedDomain{
		1: {domain: "example.com", source: SourcePDB},   // unchanged
		2: {domain: "old.example", source: SourcePDB},   // PeeringDB domain changed
		3: {domain: "gmail.com", source: ""},            // older row: PeeringDB replaces it
		4: {domain: "rdap.example", source: SourceRDAP}, // RDAP value: PeeringDB wins
		5: {domain: "gone.example", source: SourcePDB},  // left PeeringDB: removed
		6: {domain: "keep.example", source: SourceRDAP}, // not in PeeringDB: untouched
	}
	wanted := map[int64]string{
		1: "example.com",
		2: "new.example",
		3: "three.example",
		4: "four.example",
		7: "seven.example", // no row yet
	}
	plan := planPDB(stored, wanted)
	wantSet := map[int64]string{2: "new.example", 3: "three.example", 4: "four.example", 7: "seven.example"}
	if !reflect.DeepEqual(plan.set, wantSet) {
		t.Errorf("set = %v, want %v", plan.set, wantSet)
	}
	if !reflect.DeepEqual(plan.remove, []int64{5}) {
		t.Errorf("remove = %v, want [5]", plan.remove)
	}
	if plan.same != 1 {
		t.Errorf("same = %d, want 1", plan.same)
	}
}
