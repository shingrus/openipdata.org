package main

// A small encoder for the MaxMind DB metadata section (MaxMind DB spec, "Data
// Section" encoding). It writes only what metadata needs: map, utf8 string,
// uint16, uint32, uint64 and array.

import (
	"errors"
	"fmt"
	"sort"
)

// metadataMarker starts the metadata section; the map follows it.
var metadataMarker = []byte("\xab\xcd\xefMaxMind.com")

// MaxMind DB data types (the numbers of the spec).
const (
	typeString = 2
	typeUint16 = 5
	typeMap    = 7
	typeUint32 = 6
	typeUint64 = 9
	typeArray  = 11
)

// appendControl writes the control byte, the extended type byte (types above 7)
// and the size bytes: sizes under 29 are in the control byte; 29 adds one byte
// (size 29+n), 30 two bytes (285+n), 31 three bytes (65821+n).
func appendControl(b []byte, typ, size int) ([]byte, error) {
	var t byte
	var ext []byte
	if typ <= 7 {
		t = byte(typ) << 5
	} else {
		ext = []byte{byte(typ - 7)}
	}
	switch {
	case size < 0:
		return nil, errors.New("negative size")
	case size < 29:
		b = append(b, t|byte(size))
		return append(b, ext...), nil
	case size < 285:
		b = append(b, t|29)
		b = append(b, ext...)
		return append(b, byte(size-29)), nil
	case size < 65821:
		n := size - 285
		b = append(b, t|30)
		b = append(b, ext...)
		return append(b, byte(n>>8), byte(n)), nil
	case size < 65821+1<<24:
		n := size - 65821
		b = append(b, t|31)
		b = append(b, ext...)
		return append(b, byte(n>>16), byte(n>>8), byte(n)), nil
	}
	return nil, fmt.Errorf("size %d is too big for the MMDB encoding", size)
}

func appendString(b []byte, s string) ([]byte, error) {
	b, err := appendControl(b, typeString, len(s))
	if err != nil {
		return nil, err
	}
	return append(b, s...), nil
}

// appendUint writes an unsigned integer of the given type (uint16, uint32 or
// uint64) in the fewest big-endian bytes; zero has no bytes.
func appendUint(b []byte, typ int, v uint64) ([]byte, error) {
	max := map[int]uint64{typeUint16: 1<<16 - 1, typeUint32: 1<<32 - 1, typeUint64: 1<<64 - 1}[typ]
	if max == 0 || v > max {
		return nil, fmt.Errorf("value %d does not fit type %d", v, typ)
	}
	var raw []byte
	for x := v; x > 0; x >>= 8 {
		raw = append([]byte{byte(x)}, raw...)
	}
	b, err := appendControl(b, typ, len(raw))
	if err != nil {
		return nil, err
	}
	return append(b, raw...), nil
}

func appendStringArray(b []byte, items []string) ([]byte, error) {
	b, err := appendControl(b, typeArray, len(items))
	if err != nil {
		return nil, err
	}
	for _, s := range items {
		if b, err = appendString(b, s); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// appendStringMap writes a map of strings, keys sorted (the output is stable).
func appendStringMap(b []byte, m map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b, err := appendControl(b, typeMap, len(keys))
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if b, err = appendString(b, k); err != nil {
			return nil, err
		}
		if b, err = appendString(b, m[k]); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// standardMetadata are the fields every MMDB has.
type standardMetadata struct {
	Major, Minor uint16
	BuildEpoch   uint64
	DatabaseType string
	Description  map[string]string
	IPVersion    uint16
	Languages    []string
	NodeCount    uint32
	RecordSize   uint16
}

// encodeMetadata is the metadata map as it follows the marker.
func encodeMetadata(m standardMetadata) ([]byte, error) {
	b, err := appendControl(nil, typeMap, 9)
	if err != nil {
		return nil, err
	}
	// Each step appends a key and its value; the first error ends the chain.
	step := func(key string, value func([]byte) ([]byte, error)) {
		if err != nil {
			return
		}
		if b, err = appendString(b, key); err != nil {
			return
		}
		b, err = value(b)
	}
	u := func(typ int, v uint64) func([]byte) ([]byte, error) {
		return func(b []byte) ([]byte, error) { return appendUint(b, typ, v) }
	}
	step("binary_format_major_version", u(typeUint16, uint64(m.Major)))
	step("binary_format_minor_version", u(typeUint16, uint64(m.Minor)))
	step("build_epoch", u(typeUint64, m.BuildEpoch))
	step("database_type", func(b []byte) ([]byte, error) { return appendString(b, m.DatabaseType) })
	step("description", func(b []byte) ([]byte, error) { return appendStringMap(b, m.Description) })
	step("ip_version", u(typeUint16, uint64(m.IPVersion)))
	step("languages", func(b []byte) ([]byte, error) { return appendStringArray(b, m.Languages) })
	step("node_count", u(typeUint32, uint64(m.NodeCount)))
	step("record_size", u(typeUint16, uint64(m.RecordSize)))
	return b, err
}
