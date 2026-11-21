package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"testing"
)

// TestGuessSourcesMatchBuilder reads the source text of ../mmdbbuilder (a plain
// file read, no import) so that the site's list of guess sources cannot drift.
func TestGuessSourcesMatchBuilder(t *testing.T) {
	files, _ := filepath.Glob("../mmdbbuilder/*.go")
	var text string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		text += string(b) + "\n"
	}
	m := regexp.MustCompile(`(?m)^var guessSources = \[\]string\{([^}]*)\}`).FindStringSubmatch(text)
	if m == nil {
		t.Fatal("guessSources not found in mmdbbuilder")
	}
	var want []string
	for _, id := range regexp.MustCompile(`\w+`).FindAllString(m[1], -1) {
		v := regexp.MustCompile(`(?m)^\s*(?:const )?` + id + `\s*=\s*"([^"]+)"`).FindStringSubmatch(text)
		if v == nil {
			t.Fatalf("constant %s not found", id)
		}
		want = append(want, v[1])
	}
	var got []string
	for k := range guessSources {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("site %v, builder %v", got, want)
	}
}
