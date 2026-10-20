package web

import (
	"io/fs"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestServesAssetsWithSecurityHeaders(t *testing.T) {
	cases := []struct {
		path, ctype string
		code        int
		contains    string
	}{
		{"/", "text/html", 200, `<script type="module" src="app.js">`},
		{"/app.js", "text/javascript", 200, "function h(tag"},
		{"/style.css", "text/css", 200, "--accent"},
		{"/index.html", "", 404, ""},
		{"/dashboard/app.js", "", 404, ""},
		{"/../embed.go", "", 404, ""},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			Handler().ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
			if w.Code != tc.code {
				t.Fatalf("code %d, want %d", w.Code, tc.code)
			}
			if tc.code != 200 {
				return
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.ctype) {
				t.Fatalf("content type %q", ct)
			}
			for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
				if w.Header().Get(h) == "" {
					t.Fatalf("missing %s", h)
				}
			}
			if !strings.Contains(w.Body.String(), tc.contains) {
				t.Fatalf("body does not contain %q", tc.contains)
			}
		})
	}
}

// TestNoHTMLInjection keeps the rule of app.js: data reaches the page as text nodes only.
// Traffic lines carry Host headers and paths chosen by anyone on the internet.
func TestNoHTMLInjection(t *testing.T) {
	js, err := fs.ReadFile(files, "dashboard/app.js")
	if err != nil {
		t.Fatal(err)
	}
	code := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(string(js), "") // the rule itself is in a comment
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "javascript:"} {
		if strings.Contains(code, bad) {
			t.Errorf("app.js uses %s", bad)
		}
	}
}

// TestCSPHasNoInlineScript: the CSP must forbid inline and external scripts.
func TestCSPHasNoInlineScript(t *testing.T) {
	if !strings.Contains(CSP, "script-src 'self';") || strings.Contains(CSP, "unsafe-eval") {
		t.Fatalf("CSP %q", CSP)
	}
	html, err := fs.ReadFile(files, "dashboard/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`<script(?:\s[^>]*)?>[^<]+</script>`).Match(html) || regexp.MustCompile(`\son[a-z]+=`).Match(html) {
		t.Fatal("index.html has an inline script or an inline event handler (blocked by the CSP)")
	}
}

// TestEveryIDExists: each $("id") in app.js names an element of index.html, so a renamed
// element fails here instead of in the browser.
func TestEveryIDExists(t *testing.T) {
	js, _ := fs.ReadFile(files, "dashboard/app.js")
	html, _ := fs.ReadFile(files, "dashboard/index.html")
	ids := map[string]bool{}
	for _, m := range regexp.MustCompile(`\sid="([^"]+)"`).FindAllStringSubmatch(string(html), -1) {
		ids[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`\$\("([^"]+)"\)`).FindAllStringSubmatch(string(js), -1) {
		if !ids[m[1]] {
			t.Errorf("app.js uses #%s, index.html has no such id", m[1])
		}
	}
	for _, tab := range regexp.MustCompile(`data-tab="([^"]+)"`).FindAllStringSubmatch(string(html), -1) {
		if !ids["tab-"+tab[1]] {
			t.Errorf("tab %s has no section #tab-%s", tab[1], tab[1])
		}
	}
}
