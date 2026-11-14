package main

// mmdbbuilder stamp: put the build time and the scores into the metadata of an
// MMDB. Only the metadata section is rewritten; the search tree and the data
// section stay byte for byte the same, so every lookup gives what it gave.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
)

const (
	maxStampMeta   = 64 << 10
	stampKey       = "openipdata"
	stampSampleMax = 5000 // networks compared before and after
)

// stampMMDB writes in to out with description.en replaced by a readable line
// and description.openipdata set to the compact JSON of the meta file. It
// verifies the result and leaves no out file on any failure.
func stampMMDB(in, metaPath, out string) (err error) {
	compact, builtAt, err := readStampMeta(in, metaPath)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	idx := bytes.LastIndex(data, metadataMarker)
	if idx < 0 {
		return errors.New("not an MMDB: no metadata marker")
	}
	head := data[:idx+len(metadataMarker)]

	src, err := maxminddb.Open(in)
	if err != nil {
		return err
	}
	defer src.Close()
	m := src.Metadata
	desc := map[string]string{}
	for k, v := range m.Description {
		desc[k] = v
	}
	desc["en"] = "openipdata.org ip2geo, built " + builtAt.UTC().Format("2006-01-02 15:04") + " UTC"
	desc[stampKey] = compact
	encoded, err := encodeMetadata(standardMetadata{
		Major: uint16(m.BinaryFormatMajorVersion), Minor: uint16(m.BinaryFormatMinorVersion),
		BuildEpoch: uint64(m.BuildEpoch), DatabaseType: m.DatabaseType, Description: desc,
		IPVersion: uint16(m.IPVersion), Languages: m.Languages,
		NodeCount: uint32(m.NodeCount), RecordSize: uint16(m.RecordSize),
	})
	if err != nil {
		return err
	}
	tmp := out + ".tmp"
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	if err = os.WriteFile(tmp, append(append([]byte{}, head...), encoded...), 0o640); err != nil {
		return err
	}
	if err = verifyStamped(src, tmp, compact); err != nil {
		return err
	}
	return os.Rename(tmp, out)
}

// readStampMeta returns the compact meta JSON and the build time to show:
// built_at of the meta file if it has one, else the MMDB's build_epoch (which
// is also put into the compact JSON, so a retried stamp gives the same text).
func readStampMeta(mmdbPath, metaPath string) (string, time.Time, error) {
	info, err := os.Stat(metaPath)
	if err != nil {
		return "", time.Time{}, err
	}
	if info.Size() > maxStampMeta {
		return "", time.Time{}, fmt.Errorf("meta file is over %d bytes", maxStampMeta)
	}
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return "", time.Time{}, err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return "", time.Time{}, errors.New("meta file is not a JSON object")
	}
	var built time.Time
	if s, ok := obj["built_at"].(string); ok {
		built, _ = time.Parse(time.RFC3339, s)
	}
	if built.IsZero() {
		r, err := maxminddb.Open(mmdbPath)
		if err != nil {
			return "", time.Time{}, err
		}
		built = r.Metadata.BuildTime()
		r.Close()
		obj["built_at"] = built.UTC().Format(time.RFC3339)
	}
	compact, err := json.Marshal(obj)
	if err != nil {
		return "", time.Time{}, err
	}
	if len(compact) > maxStampMeta {
		return "", time.Time{}, fmt.Errorf("meta is over %d bytes", maxStampMeta)
	}
	return string(compact), built, nil
}

// verifyStamped opens the new file and checks it against the source: Verify,
// metadata other than the description, the stamp itself, and the records of a
// sample of networks.
func verifyStamped(src *maxminddb.Reader, path, compact string) error {
	dst, err := maxminddb.Open(path)
	if err != nil {
		return fmt.Errorf("stamped file: %w", err)
	}
	defer dst.Close()
	if err := dst.Verify(); err != nil {
		return fmt.Errorf("stamped file: %w", err)
	}
	a, b := src.Metadata, dst.Metadata
	a.Description, b.Description = nil, nil
	if !reflect.DeepEqual(a, b) {
		return errors.New("stamped file: metadata differs")
	}
	if dst.Metadata.Description[stampKey] != compact {
		return errors.New("stamped file: stamp not readable")
	}
	// Compare every Nth network so that a big file stays cheap.
	step := int(src.Metadata.NodeCount)/stampSampleMax + 1
	i := 0
	for res := range src.Networks(maxminddb.IncludeNetworksWithoutData()) {
		if err := res.Err(); err != nil {
			return err
		}
		if i++; (i-1)%step != 0 {
			continue
		}
		var want, got any
		if err := res.Decode(&want); err != nil {
			return err
		}
		other := dst.Lookup(res.Prefix().Addr())
		if err := other.Err(); err != nil {
			return err
		}
		if other.Prefix() != res.Prefix() || other.Found() != res.Found() {
			return fmt.Errorf("stamped file: lookup of %s differs", res.Prefix())
		}
		if err := other.Decode(&got); err != nil {
			return err
		}
		if !reflect.DeepEqual(want, got) {
			return fmt.Errorf("stamped file: record of %s differs", res.Prefix())
		}
	}
	return nil
}

func runStamp(args []string) {
	flags := flag.NewFlagSet("stamp", flag.ExitOnError)
	db := flags.String("db", "", "input MMDB")
	meta := flags.String("meta", "", "JSON object to put into description.openipdata")
	out := flags.String("out", "", "output MMDB (may be the input path)")
	flags.Parse(args)
	if *db == "" || *meta == "" || *out == "" || flags.NArg() > 0 {
		usage()
	}
	if err := stampMMDB(*db, *meta, *out); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("stamped %s\n", *out)
}
