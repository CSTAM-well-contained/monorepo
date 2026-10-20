// Package web holds the admin dashboard (plain HTML, CSS and an ES module, no build step)
// and serves it from cpd. All data comes from the REST API with the admin token.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed dashboard
var files embed.FS

// assets are the only paths served; anything else is a 404.
var assets = map[string]string{
	"/":          "dashboard/index.html",
	"/app.js":    "dashboard/app.js",
	"/style.css": "dashboard/style.css",
}

var types = map[string]string{
	"/":          "text/html; charset=utf-8",
	"/app.js":    "text/javascript; charset=utf-8",
	"/style.css": "text/css; charset=utf-8",
}

// CSP allows only this origin: no inline script or style attribute injection from data,
// no external host, no framing.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; " +
	"img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// Handler serves the dashboard with security headers.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := assets[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		data, err := fs.ReadFile(files, name)
		if err != nil {
			http.Error(w, "missing asset", http.StatusInternalServerError)
			return
		}
		hd := w.Header()
		hd.Set("Content-Type", types[r.URL.Path])
		hd.Set("Content-Security-Policy", CSP)
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cache-Control", "no-cache")
		_, _ = w.Write(data)
	})
}
