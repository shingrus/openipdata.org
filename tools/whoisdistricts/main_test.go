package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFold(t *testing.T) {
	for in, want := range map[string]string{
		"Dalston": "dalston", "Saint-Étienne": "saintetienne", "St. Pauls": "saintpauls", "Zürich": "zurich", "": "",
	} {
		if got := fold(in); got != want {
			t.Errorf("fold(%q) = %q, want %q", in, got, want)
		}
	}
}

func writeZip(t *testing.T, dir, name, inner string, lines []string) string {
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create(inner)
	for _, l := range lines {
		w.Write([]byte(l + "\n"))
	}
	zw.Close()
	f.Close()
	return path
}

// geonames builds a line with the columns the tool reads.
func geonames(name, ascii, lat, lon, feat, cc, pop string) string {
	f := make([]string, 19)
	f[0], f[1], f[2], f[4], f[5], f[7], f[8], f[14] = "1", name, ascii, lat, lon, feat, cc, pop
	out := f[0]
	for _, v := range f[1:] {
		out += "\t" + v
	}
	return out
}

func TestDistricts(t *testing.T) {
	dir := t.TempDir()
	cities := writeZip(t, dir, "c.zip", "cities1000.txt", []string{
		geonames("London", "London", "51.5", "-0.12", "PPLC", "GB", "8900000"),
		geonames("Dalston", "Dalston", "54.85", "-2.99", "PPL", "GB", "1285"),
		geonames("Bromley", "Bromley", "51.4", "0.02", "PPL", "GB", "5000"),
		geonames("Hackney", "Hackney", "51.54", "-0.05", "PPL", "GB", "30000"), // not small
	})
	all := writeZip(t, dir, "a.zip", "allCountries.txt", []string{
		geonames("Dalston", "Dalston", "51.546", "-0.075", "PPLX", "GB", "0"),   // near London, clashes
		geonames("Bromley", "Bromley", "51.40", "0.02", "PPLX", "GB", "0"),      // near London, clashes
		geonames("Hackney", "Hackney", "51.54", "-0.05", "PPLX", "GB", "0"),     // clashes only with a big place
		geonames("Faraway", "Faraway", "54.0", "-2.0", "PPLX", "GB", "0"),       // no clash
		geonames("Dalston", "Dalston", "56.0", "-3.0", "PPLX", "GB", "0"),       // clash but far from London: still the same entry
		geonames("Marais", "Marais", "48.85", "2.35", "PPLX", "FR", "0"),        // no big place in cities
		geonames("Dalston", "Dalston", "51.546", "-0.075", "PPL", "GB", "1000"), // not a district
	})
	big, small, err := readCities(cities)
	if err != nil {
		t.Fatal(err)
	}
	got, err := districts(all, big, small)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"GB\tBromley", "GB\tDalston"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}
