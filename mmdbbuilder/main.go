// Command mmdbbuilder builds and checks openipdata MMDB files.
//
//	mmdbbuilder asn -in <asn-ip/as> -out <ip2asn.mmdb> [-debug]
//	mmdbbuilder lookup -db <file.mmdb> [-ip <ip>] [-ips <file>]
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "asn":
		runASN(os.Args[2:])
	case "lookup":
		runLookup(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mmdbbuilder asn -in <asn-ip/as> -out <file.mmdb> [-debug]")
	fmt.Fprintln(os.Stderr, "       mmdbbuilder lookup -db <file.mmdb> [-ip <ip>] [-ips <file>]")
	os.Exit(2)
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
