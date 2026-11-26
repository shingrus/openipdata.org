// Command mmdbbuilder builds and checks openipdata MMDB files.
//
//	mmdbbuilder version
//	mmdbbuilder asn -in <asn-ip/as> -out <ip2asn.mmdb> [-debug]
//	mmdbbuilder geo -out <ip2geo.mmdb> [-names <file.tsv>] [-geonames <cities1000.txt>] [-same-country <layer,...>] [-within-first <layer,...>] [-first-country <layer,...>] [-asn-main <asn-prefixes.csv.gz>] [-fallback] [-holders <holder-cities.csv.gz>] [-rtt-country <speedtest-rtt.csv.gz>] [-rtt-city <speedtest-rtt.csv.gz>] [-ping-country <hosting-rtt.csv.gz>] [-debug] <layer.csv.gz>...
//	mmdbbuilder lookup -db <file.mmdb> [-ip <ip>] [-ips <file>]
//	mmdbbuilder lookup -db <ip2geo.mmdb> -in <ips.txt> -out <cities.tsv> [-with-source [-countries]]
//	mmdbbuilder gaps -db <ip2geo.mmdb> -out <gaps.txt>
//	mmdbbuilder stamp -db <ip2geo.mmdb> -meta <meta.json> -out <stamped.mmdb>
//	mmdbbuilder convert -db <ip2geo.mmdb|ip2asn.mmdb> -out <openipdata-org-ip2geo.mmdb|openipdata-org-ip2asn.mmdb> [-geonames <cities1000.txt>]
//	mmdbbuilder eval -db <ip2geo.mmdb> -rir <10-rir.csv.gz> [-dbip <file.mmdb>] [-maxmind <file.mmdb>] [-atlas <probes.csv> [-geonames <cities1000.txt>]] [-holdout-db <file.mmdb> -holdout-truth <rows.csv>]
//
// For geo, -geonames adds latitude and longitude to the records whose city it places.
// -asn-main (an asn-prefixes file, gzipped CSV asn,prefix) adds a last, guessed fill: where
// the addresses of an ASN in a country that have a city are at least 90% in one city, its
// addresses in that country with no city get it (IPv4 only, city_source 80-asn-main). See asnmain.go.
// -fallback adds more guesses for the addresses still without a city, first one that
// applies wins: 82-asn-likely (the ASN's main city at 70%), 84-asn-near (the nearest city of
// the same ASN and country in the same /20), 86-country-main (the country's main city at
// 30%, also for addresses with no ASN) and, only where the country has no main city,
// 88-asn-far (the nearest city of the same ASN and country in the same /16) and
// 89-asn-anywhere (the same without a distance limit) and, last, 89-country-near (the nearest
// city of the same country from any network). Without
// -asn-main only 86-country-main runs. See fallback.go.
// -holders (a holder-cities file, gzipped CSV in the layer layout; needs -asn-main) adds 85-holder-city, which
// runs before 86-country-main: address space that no network announces gets the city of the
// holder block that covers it, when the block's country is the address's country. See holders.go.
// -rtt-country (an RTT file from import-mlab-sites, gzipped CSV prefix,country,city,rtt_ms; needs
// -geonames) runs after all layers and before the guesses: a /24 whose record's country has no
// GeoNames place within (r+1)*102.1+300 km of the M-Lab server that answered it in r ms gets
// the country of its fastest server, when that country passes the same test. See rttcountry.go.
// -ping-country (a gzipped CSV: prefix, then one column <CC>/<City> per measuring site with the ping
// time in ms; needs -geonames) runs right after it: a /24 whose country has no GeoNames cell inside
// every ping-time disk takes the country that holds at least half of the possible weight. See pingcountry.go.
// A layer named in -same-country applies only where the country so
// far is the same or unknown. A layer named in -within-first is used only for
// rows no broader than one prefix of the first layer; others are skipped. A
// layer named in -first-country applies a row only to the addresses where the
// first layer gives the row's country (a broad row is split); a row with no
// such address is skipped.
//
// lookup with -in and -out reads an ip2geo MMDB for the IPs of a file (one per
// line) and writes ip<TAB>country<TAB>city for each one that has a city (a guessed city,
// 80-asn-main, 82-asn-likely, 84-asn-near, 85-holder-city, 86-country-main, 88-asn-far, 89-asn-anywhere or 89-country-near, is left out); with -with-source each row also has a fourth column, the
// record's city_source, and with -countries an IP that has a country but no city also gets a
// row (empty city). See lookup.go.
//
// gaps lists the IPv4 /24s of an ip2geo MMDB that have a country and no city (a city whose
// source is one of the ten guesses, 25-bgp, 35-speedtest-site, 36-mlab, 37-whois-arin, 37-whois-text or 39-github-geofeeds counts as none), as lines
// "<first /24> <count of consecutive /24s> <country>". See gaps.go.
//
// stamp rewrites only the metadata section of an MMDB: description.en becomes a
// readable build line and description.openipdata the compact JSON of the meta
// file. See stamp.go.
//
// convert turns the flat ip2geo MMDB into a file in the MaxMind GeoLite2-City record layout
// (continent, country, city, location; English names only; database_type OpenIPData-City). With
// -geonames a city whose GeoNames place has the record's own coordinates also gets its
// geoname_id and time zone. A flat ip2asn MMDB becomes the GeoLite2-ASN layout
// (autonomous_system_number, autonomous_system_organization, plus the extra keys name
// and country_code of the flat file; database_type
// "OpenIPData-ASN (compat=GeoLite2-ASN)"). The input's database_type decides which.
// The metadata description and build epoch of the input are kept. See convert.go.
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

// version is set at build time (-ldflags "-X main.version=..."); "dev" for a plain go build.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
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
	case "gaps":
		runGaps(os.Args[2:])
	case "convert":
		runConvert(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mmdbbuilder version")
	fmt.Fprintln(os.Stderr, "       mmdbbuilder asn -in <asn-ip/as> -out <file.mmdb> [-debug]")
	fmt.Fprintln(os.Stderr, "       mmdbbuilder geo -out <file.mmdb> [-names <file.tsv>] [-geonames <cities1000.txt>] [-same-country <layer,...>] [-within-first <layer,...>] [-first-country <layer,...>] [-asn-main <asn-prefixes.csv.gz>] [-fallback] [-holders <holder-cities.csv.gz>] [-rtt-country <speedtest-rtt.csv.gz>] [-rtt-city <speedtest-rtt.csv.gz>] [-ping-country <hosting-rtt.csv.gz>] [-debug] <layer.csv.gz>...")
	fmt.Fprintln(os.Stderr, "       mmdbbuilder lookup -db <file.mmdb> [-ip <ip>] [-ips <file>]")
	fmt.Fprintln(os.Stderr, "       mmdbbuilder lookup -db <ip2geo.mmdb> -in <ips.txt> -out <cities.tsv> [-with-source [-countries]]")
	fmt.Fprintln(os.Stderr, "       mmdbbuilder gaps -db <ip2geo.mmdb> -out <gaps.txt>")
	fmt.Fprintln(os.Stderr, "       mmdbbuilder stamp -db <ip2geo.mmdb> -meta <meta.json> -out <stamped.mmdb>")
	fmt.Fprintln(os.Stderr, "       mmdbbuilder convert -db <ip2geo.mmdb|ip2asn.mmdb> -out <file.mmdb> [-geonames <cities1000.txt>]")
	fmt.Fprintln(os.Stderr, "       mmdbbuilder eval -db <ip2geo.mmdb> -rir <10-rir.csv.gz> [-dbip <file.mmdb>] [-maxmind <file.mmdb>] [-atlas <probes.csv> [-geonames <cities1000.txt>]] [-holdout-db <file.mmdb> -holdout-truth <rows.csv>]")
	os.Exit(2)
}

func runGeo(args []string) {
	flags := flag.NewFlagSet("geo", flag.ExitOnError)
	out := flags.String("out", "", "output MMDB path")
	names := flags.String("names", "", "country names, one code<TAB>name per line (tzdata names fill the gaps)")
	debugMode := flags.Bool("debug", false, "include the network in each record")
	geonames := flags.String("geonames", "", "GeoNames cities1000.txt: records whose city it places get latitude and longitude (optional)")
	sameCountry := flags.String("same-country", "", "layer file names (comma-separated) whose rows apply only where the country so far is the same or unknown")
	withinFirst := flags.String("within-first", "", "layer file names (comma-separated) whose rows are used only if no broader than one prefix of the first layer")
	asnMain := flags.String("asn-main", "", "asn-prefixes.csv.gz (asn,prefix; IPv4 rows used): after all layers, give the main city of an ASN to its addresses in a country that have no city (a guess, city_source 80-asn-main)")
	fallback := flags.Bool("fallback", false, "after the ASN main city fill, more guesses for addresses still without a city: 82-asn-likely, 84-asn-near, 88-asn-far, 89-asn-anywhere, 89-country-near (all need -asn-main) and 86-country-main")
	holders := flags.String("holders", "", "holder-cities.csv.gz (layer layout; needs -asn-main): address space that no network announces gets the city of the holder block that covers it (a guess, city_source 85-holder-city)")
	rttCountry := flags.String("rtt-country", "", "speedtest-rtt.csv.gz (prefix,country,city,rtt_ms; needs -geonames): after all layers and before the guesses, a /24 whose country is too far from the M-Lab server that answered it in r ms gets the country of its fastest server")
	rttCity := flags.String("rtt-city", "", "speedtest-rtt.csv.gz (the file of -rtt-country; needs -geonames): after the guesses, a city outside a reply-time disk of its /24 (not from 70-geofeeds) is replaced by a network city that fits (87-rtt-network) or the most populous city of the country that fits (87-rtt-country)")
	pingCountry := flags.String("ping-country", "", "hosting-rtt.csv.gz (prefix, then one column <CC>/<City> per measuring site with the ping time in ms; needs -geonames): right after -rtt-country and before the guesses, a /24 whose country has no place inside every ping-time disk takes the country that holds at least half of the possible places")
	firstCountry := flags.String("first-country", "", "layer file names (comma-separated) whose rows apply only to the addresses where the first layer gives the row's country")
	flags.Parse(args)

	if *out == "" || flags.NArg() == 0 {
		usage()
	}
	if *holders != "" && *asnMain == "" {
		fmt.Fprintln(os.Stderr, "error: -holders needs -asn-main")
		os.Exit(2)
	}
	if *rttCountry != "" && *geonames == "" {
		fmt.Fprintln(os.Stderr, "error: -rtt-country needs -geonames")
		os.Exit(2)
	}
	if *pingCountry != "" && *geonames == "" {
		fmt.Fprintln(os.Stderr, "error: -ping-country needs -geonames")
		os.Exit(2)
	}
	if *rttCity != "" && *geonames == "" {
		fmt.Fprintln(os.Stderr, "error: -rtt-city needs -geonames")
		os.Exit(2)
	}
	var index geoNames
	var folded foldedNames
	if *geonames != "" {
		var err error
		if index, folded, err = loadGeoNamesFolded(*geonames, true); err != nil {
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", *geonames, err)
			os.Exit(1)
		}
	}
	stats, err := buildGeoNames(flags.Args(), *names, *out, *debugMode, nameSet(*sameCountry), nameSet(*withinFirst), nameSet(*firstCountry), index, folded, *asnMain, *fallback, *holders, *rttCountry, *rttCity, *pingCountry)
	prefixes, placed, unplaced, renamed := 0, 0, 0, 0
	for _, s := range stats {
		fmt.Printf("layer %s: %d rows, %d prefixes, %d skipped\n", s.name, s.rows, s.prefixes, s.skipped)
		if s.note != "" {
			fmt.Println(s.note)
		}
		if s.noCity > 0 {
			fmt.Printf("layer %s: %d cities left out\n", s.name, s.noCity)
		}
		prefixes += s.prefixes
		placed += s.placed
		unplaced += s.unplaced
		renamed += s.renamed
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if index != nil {
		fmt.Printf("city-names: %d rows renamed\n", renamed)
		fmt.Printf("coordinates: %d records placed, %d cities not placed\n", placed, unplaced)
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
	withSource := flags.Bool("with-source", false, "with -in: add a fourth column, the record's city_source (empty when none)")
	countries := flags.Bool("countries", false, "with -with-source: also write a row (empty city) for an IP whose record has a country but no city")
	flags.Parse(args)

	if *db == "" || flags.NArg() > 0 {
		usage()
	}
	if (*withSource && (*in == "" || *out == "")) || (*countries && !*withSource) {
		usage()
	}
	if *in != "" || *out != "" {
		if *in == "" || *out == "" || *ip != "" || *ips != "" {
			usage()
		}
		stats, err := writeCitiesSource(*db, *in, *out, *withSource, *countries)
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
	flags.StringVar(&in.holdoutTruth, "holdout-truth", "", "CSV prefix,country,city[,feed] of the held-out rows (optional, needs -holdout-db)")
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

func runGaps(args []string) {
	flags := flag.NewFlagSet("gaps", flag.ExitOnError)
	db := flags.String("db", "", "ip2geo MMDB to read")
	out := flags.String("out", "", "output file: lines <first /24> <count> <country>")
	flags.Parse(args)

	if *db == "" || *out == "" || flags.NArg() > 0 {
		usage()
	}
	stats, err := writeGaps(*db, *out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d /24s in %d runs; skipped %d split between countries, %d with a city in a part)\n",
		*out, stats.networks, stats.runs, stats.split, stats.hasCity)
}
