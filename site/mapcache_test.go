package main

import (
	"html/template"
	"strconv"
	"sync"
	"testing"
)

func TestMapCache(t *testing.T) {
	c := newMapCache(3)
	drawn := 0
	get := func(code string) template.HTML {
		return c.get(mapKey{code: code}, func() template.HTML { drawn++; return template.HTML("<" + code + ">") })
	}
	for _, code := range []string{"AA", "BB", "AA", "CC", "AA"} {
		if got := get(code); got != template.HTML("<"+code+">") {
			t.Fatalf("%s: got %q", code, got)
		}
	}
	if drawn != 3 || c.len() != 3 {
		t.Fatalf("drawn %d, kept %d, want 3 and 3", drawn, c.len())
	}
	get("DD") // BB was used longest ago: it goes
	if c.len() != 3 {
		t.Fatalf("kept %d after one more, want 3", c.len())
	}
	get("AA")
	get("CC")
	if drawn != 4 {
		t.Errorf("AA and CC were drawn again (drawn %d, want 4)", drawn)
	}
	get("BB")
	if drawn != 5 {
		t.Errorf("BB was not dropped (drawn %d, want 5)", drawn)
	}
	// A city and a country are different keys, and an empty map is kept too.
	if c.get(mapKey{city: true}, func() template.HTML { return "" }) != "" {
		t.Error("empty map changed")
	}
}

func TestMapCacheConcurrent(t *testing.T) {
	c := newMapCache(16)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				k := strconv.Itoa((g + i) % 40)
				if got := c.get(mapKey{code: k}, func() template.HTML { return template.HTML(k) }); got != template.HTML(k) {
					t.Errorf("key %s: got %q", k, got)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if c.len() > 16 {
		t.Errorf("kept %d, limit 16", c.len())
	}
}

// The cache must not change what a page shows: a cached map equals a drawn one.
func TestPlaceMapCached(t *testing.T) {
	w, err := parseWorld(worldBin)
	if err != nil {
		t.Fatal(err)
	}
	lat, lon := 51.5074, -0.1278
	g := &geoRecord{CityName: "London", CountryCode: "GB", Latitude: &lat, Longitude: &lon}
	want, _ := w.drawMap(lat, lon, "", true)
	if a, b := w.placeMap(g), w.placeMap(g); a != want || b != want {
		t.Error("cached city map differs from the drawn one")
	}
	wantRU, _ := w.countryMap("RU")
	ru := &geoRecord{CountryCode: "RU"}
	if a, b := w.placeMap(ru), w.placeMap(ru); a != wantRU || b != wantRU {
		t.Error("cached country map differs from the drawn one")
	}
	if w.placeMap(&geoRecord{CountryCode: "zz"}) != "" || w.placeMap(&geoRecord{}) != "" {
		t.Error("no map expected without a country code")
	}
}
