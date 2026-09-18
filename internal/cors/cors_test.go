package cors

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// inner is a handler that records every call and, on /known, answers a
// method not allowed for OPTIONS the way the console handler does today.
func inner() (h http.Handler, calls *[]string) {
	logged := []string{}
	calls = &logged
	h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls = append(*calls, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodOptions && r.URL.Path == "/known" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return h, &logged
}

func listedMethods(r *http.Request) []string {
	if r.URL.Path == "/known" {
		return []string{http.MethodGet, http.MethodPost}
	}
	return nil
}

func TestPreflightFromListedOrigin(t *testing.T) {
	h, calls := inner()
	wrap := Wrap(h, []string{"https://console.example"}, listedMethods)

	req := httptest.NewRequest(http.MethodOptions, "http://api.example/known", nil)
	req.Header.Set("Origin", "https://console.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	rr := httptest.NewRecorder()
	wrap.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rr.Code)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://console.example" {
		t.Errorf("Allow-Origin = %q", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Methods"); got != "GET,POST" {
		t.Errorf("Allow-Methods = %q, want GET,POST", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Headers"); got != "Authorization, Content-Type" {
		t.Errorf("Allow-Headers = %q", got)
	}
	if got := rr.Header().Get("Access-Control-Max-Age"); got != "86400" {
		t.Errorf("Max-Age = %q", got)
	}
	if got := rr.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("credentials header must never be set, got %q", got)
	}
	if len(*calls) != 0 {
		t.Errorf("inner handler must not run on preflight, calls = %v", *calls)
	}
}

func TestSimpleRequestFromListedOrigin(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		h, calls := inner()
		wrap := Wrap(h, []string{"https://console.example"}, listedMethods)

		req := httptest.NewRequest(method, "http://api.example/known", nil)
		req.Header.Set("Origin", "https://console.example")
		rr := httptest.NewRecorder()
		wrap.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", method, rr.Code)
		}
		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://console.example" {
			t.Errorf("%s Allow-Origin = %q", method, got)
		}
		if got := rr.Header().Get("Vary"); got != "Origin" {
			t.Errorf("%s Vary = %q", method, got)
		}
		if len(*calls) != 1 {
			t.Errorf("%s inner handler must run, calls = %v", method, *calls)
		}
	}
}

// The actual cross-origin response exposes the slotfare headers, so a
// browser client can read the charge and balance it carries.
func TestSimpleRequestExposesSlotfareHeaders(t *testing.T) {
	h, calls := inner()
	wrap := Wrap(h, []string{"https://console.example"}, listedMethods)

	req := httptest.NewRequest(http.MethodGet, "http://api.example/known", nil)
	req.Header.Set("Origin", "https://console.example")
	rr := httptest.NewRecorder()
	wrap.ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Expose-Headers"); got != "x-slotfare-credits-charged,x-slotfare-balance,x-slotfare-receipt" {
		t.Errorf("Expose-Headers = %q, want the slotfare headers", got)
	}
	if len(*calls) != 1 {
		t.Errorf("inner handler must run, calls = %v", *calls)
	}
}

// An unlisted origin gets the passthrough without the expose header: the
// slotfare headers stay unreadable cross-origin for origins slotfared does not
// serve.
func TestUnlistedOriginExposesNoSlotfareHeaders(t *testing.T) {
	h, calls := inner()
	wrap := Wrap(h, []string{"https://console.example"}, listedMethods)

	req := httptest.NewRequest(http.MethodGet, "http://api.example/known", nil)
	req.Header.Set("Origin", "https://other.example")
	rr := httptest.NewRecorder()
	wrap.ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Expose-Headers"); got != "" {
		t.Errorf("unlisted origin Expose-Headers = %q, want absent", got)
	}
	if len(*calls) != 1 {
		t.Errorf("inner handler must run, calls = %v", *calls)
	}
}

func TestUnlistedOriginGetsNoCORSHeaders(t *testing.T) {
	h, calls := inner()
	wrap := Wrap(h, []string{"https://console.example"}, listedMethods)

	// Preflight: passes through untouched.
	req := httptest.NewRequest(http.MethodOptions, "http://api.example/known", nil)
	req.Header.Set("Origin", "https://other.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	rr := httptest.NewRecorder()
	wrap.ServeHTTP(rr, req)
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("unlisted preflight Allow-Origin = %q", got)
	}
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("unlisted preflight status = %d, want the inner 405", rr.Code)
	}

	// Simple request: passes through untouched.
	req = httptest.NewRequest(http.MethodGet, "http://api.example/known", nil)
	req.Header.Set("Origin", "https://other.example")
	rr = httptest.NewRecorder()
	wrap.ServeHTTP(rr, req)
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("unlisted simple Allow-Origin = %q", got)
	}
	if len(*calls) != 2 {
		t.Errorf("both unlisted requests must reach the inner handler, calls = %v", *calls)
	}
}

func TestNoOriginHeaderGetsNoCORSHeaders(t *testing.T) {
	h, calls := inner()
	wrap := Wrap(h, []string{"https://console.example"}, listedMethods)

	req := httptest.NewRequest(http.MethodGet, "http://api.example/known", nil)
	rr := httptest.NewRecorder()
	wrap.ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("same-origin/curl Allow-Origin = %q", got)
	}
	if got := rr.Header().Get("Vary"); got != "" {
		t.Errorf("same-origin/curl Vary = %q", got)
	}
	if len(*calls) != 1 {
		t.Errorf("inner handler must run, calls = %v", *calls)
	}
}

func TestListedOriginUnknownPathGetsNoCORSHeaders(t *testing.T) {
	h, calls := inner()
	wrap := Wrap(h, []string{"https://console.example"}, listedMethods)

	req := httptest.NewRequest(http.MethodOptions, "http://api.example/unknown", nil)
	req.Header.Set("Origin", "https://console.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	rr := httptest.NewRecorder()
	wrap.ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("unknown-path Allow-Origin = %q", got)
	}
	if len(*calls) != 1 {
		t.Errorf("request must pass through, calls = %v", *calls)
	}
}

func TestEmptyOriginsIsInert(t *testing.T) {
	h, calls := inner()
	wrap := Wrap(h, nil, listedMethods)

	// OPTIONS on a listed-looking path behaves exactly as it does today:
	// the inner handler's 405, with no CORS headers.
	req := httptest.NewRequest(http.MethodOptions, "http://api.example/known", nil)
	req.Header.Set("Origin", "https://console.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	rr := httptest.NewRecorder()
	wrap.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want the inner 405", rr.Code)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Methods"); got != "" {
		t.Errorf("Allow-Methods = %q", got)
	}
	if len(*calls) != 1 {
		t.Errorf("inner handler must run, calls = %v", *calls)
	}
}
