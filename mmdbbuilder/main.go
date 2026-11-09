// Command mmdbbuilder builds and checks openipdata MMDB files.
//
//	mmdbbuilder asn -in <asn-ip/as> -out <ip2asn.mmdb> [-debug]
//	mmdbbuilder geo -out <ip2geo.mmdb> [-names <file.tsv>] [-same-country <layer,...>] [-within-first <layer,...>] [-debug] <layer.csv[.gz]>...
//	mmdbbuilder lookup -db <file.mmdb> [-ip <ip>] [-ips <file>]
//
// For geo, a layer named in -same-country applies only where the country so
// far is the same or unknown. A layer named in -within-first is used only for
// rows no broader than one prefix of the first layer; others are skipped.
package main

import (
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
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mmdbbuilder asn -in <asn-ip/as> -out <file.mmdb> [-debug]")
	fmt.Fprintln(os.Stderr, "       mmdbbuilder geo -out <file.mmdb> [-names <file.tsv>] [-same-country <layer,...>] [-within-first <layer,...>] [-debug] <layer.csv[.gz]>...")
	fmt.Fprintln(os.Stderr, "       mmdbbuilder lookup -db <file.mmdb> [-ip <ip>] [-ips <file>]")
	os.Exit(2)
}

func runGeo(args []string) {
	flags := flag.NewFlagSet("geo", flag.ExitOnError)
	out := flags.String("out", "", "output MMDB path")
	names := flags.String("names", "", "country names, one code<TAB>name per line (tzdata names fill the gaps)")
	debugMode := flags.Bool("debug", false, "include the network in each record")
	sameCountry := flags.String("same-country", "", "layer file names (comma-separated) whose rows apply only where the country so far is the same or unknown")
	withinFirst := flags.String("within-first", "", "layer file names (comma-separated) whose rows are used only if no broader than one prefix of the first layer")
	flags.Parse(args)

	if *out == "" || flags.NArg() == 0 {
		usage()
	}
	stats, err := buildGeo(flags.Args(), *names, *out, *debugMode, nameSet(*sameCountry), nameSet(*withinFirst))
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
	flags.Parse(args)

	if *db == "" {
		usage()
	}
	if err := printLookups(*db, *ip, *ips); err != nil {
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
