package main

import (
	"bytes"
	"image/png"
	"regexp"
	"strings"
	"testing"
)

// Both colours come from style.css (--bg of the dark and of the light theme), one
// tag per scheme on every page, and the theme scripts set the tags by hand.
func TestThemeColorTags(t *testing.T) {
	bg := regexp.MustCompile(`--bg:(#[0-9a-f]{6})`).FindAllStringSubmatch(styleSrc, -1)
	if len(bg) != 3 || bg[1][1] != bg[2][1] {
		t.Fatalf("--bg values in style.css: %v", bg)
	}
	dark, light := bg[0][1], bg[1][1]
	s := fixtureServer(t)
	for _, target := range []string{"/", "/?203.0.113.9", "/download", "/status", "/as64500"} {
		b := get(s, "GET", target).Body.String()
		for _, x := range []string{
			`<meta name="theme-color" content="` + dark + `" media="(prefers-color-scheme: dark)">`,
			`<meta name="theme-color" content="` + light + `" media="(prefers-color-scheme: light)">`,
			`<link rel="apple-touch-icon" href="/apple-touch-icon.png">`,
		} {
			if !strings.Contains(b, x) {
				t.Errorf("%s lacks %s", target, x)
			}
		}
		// The tags come before the script that changes them.
		if i, j := strings.Index(b, `name="theme-color"`), strings.Index(b, "<script>"); i < 0 || j < i {
			t.Errorf("%s: theme-color tags after the first script", target)
		}
	}
	// The script's colours are the same two values; both scripts set the tags.
	if !strings.Contains(headJS, `"`+dark+`"`) || !strings.Contains(headJS, `"`+light+`"`) || !strings.Contains(headJS, "meta[name=theme-color]") {
		t.Error("head.js does not set the theme-color tags")
	}
	if !strings.Contains(headJS, "bar(t)") || !strings.Contains(themeJS, "bar(t)") {
		t.Error("a theme chosen by hand does not reach the tags")
	}
}

func TestTouchIcon(t *testing.T) {
	s := fixtureServer(t)
	for _, p := range []string{"/apple-touch-icon.png", "/apple-touch-icon-precomposed.png"} {
		w := get(s, "GET", p)
		h := w.Header()
		if w.Code != 200 || h.Get("Content-Type") != "image/png" || h.Get("Cache-Control") != "public, max-age=31536000, immutable" || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: %d %v", p, w.Code, h)
		}
		img, err := png.Decode(bytes.NewReader(w.Body.Bytes()))
		if err != nil || img.Bounds().Dx() != 180 || img.Bounds().Dy() != 180 {
			t.Errorf("%s: not a 180x180 PNG: %v", p, err)
		}
		if w.Body.Len() > 2048 {
			t.Errorf("%s is %d bytes", p, w.Body.Len())
		}
		// The corner is the page background, the middle the accent.
		if r, g, b, _ := img.At(2, 2).RGBA(); r>>8 != 0x0a || g>>8 != 0x0c || b>>8 != 0x0f {
			t.Errorf("%s: corner colour %x %x %x", p, r>>8, g>>8, b>>8)
		}
		if r, g, b, _ := img.At(90, 90).RGBA(); r>>8 != 0xff || g>>8 != 0x7d || b>>8 != 0x1f {
			t.Errorf("%s: centre colour %x %x %x", p, r>>8, g>>8, b>>8)
		}
	}
}

// scriptText keeps no comment line and no leading space; every script still has
// balanced brackets (a rough check: the text is never run by the tests).
func TestScriptTextProcessing(t *testing.T) {
	if got := scriptText("x.js", "// first\n// second\nvar a = 1; // kept? no: only whole lines go\n\n  // note\n  if (a) {\n    a = \"//x\";\n  }\n"); got != "var a = 1; // kept? no: only whole lines go\nif (a) {\na = \"//x\";\n}" {
		t.Errorf("scriptText = %q", got)
	}
	for name, js := range map[string]string{"head": headJS, "theme": themeJS, "fill": fillJS, "resolver": resolverJS, "panel": panelJS, "share": shareJS} {
		if regexp.MustCompile(`(?m)^\s`).MatchString(js) || regexp.MustCompile(`(?m)^//`).MatchString(js) || strings.HasPrefix(js, "\n") || strings.HasSuffix(js, "\n") || strings.Contains(js, "\n\n") {
			t.Errorf("%s.js: a comment line, white space or an empty line is left", name)
		}
		if regexp.MustCompile(`\s//`).MatchString(js) {
			// A trailing comment would be in the page; the sources keep comments on lines of their own.
			t.Errorf("%s.js: a comment follows code", name)
		}
		if err := balanced(js); err != "" {
			t.Errorf("%s.js: %s", name, err)
		}
	}
}

// balanced checks (), [] and {} outside "..." strings and regular expressions
// written as /.../ after one of ( , = : ? ! & | { ; or a return.
func balanced(js string) string {
	var stack []byte
	pair := map[byte]byte{')': '(', ']': '[', '}': '{'}
	for i := 0; i < len(js); i++ {
		c := js[i]
		switch {
		case c == '"':
			for i++; i < len(js) && js[i] != '"'; i++ {
				if js[i] == '\\' {
					i++
				}
			}
		case c == '/' && i > 0 && strings.ContainsRune("(,=:?!&|{;", rune(strings.TrimRight(js[:i], " ")[len(strings.TrimRight(js[:i], " "))-1])):
			inClass := false
			for i++; i < len(js) && (js[i] != '/' || inClass); i++ {
				switch js[i] {
				case '\\':
					i++
				case '[':
					inClass = true
				case ']':
					inClass = false
				}
			}
		case c == '(' || c == '[' || c == '{':
			stack = append(stack, c)
		case c == ')' || c == ']' || c == '}':
			if len(stack) == 0 || stack[len(stack)-1] != pair[c] {
				return "unbalanced " + string(c) + " at " + js[max(0, i-20):i+1]
			}
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) > 0 {
		return "unclosed " + string(stack[len(stack)-1])
	}
	return ""
}

func TestBalanced(t *testing.T) {
	if balanced(`a(b[1]) { x = /[)]\//.test("(") }`) != "" || balanced("a(") == "" || balanced("a)") == "" || balanced("{]") == "" {
		t.Error("balanced")
	}
}

func TestSlashKeyScript(t *testing.T) {
	for _, x := range []string{`e.key == "/"`, "preventDefault()", ".focus()", ".select()", "e.ctrlKey", "e.metaKey", "e.altKey", "INPUT|TEXTAREA|SELECT", "activeElement", ".lookup input"} {
		if !strings.Contains(themeJS, x) {
			t.Errorf("themeJS lacks %s", x)
		}
	}
	// Every page that has the theme script has the lookup box the key goes to.
	s := fixtureServer(t)
	for _, target := range []string{"/", "/download", "/status", "/as64500"} {
		if b := get(s, "GET", target).Body.String(); !strings.Contains(b, "<script>"+themeJS+"</script>") || !strings.Contains(b, `<form class="lookup"`) {
			t.Errorf("%s lacks the theme script or the lookup form", target)
		}
	}
}

// The other family's row ends in "none" (faint) when the script gets no usable
// answer; the dash stays while it waits and without JavaScript.
func TestFillNoneState(t *testing.T) {
	for _, x := range []string{`"none"`, `classList.add("none")`, ".catch(no)", "else no()"} {
		if !strings.Contains(fillJS, x) {
			t.Errorf("fillJS lacks %s", x)
		}
	}
	if !strings.Contains(liveCSS, "dd.none{color:var(--faint)}") {
		t.Error("no faint style for the none state")
	}
	// The row's reserved height rules do not depend on the text.
	if !strings.Contains(liveCSS, "dd.o6,dd.od{min-height:2lh}") {
		t.Error("the reserved height of the IPv6 row is gone")
	}
	s := fillServer(t, u4, u6)
	if b := fromPeer(s, "/", "127.0.0.1:1", "X-Real-IP", "203.0.113.9").Body.String(); !strings.Contains(b, `id="other"`) || !strings.Contains(b, `<span class="v">—</span>`) {
		t.Error("the row does not start as a dash")
	}
}
