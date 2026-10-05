package console

import (
	"net/http"
	"strings"
)

// AllowedMethods reports the HTTP methods the console serves on the given
// path, relative to the console mount point. It mirrors the ServeHTTP
// switch so the CORS layer can advertise exactly the methods each route
// serves; unknown paths return nil.
func AllowedMethods(path string) []string {
	switch path {
	case "/session/nonce", "/session/verify", "/session/secret":
		return []string{http.MethodPost}
	case "/session/signout":
		return []string{http.MethodPost}
	case "/keys":
		return []string{http.MethodGet, http.MethodPost}
	case "/account", "/usage", "/funding", "/chains", "/market", "/models", "/stream", "/attestation", "/deposit-target":
		return []string{http.MethodGet}
	case "/listings":
		return []string{http.MethodGet, http.MethodPost}
	}
	if strings.HasPrefix(path, "/listings/") {
		return []string{http.MethodPost}
	}
	if strings.HasPrefix(path, "/keys/") {
		return []string{http.MethodDelete}
	}
	if _, ok := parseUsageProofPath(path); ok {
		return []string{http.MethodGet}
	}
	return nil
}
