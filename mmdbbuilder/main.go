// Command mmdbbuilder builds and checks openipdata MMDB files.
//
//	mmdbbuilder asn -in <asn-ip/as> -out <ip2asn.mmdb> [-debug]
//	mmdbbuilder geo -out <ip2geo.mmdb> [-names <file.tsv>] [-same-country <layer,...>] [-within-first <layer,...>] [-first-country <layer,...>] [-debug] <layer.csv.gz>...
//	mmdbbuilder lookup -db <file.mmdb> [-ip <ip>] [-ips <file>]
//	mmdbbuilder lookup -db <ip2geo.mmdb> -in <ips.txt> -out <cities.tsv>
//	mmdbbuilder stamp -db <ip2geo.mmdb> -meta <meta.json> -out <stamped.mmdb>
//	mmdbbuilder eval -db <ip2geo.mmdb> -rir <10-rir.csv.gz> [-dbip <file.mmdb>] [-maxmind <file.mmdb>] [-atlas <probes.csv> [-geonames <cities1000.txt>]] [-holdout-db <file.mmdb> -holdout-truth <rows.csv>]
//
// For geo, a layer named in -same-country applies only where the country so
// far is the same or unknown. A layer named in -within-first is used only for
// rows no broader than one prefix of the first layer; others are skipped. A
// layer named in -first-country applies a row only to the addresses where the
// first layer gives the row's country (a broad row is split); a row with no
// such address is skipped.
//
// lookup with -in and -out reads an ip2geo MMDB for the IPs of a file (one per
// line) and writes ip<TAB>country<TAB>city for each one that has a city. See lookup.go.
//
// stamp rewrites only the metadata section of an MMDB: description.en becomes a
// readable build line and description.openipdata the compact JSON of the meta
// file. See stamp.go.
//
// eval scores an ip2geo MMDB (IPv4 only) and prints one JSON object. See eval.go.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "asn":
		runASN(os.Args[2:])
	case "geo":
		runGeo(os.Args[2:])
	case "lookup":
		runLookup(os.Args[2:])
	case "eval":
		runEval(os.Args[2:])
	case "stamp":
		runStamp(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mmdbbuilder asn -in <asn-ip/as> -out <file.mmdb> [-debug]")
	fmt.Fprintln(os.Stderr, "       mmdbbuilder geo -out <file.mmdb> [-names <file.tsv>] [-same-country <layer,...>] [-within-first <layer,...>] [-first-country <layer,...>] [-debug] <layer.csv.gz>...")
	fmt.Fprintln(os.Stderr, "       mmdbbuilder lookup -db <file.mmdb> [-ip <ip>] [-ips <file>]")
	fmt.Fprintln(os.Stderr, "       mmdbbuilder lookup -db <ip2geo.mmdb> -in <ips.txt> -out <cities.tsv>")
	fmt.Fprintln(os.Stderr, "       mmdbbuilder stamp -db <ip2geo.mmdb> -meta <meta.json> -out <stamped.mmdb>")
	fmt.Fprintln(os.Stderr, "       mmdbbuilder eval -db <ip2geo.mmdb> -rir <10-rir.csv.gz> [-dbip <file.mmdb>] [-maxmind <file.mmdb>] [-atlas <probes.csv> [-geonames <cities1000.txt>]] [-holdout-db <file.mmdb> -holdout-truth <rows.csv>]")
	os.Exit(2)
}

func runGeo(args []string) {
	flags := flag.NewFlagSet("geo", flag.ExitOnError)
	out := flags.String("out", "", "output MMDB path")
	names := flags.String("names", "", "country names, one code<TAB>name per line (tzdata names fill the gaps)")
	debugMode := flags.Bool("debug", false, "include the network in each record")
	sameCountry := flags.String("same-country", "", "layer file names (comma-separated) whose rows apply only where the country so far is the same or unknown")
	withinFirst := flags.String("within-first", "", "layer file names (comma-separated) whose rows are used only if no broader than one prefix of the first layer")
	firstCountry := flags.String("first-country", "", "layer file names (comma-separated) whose rows apply only to the addresses where the first layer gives the row's country")
	flags.Parse(args)

	if *out == "" || flags.NArg() == 0 {
		usage()
	}
	stats, err := buildGeo(flags.Args(), *names, *out, *debugMode, nameSet(*sameCountry), nameSet(*withinFirst), nameSet(*firstCountry))
	prefixes := 0
	for _, s := range stats {
		fmt.Printf("layer %s: %d rows, %d prefixes, %d skipped\n", s.name, s.rows, s.prefixes, s.skipped)
		if s.noCity > 0 {
			fmt.Printf("layer %s: %d cities left out\n", s.name, s.noCity)
		}
		prefixes += s.prefixes
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d layers, %d prefixes)\n", *out, len(stats), prefixes)
}

// nameSet turns a comma-separated list of layer file names into a set.
func nameSet(list string) map[string]bool {
	set := map[string]bool{}
	for _, name := range strings.Split(list, ",") {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = true
		}
	}
	return set
}

func runLookup(args []string) {
	flags := flag.NewFlagSet("lookup", flag.ExitOnError)
	db := flags.String("db", "", "MMDB file to query")
	ip := flags.String("ip", "", "single IP to look up (overrides -ips and the builtin IPs)")
	ips := flags.String("ips", "", "file with one IP per line, looked up together with the builtin IPs")
	in := flags.String("in", "", "file with one IP per line: write each one's city to -out (an ip2geo MMDB)")
	out := flags.String("out", "", "with -in: output TSV, ip<TAB>country<TAB>city, only for IPs that have a city")
	flags.Parse(args)

	if *db == "" || flags.NArg() > 0 {
		usage()
	}
	if *in != "" || *out != "" {
		if *in == "" || *out == "" || *ip != "" || *ips != "" {
			usage()
		}
		stats, err := writeCities(*db, *in, *out)
		if stats.invalid > 0 || stats.failed > 0 {
			// Counts only: the lines themselves may be anything.
			fmt.Fprintf(os.Stderr, "warning: skipped %d invalid lines, %d failed lookups\n", stats.invalid, stats.failed)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("looked up %d IPs, %d with a city, wrote %s\n", stats.lookups, stats.withCity, *out)
		return
	}
	if err := printLookups(*db, *ip, *ips); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func runEval(args []string) {
	flags := flag.NewFlagSet("eval", flag.ExitOnError)
	var in evalInputs
	flags.StringVar(&in.db, "db", "", "ip2geo MMDB to score")
	flags.StringVar(&in.rir, "rir", "", "RIR layer CSV: the IPv4 space that should have a country")
	flags.StringVar(&in.dbip, "dbip", "", "DB-IP city MMDB (optional)")
	flags.StringVar(&in.maxmind, "maxmind", "", "MaxMind GeoLite2 City MMDB (optional)")
	flags.StringVar(&in.atlas, "atlas", "", "RIPE Atlas probes CSV ip,country,lat,lon (optional)")
	flags.StringVar(&in.geonames, "geonames", "", "GeoNames cities1000.txt: places our cities for atlas_city_pct (optional)")
	flags.StringVar(&in.holdoutDB, "holdout-db", "", "MMDB built without the held-out geofeeds (optional, needs -holdout-truth)")
	flags.StringVar(&in.holdoutTruth, "holdout-truth", "", "CSV prefix,country,city of the held-out rows (optional, needs -holdout-db)")
	flags.Parse(args)

	if in.db == "" || in.rir == "" || flags.NArg() > 0 {
		usage()
	}
	metrics, err := evaluate(in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(metrics); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func runASN(args []string) {
	flags := flag.NewFlagSet("asn", flag.ExitOnError)
	in := flags.String("in", "", "asn-ip \"as\" directory with per-ASN aggregated.json files")
	out := flags.String("out", "", "output MMDB path")
	debugMode := flags.Bool("debug", false, "include the network in each record")
	flags.Parse(args)

	if *in == "" || *out == "" {
		usage()
	}
	if info, err := os.Stat(*in); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "error: ASN directory not found: %s\n", *in)
		os.Exit(2)
	}

	stats, err := buildASN(*in, *out, *debugMode)
	if stats.skipped > 0 {
		fmt.Fprintf(os.Stderr, "warning: skipped %d ASN entries\n", stats.skipped)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d ASN entries, %d prefixes)\n", *out, stats.entries, stats.prefixes)
	if stats.duplicates > 0 {
		fmt.Printf("%d prefixes announced by several ASNs; the lowest ASN kept each\n", stats.duplicates)
	}
}
