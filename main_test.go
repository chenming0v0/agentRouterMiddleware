package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"agentrouter/internal/config"
)

type funcHandler func(http.ResponseWriter, *http.Request)

func (f funcHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) { f(w, r) }

// rootFixture builds a rootHandler whose three backends merely record which one
// handled the request, plus which headers the proxy side would have seen.
type rootFixture struct {
	h      *rootHandler
	hit    string
	proxed *http.Request
}

func newRootFixture() *rootFixture {
	f := &rootFixture{}
	f.h = &rootHandler{
		api:    funcHandler(func(_ http.ResponseWriter, _ *http.Request) { f.hit = "api" }),
		static: funcHandler(func(_ http.ResponseWriter, _ *http.Request) { f.hit = "static" }),
		proxy: funcHandler(func(_ http.ResponseWriter, r *http.Request) {
			f.hit = "proxy"
			f.proxed = r
		}),
	}
	return f
}

func TestRootHandlerDispatch(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   string
	}{
		{http.MethodGet, "/", "static"},
		{http.MethodHead, "/", "static"},
		{http.MethodGet, "/assets/app.js", "static"},
		{http.MethodGet, "/favicon.svg", "static"},
		{http.MethodGet, "/favicon.ico", "static"},
		{http.MethodPost, "/", "proxy"},
		{http.MethodGet, "/v1/models", "proxy"},
		{http.MethodPost, "/v1/chat/completions", "proxy"},
		{http.MethodGet, "/favicon.png", "proxy"},
		{http.MethodGet, "/api/config", "api"},
		{http.MethodPost, "/api/health", "api"},
		{http.MethodGet, "/api/logs/abc123", "api"},
		{http.MethodGet, "/api/logs/abc123/body/request", "api"},
		// Unknown /api paths are NOT admin: they must reach the proxy so that
		// OpenAI-compatible routes such as /api/chat keep working.
		{http.MethodGet, "/api", "proxy"},
		{http.MethodGet, "/api/", "proxy"},
		{http.MethodGet, "/api/nope", "proxy"},
		{http.MethodPost, "/api/chat", "proxy"},
		{http.MethodPost, "/api/chat/completions", "proxy"},
		{http.MethodGet, "/apix", "proxy"},
	}
	for _, tc := range tests {
		f := newRootFixture()
		f.h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, tc.path, nil))
		if f.hit != tc.want {
			t.Errorf("%s %s routed to %q, want %q", tc.method, tc.path, f.hit, tc.want)
		}
	}
}

// TestRootHandlerBusinessRouteDoesNotCheckAdminToken proves the business router
// is a pure pass-through: an Authorization value that happens to equal the
// admin token is forwarded to the provider, never inspected or rejected. Admin
// auth is enforced only by the management API handler.
func TestRootHandlerBusinessRouteDoesNotCheckAdminToken(t *testing.T) {
	f := newRootFixture()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer supersecret")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)

	if f.hit != "proxy" {
		t.Fatalf("business route routed to %q, want proxy", f.hit)
	}
	if f.proxed == nil {
		t.Fatal("proxy did not receive the request")
	}
	if got := f.proxed.Header.Get("Authorization"); got != "Bearer supersecret" {
		t.Fatalf("forwarded Authorization = %q, want the caller's key unchanged", got)
	}

	// A dedicated provider URL is likewise pass-through business traffic.
	f2 := newRootFixture()
	r2 := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", nil)
	r2.Header.Set("Authorization", "Bearer vendor-key")
	f2.h.ServeHTTP(httptest.NewRecorder(), r2)
	if f2.hit != "proxy" {
		t.Fatalf("dedicated route routed to %q, want proxy", f2.hit)
	}
}

func TestListenAddress(t *testing.T) {
	tests := []struct {
		explicit bool
		flagVal  string
		cfgVal   string
		want     string
	}{
		{true, ":9999", ":1111", ":9999"},
		{false, ":9999", ":1111", ":1111"},
		{false, ":9999", "", ":9999"},
		{true, ":9999", "", ":9999"},
	}
	for _, tc := range tests {
		if got := listenAddress(tc.explicit, tc.flagVal, tc.cfgVal); got != tc.want {
			t.Errorf("listenAddress(%v,%q,%q) = %q, want %q", tc.explicit, tc.flagVal, tc.cfgVal, got, tc.want)
		}
	}
}

func TestDefaultListenIsLoopback18851(t *testing.T) {
	if config.DefaultListen != "127.0.0.1:18851" {
		t.Fatalf("config.DefaultListen = %q, want 127.0.0.1:18851", config.DefaultListen)
	}
}

func TestAdminToken(t *testing.T) {
	t.Setenv("AGENTROUTER_ADMIN_TOKEN", "envtok")
	if got := adminToken("", false); got != "envtok" {
		t.Errorf("adminToken from env = %q, want envtok", got)
	}
	if got := adminToken("flagtok", true); got != "flagtok" {
		t.Errorf("adminToken from flag = %q, want flagtok", got)
	}
	if got := adminToken("", true); got != "" {
		t.Errorf("explicit empty flag = %q, want empty", got)
	}
}

func TestCheckAdminExposure(t *testing.T) {
	tests := []struct {
		addr  string
		token string
		want  bool // true means accepted
	}{
		{"127.0.0.1:18851", "", true},
		{"localhost:18851", "", true},
		{"[::1]:18851", "", true},
		{"0.0.0.0:18851", "", false},
		{":18851", "", false},
		{"192.168.1.10:18851", "", false},
		{":18851", "tok", true},
		{"0.0.0.0:18851", "tok", true},
	}
	for _, tc := range tests {
		err := checkAdminExposure(tc.addr, tc.token)
		if got := err == nil; got != tc.want {
			t.Errorf("checkAdminExposure(%q, token=%q) accepted=%v, want %v (err=%v)", tc.addr, tc.token, got, tc.want, err)
		}
	}
}

func TestIsLoopbackListen(t *testing.T) {
	tests := map[string]bool{
		"127.0.0.1:18851": true,
		"localhost:18851": true,
		"[::1]:18851":     true,
		"0.0.0.0:18851":   false,
		":18851":          false,
		"10.0.0.5:1":      false,
	}
	for addr, want := range tests {
		if got := isLoopbackListen(addr); got != want {
			t.Errorf("isLoopbackListen(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestStaticHandler(t *testing.T) {
	h, err := staticHandler("")
	if err != nil {
		t.Fatalf("staticHandler(embed): %v", err)
	}
	if h == nil {
		t.Fatal("staticHandler(embed) returned nil")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>disk</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	hd, err := staticHandler(dir)
	if err != nil {
		t.Fatalf("staticHandler(dir): %v", err)
	}
	rec := httptest.NewRecorder()
	hd.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("disk index status = %d, want 200", rec.Code)
	}
}
