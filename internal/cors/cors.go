// Package cors adds an opt-in CORS layer in front of an existing handler
// tree. slotfared normally serves same-origin through the console's dev proxy
// and sends no CORS headers at all; when origins are configured, requests
// carrying one of the listed Origins get the standard CORS response headers,
// preflight OPTIONS included. Origins are exact matches: no wildcards, no
// substring, no case folding.
package cors

import (
	"net/http"
	"strings"
)

// allowedRequestHeaders is the fixed list of request headers a browser may
// send on a cross-origin call to slotfared: the Slotfare key rides in
// Authorization and the RPC/inference bodies are JSON. It is never derived
// per route.
const allowedRequestHeaders = "Authorization, Content-Type"

// exposedResponseHeaders is the fixed list of response headers a browser
// may read on a cross-origin call to slotfared: the slotfare charge, balance
// and receipt headers, which a browser client cannot see unless they are
// exposed.
const exposedResponseHeaders = "x-slotfare-credits-charged,x-slotfare-balance,x-slotfare-receipt"

// Wrap returns next with CORS response headers for requests whose Origin
// header is one of origins. methods resolves the path a preflight asks
// about to the HTTP methods that path actually serves; an empty result
// means the path serves nothing, and the preflight passes through to next
// with no CORS headers. An empty origins slice turns CORS off entirely:
// Wrap returns next itself, so no header logic runs at all.
func Wrap(next http.Handler, origins []string, methods func(r *http.Request) []string) http.Handler {
	if len(origins) == 0 {
		return next
	}
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if !allowed[origin] {
			next.ServeHTTP(w, r)
			return
		}
		// Preflight: OPTIONS plus the request-method a browser always
		// sends. A bare OPTIONS without it is not a preflight and passes
		// through to next, unchanged by this layer.
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			served := methods(r)
			if len(served) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Methods", strings.Join(served, ","))
			h.Set("Access-Control-Allow-Headers", allowedRequestHeaders)
			h.Set("Access-Control-Max-Age", "86400")
			h.Add("Vary", "Origin")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Simple or actual cross-origin request: the headers are set
		// before next writes anything, so they survive on every status.
		// No credentials header is ever sent: the API authenticates via
		// Authorization, not cookies.
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Expose-Headers", exposedResponseHeaders)
		h.Add("Vary", "Origin")
		next.ServeHTTP(w, r)
	})
}
