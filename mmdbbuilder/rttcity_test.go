package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func rttCityIndex(t *testing.T, dir string) geoNames {
	t.Helper()
	path := writeFile(t, filepath.Join(dir, "cities1000.txt"),
		geoNamesLine("1", "London", "", "51.50853", "-0.12574", "GB", "8961989")+
			geoNamesLine("2", "Manchester", "", "53.48095", "-2.23743", "GB", "510746")+
			geoNamesLine("3", "Edinburgh", "", "55.95206", "-3.19648", "GB", "464990"))
	index, err := loadGeoNames(path)
	if err != nil {
		t.Fatal(err)
	}
	return index
}

func TestRTTCity(t *testing.T) {
	dir := t.TempDir()
	index := rttCityIndex(t, dir)
	main := writeFile(t, filepath.Join(dir, "10-main.csv.gz"), "prefix,country,region,city,source\n"+
		"10.1.1.0/24,GB,,Edinburgh,x\n"+ // impossible at 2 ms from London, AS1: B = Manchester
		"10.1.2.0/24,GB,,London,x\n"+ // possible: kept
		"10.1.9.0/23,GB,,Manchester,x\n"+ // AS1's biggest allowed city
		"10.2.1.0/24,GB,,Edinburgh,x\n"+ // AS2 has no allowed city: C = London (the largest)
		"10.3.1.0/24,GB,,Edinburgh,x\n") // no ASN: C = London
	feeds := writeFile(t, filepath.Join(dir, "70-geofeeds.csv.gz"), "prefix,country,region,city,source\n"+
		"10.1.3.0/24,GB,,Edinburgh,x\n") // geofeed city: never touched
	asn := writeFile(t, filepath.Join(dir, "asn.csv.gz"), "asn,prefix\n1,10.1.0.0/16\n2,10.2.0.0/16\n")
	var rtt strings.Builder
	rtt.WriteString(rttHeader)
	for _, p := range []string{"10.1.1.0", "10.1.2.0", "10.1.3.0", "10.2.1.0", "10.3.1.0"} {
		rtt.WriteString(p + "/24,GB,London,2\n")
	}
	rttPath := writeFile(t, filepath.Join(dir, "rtt.csv.gz"), rtt.String())
	out := filepath.Join(dir, "out.mmdb")
	stats, err := buildGeoNames([]string{main, feeds}, "", out, false, nil, nil, nil, index, nil, asn, false, "", "", rttPath, "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, st := range stats {
		got[st.name] = st.rows
	}
	if got[rttNetworkSource] != 1 || got[rttCountrySource] != 2 || !isGuessSource(rttNetworkSource) || !isGuessSource(rttCountrySource) {
		t.Errorf("stats = %v", got)
	}
	checkCities(t, out, map[string]string{
		"10.1.1.9": "GB/Manchester/87-rtt-network",
		"10.1.2.9": "GB/London/10-main",
		"10.1.3.9": "GB/Edinburgh/70-geofeeds",
		"10.2.1.9": "GB/London/87-rtt-country",
		"10.3.1.9": "GB/London/87-rtt-country",
		"10.1.9.9": "GB/Manchester/10-main",
	})

	// Without any city that fits, the record stays: 0.5 ms from London allows 150 km, and
	// London itself is the only city inside, so with London absent from the evidence it is kept.
	rtt2 := writeFile(t, filepath.Join(dir, "rtt2.csv.gz"), rttHeader+"10.1.1.0/24,GB,London,0.5\n")
	out2 := filepath.Join(dir, "out2.mmdb")
	onlyEdi := writeFile(t, filepath.Join(dir, "20-edi.csv.gz"), "prefix,country,region,city,source\n10.1.1.0/24,GB,,Edinburgh,x\n")
	if _, err := buildGeoNames([]string{onlyEdi}, "", out2, false, nil, nil, nil, index, nil, "", false, "", "", rtt2, ""); err != nil {
		t.Fatal(err)
	}
	checkCities(t, out2, map[string]string{"10.1.1.9": "GB/Edinburgh/20-edi"})

	// Needs GeoNames.
	if _, err := buildGeoNames([]string{main}, "", out2, false, nil, nil, nil, nil, nil, "", false, "", "", rttPath, ""); err == nil {
		t.Error("-rtt-city without GeoNames was accepted")
	}
}
