package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testFS() fs.FS {
	return fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<html>app</html>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")},
		"favicon.svg":   &fstest.MapFile{Data: []byte("<svg/>")},
	}
}

func get(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestHandlerServesIndexAndAssets(t *testing.T) {
	h := NewHandler(testFS())

	rec := get(t, h, http.MethodGet, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "app") {
		t.Fatalf("GET / body = %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("GET / content-type = %q", ct)
	}

	rec = get(t, h, http.MethodGet, "/assets/app.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET asset status = %d, want 200", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("asset Cache-Control = %q, want immutable", cc)
	}

	if rec := get(t, h, http.MethodHead, "/"); rec.Code != http.StatusOK {
		t.Fatalf("HEAD / status = %d, want 200", rec.Code)
	}
}

func TestHandlerSPAFallback(t *testing.T) {
	h := NewHandler(testFS())
	rec := get(t, h, http.MethodGet, "/dashboard/settings")
	if rec.Code != http.StatusOK {
		t.Fatalf("SPA route status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "app") {
		t.Fatalf("SPA route body = %q", rec.Body.String())
	}
}

func TestHandlerMissingAndMethod(t *testing.T) {
	h := NewHandler(testFS())
	if rec := get(t, h, http.MethodGet, "/missing.txt"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing file status = %d, want 404", rec.Code)
	}
	if rec := get(t, h, http.MethodPost, "/"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST / status = %d, want 405", rec.Code)
	}
	if rec := get(t, h, http.MethodGet, "/api/config"); rec.Code != http.StatusNotFound {
		t.Fatalf("api path through static status = %d, want 404", rec.Code)
	}
}
