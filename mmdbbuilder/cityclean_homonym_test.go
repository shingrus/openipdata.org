package main

import (
	"os"
	"path/filepath"
	"testing"
)

func cleanerFor(t *testing.T, body string) *cityCleaner {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cities1000.txt")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	index, folded, err := loadGeoNamesFolded(path, true)
	if err != nil {
		t.Fatal(err)
	}
	return &cityCleaner{index: index, folded: folded}
}

// A name that was cut is matched only when no namesake of comparable size exists.
func TestCityCleanCutHomonyms(t *testing.T) {
	c := cleanerFor(t, gnRow("11", "Union", "Union", "", "1", "1", "US", "5000")+
		gnRow("12", "Union", "Union", "", "2", "2", "US", "4000")+ // a second Union
		gnRow("13", "Eau Claire", "Eau Claire", "", "3", "3", "US", "60000")+
		gnRow("14", "Bigtown", "Bigtown", "", "4", "4", "US", "500000")+
		gnRow("15", "Bigtown", "Bigtown", "", "5", "5", "US", "40000")+ // 500000 >= 10 * 40000
		gnRow("16", "Tinytown", "Tinytown", "", "6", "6", "US", "5000")+
		gnRow("17", "Tinytown", "Tinytown", "", "7", "7", "US", "4999"))
	for _, tc := range []struct {
		city, want string
		renamed    bool
	}{
		{"UNION (EAU CLAIRE)", "UNION (EAU CLAIRE)", false}, // cut, two Unions of like size
		{"Union/WI", "Union/WI", false},
		{"Tinytown, X", "Tinytown, X", false},
		{"UNIÓN", "Union", true},                // nothing cut: the best place wins as before
		{"Bigtown (Bayou)", "Bigtown", true},    // cut, but ten times bigger than the namesake
		{"Eau Claire (WI)", "Eau Claire", true}, // cut, one place
	} {
		if got, renamed := c.clean("US", tc.city); got != tc.want || renamed != tc.renamed {
			t.Errorf("clean(%q) = %q, %v; want %q, %v", tc.city, got, renamed, tc.want, tc.renamed)
		}
	}
}

// The main name written must place at the same place: an alternate name of a small place
// whose main name belongs to a bigger homonym is not renamed.
func TestCityCleanMainNameMustPlaceThere(t *testing.T) {
	c := cleanerFor(t, gnRow("30", "Springfield", "Springfield", "Spring Field Village", "1", "1", "US", "100")+
		gnRow("31", "Springfield", "Springfield", "", "2", "2", "US", "100000"))
	for _, city := range []string{"Spring-Field Village", "SPRING FIELD VILLAGE X"} {
		if got, renamed := c.clean("US", city); renamed || got != city {
			t.Errorf("clean(%q) = %q, %v; want it unchanged", city, got, renamed)
		}
	}
	if e := c.folded["US|spring field village"]; e.id != 30 || e.places != 1 {
		t.Errorf("entry = %+v", e)
	}
	if e := c.folded["US|springfield"]; e.id != 31 || e.places != 2 || e.second != 100 {
		t.Errorf("entry = %+v", e)
	}
}

// Each distinct name is cleaned once; the count is still per row.
func TestCityCleanMemo(t *testing.T) {
	c := cleanerFor(t, gnRow("1", "São Paulo", "Sao Paulo", "", "1", "1", "BR", "10"))
	for i := 0; i < 3; i++ {
		if got, renamed := c.clean("BR", "São Paulo/SP"); got != "São Paulo" || !renamed {
			t.Fatalf("clean = %q, %v", got, renamed)
		}
	}
	if len(c.memo) != 1 {
		t.Errorf("memo has %d entries", len(c.memo))
	}
}
