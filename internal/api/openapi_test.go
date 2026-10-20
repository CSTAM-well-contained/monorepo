package api

import (
	"os"
	"strings"
	"testing"
)

// TestOpenAPIListsEveryRoute keeps docs/official/openapi.yaml in step with the server:
// every route must appear under its path with its method, and with the right security.
func TestOpenAPIListsEveryRoute(t *testing.T) {
	raw, err := os.ReadFile("../../docs/official/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// path -> its block of text, up to the next path or the components section
	blocks := map[string]string{}
	var cur string
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "components:"):
			cur = ""
		case strings.HasPrefix(line, "  /") && strings.HasSuffix(line, ":"):
			cur = strings.TrimSuffix(strings.TrimSpace(line), ":")
		case cur != "":
			blocks[cur] += line + "\n"
		}
	}
	s := &Server{}
	for _, r := range s.routes() {
		method, path, _ := strings.Cut(r.pattern, " ")
		block, ok := blocks[path]
		if !ok {
			t.Errorf("openapi.yaml has no path %s", path)
			continue
		}
		op := "    " + strings.ToLower(method) + ":\n"
		i := strings.Index(block, op)
		if i < 0 {
			t.Errorf("openapi.yaml: %s has no %s operation", path, method)
			continue
		}
		rest := block[i+len(op):]
		if j := strings.Index(rest, "\n    "+"get:"); j >= 0 && method != "GET" {
			rest = rest[:j]
		}
		gwSec := strings.Contains(rest, "security: [{gateway: []}]")
		pubSec := strings.Contains(rest, "security: []")
		switch r.token {
		case gatewayOnly:
			if !gwSec {
				t.Errorf("openapi.yaml: %s must declare the gateway token", r.pattern)
			}
		case public:
			if !pubSec {
				t.Errorf("openapi.yaml: %s must declare no security", r.pattern)
			}
		default:
			if gwSec || pubSec {
				t.Errorf("openapi.yaml: %s must use the default admin token", r.pattern)
			}
		}
	}
	if n, want := countOps(blocks), len(s.routes()); n != want {
		t.Errorf("openapi.yaml has %d operations, the server %d", n, want)
	}
}

func countOps(blocks map[string]string) int {
	n := 0
	for _, b := range blocks {
		for _, m := range []string{"get", "post", "put", "patch", "delete"} {
			n += strings.Count(b, "\n    "+m+":\n")
			if strings.HasPrefix(b, "    "+m+":\n") {
				n++
			}
		}
	}
	return n
}
