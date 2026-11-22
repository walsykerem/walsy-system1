package main

import (
	"os"
	"strings"
	"testing"
)

// cssBlock returns the declaration body of the first rule whose selector list
// contains sel, so assertions cannot be satisfied by an unrelated rule.
func cssBlock(t *testing.T, css, sel string) string {
	t.Helper()
	for _, chunk := range strings.Split(css, "}") {
		head, body, ok := strings.Cut(chunk, "{")
		if !ok {
			continue
		}
		for _, s := range strings.Split(head, ",") {
			if strings.TrimSpace(s) == sel {
				return body
			}
		}
	}
	t.Fatalf("web/style.css has no rule for %q", sel)
	return ""
}

// The add-provider form must keep one left/right edge: an overlaid form and a
// bordered hint indented its text 16px away from the field labels. Both are
// layout bugs the user reported, so pin the CSS that prevents them.
func TestAddProviderFormAlignment(t *testing.T) {
	raw, err := os.ReadFile("web/style.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(raw)
	for {
		start := strings.Index(css, "/*")
		end := strings.Index(css, "*/")
		if start < 0 || end < start {
			break
		}
		css = css[:start] + css[end+2:]
	}

	form := cssBlock(t, css, ".add-provider > form")
	if !strings.Contains(form, "position: absolute") || !strings.Contains(form, "inset: 100% 0 auto 0") {
		t.Fatalf(".add-provider > form must overlay so opening does not move BUILD TARGET: %q", form)
	}
	if !strings.Contains(form, "max-height: 80vh") || !strings.Contains(form, "overflow-y: auto") {
		t.Fatalf(".add-provider > form lost its spacing below the summary: %q", form)
	}

	hint := cssBlock(t, css, ".add-provider > form > .hint")
	for _, want := range []string{"padding: 0", "border: 0", "background: none", "margin: 0"} {
		if !strings.Contains(hint, want) {
			t.Fatalf(".add-provider hint must not indent its text (%q missing): %q", want, hint)
		}
	}

	if !strings.Contains(css, ".btn {") || !strings.Contains(css, "display: inline-flex") {
		t.Fatal(".btn base rule changed; SAVE PROVIDER may stop stretching to the field width")
	}
}

// A rewrite must not silently drop the fixed markup or the form's single
// accessible name.
func TestAddProviderMarkup(t *testing.T) {
	raw, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	for _, want := range []string{
		"- ADD PROVIDER",
		"SAVE PROVIDER",
		`aria-label="Add provider"`,
		`<span class="add-open" aria-hidden="true">`,
		`<span class="add-close" aria-hidden="true">`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("web/index.html missing %q", want)
		}
	}
}
