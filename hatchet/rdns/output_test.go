package rdns

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Task outputs go to Hatchet: they hold counts and flags, never an IP, so the output
// structs have no field that could take one.
func TestOutputsAreCountsOnly(t *testing.T) {
	ip := regexp.MustCompile(`\d+\.\d+\.\d+\.\d+`)
	for _, out := range []any{
		PickOutput{Eligible: 5, Picked: 3, Visitors: 9},
		TraceOutput{Networks: 1, Targets: 3},
		NamesOutput{IPs: 4, OK: 2},
		LocateOutput{Traced: 2, Kinds: map[string]*KindStats{kindVisitor: {Traces: 1}}},
	} {
		data, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		json.Unmarshal(data, &fields)
		for name, v := range fields {
			switch v.(type) {
			case float64, bool, map[string]any:
			default:
				t.Errorf("%T field %s is %T", out, name, v)
			}
		}
		if ip.Match(data) {
			t.Errorf("%T holds an IP: %s", out, data)
		}
	}
}

// A failed statement is reported by its code: the message can quote a visitor IP.
func TestDBErrorHidesValues(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type inet: "5.6.7.8"`, Detail: "Key (ip)=(5.6.7.8)"}
	got := dbError("store traces", pgErr)
	if strings.Contains(got.Error(), "5.6.7.8") || !strings.Contains(got.Error(), "22P02") || !strings.Contains(got.Error(), "store traces") {
		t.Errorf("err = %v", got)
	}
	plain := errors.New("connection reset")
	if got := dbError("x", plain); !errors.Is(got, plain) {
		t.Errorf("err = %v", got)
	}
}
