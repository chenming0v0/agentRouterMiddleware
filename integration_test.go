package main

// Cross-module integration tests. They wire the real root dispatcher, admin API,
// live config store, retrying proxy and persistent log store together against a
// local fake upstream, so a break in any seam (routing, auth, masking, retry,
// capture, persistence) is caught. Every auxiliary listener uses httptest's
// random ephemeral port; no test ever binds a fixed port such as 8080.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentrouter/internal/api"
	"agentrouter/internal/config"
	"agentrouter/internal/proxy"
	"agentrouter/internal/store"
)

const (
	integrationAdminToken = "integration-admin-token"
	integrationClientCred = "Bearer integration-client-cred"
	integrationSecret     = "Bearer upstream-real-secret"
	integrationMarker     = "transient-body-only"
	// integrationBodyBytes keeps every payload comfortably above 1 MiB.
	integrationBodyBytes = 2 << 20
)

// integrationGateway is a real rootHandler served on an ephemeral port.
type integrationGateway struct {
	ts   *httptest.Server
	cfg  *config.Store
	logs *store.LogStore
}

func startIntegrationGateway(t *testing.T, cfgPath, logDir, token string) *integrationGateway {
	t.Helper()
	cs, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load(%s): %v", cfgPath, err)
	}
	ls, err := store.NewPersistentLogStore(config.DefaultLogLimit, logDir)
	if err != nil {
		t.Fatalf("store.NewPersistentLogStore(%s): %v", logDir, err)
	}
	root := &rootHandler{
		api: api.New(cs, ls, token),
		static: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<!doctype html><title>stub</title>"))
		}),
		proxy: proxy.New(cs, ls),
	}
	return &integrationGateway{ts: httptest.NewServer(root), cfg: cs, logs: ls}
}

// configureUpstream installs a single enabled local upstream over the admin API,
// using a defaults-based config so every required field is valid.
func configureUpstream(t *testing.T, gw *integrationGateway, upstreamURL string) {
	t.Helper()
	cfg := config.Default()
	// A generous sniff window keeps the 2 MiB body-regex attempts deterministic.
	cfg.BodySniffTimeoutMS = 1000
	cfg.Upstreams = []config.Upstream{{
		ID:        "u_local",
		Name:      "local-fake",
		BaseURL:   upstreamURL,
		Enabled:   true,
		Weight:    100,
		TimeoutMS: 10000,
		// Empty globs match every path/model so /api/chat and /v1 both reach it.
		Headers: map[string]string{"X-Fake-Auth": integrationSecret},
	}}
	cfg.Retry = config.Retry{
		MaxAttempts: 3,
		BaseDelayMS: 0,
		MaxDelayMS:  0,
		Jitter:      false,
		StatusCodes: []int{http.StatusTooManyRequests},
		BodyRegexes: []string{integrationMarker},
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	resp := doAdmin(t, gw, http.MethodPut, "/api/config", integrationAdminToken, raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("configure PUT status = %d, want 200 (body %s)", resp.StatusCode, truncate(body))
	}
}

// doAdmin performs an authenticated (or, with an empty token, unauthenticated)
// admin request against the gateway.
func doAdmin(t *testing.T, gw *integrationGateway, method, path, token string, body []byte) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, gw.ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("new %s %s: %v", method, path, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := gw.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// fetchAdminBody downloads a raw log body through the admin API and checks the
// no-store / nosniff hardening while it is at it.
func fetchAdminBody(t *testing.T, gw *integrationGateway, path string) []byte {
	t.Helper()
	resp := doAdmin(t, gw, http.MethodGet, path, integrationAdminToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s status = %d, want 200 (body %s)", path, resp.StatusCode, truncate(body))
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("GET %s Cache-Control = %q, want no-store", path, cc)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("GET %s missing X-Content-Type-Options: nosniff", path)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

type logsEnvelope struct {
	Logs  []store.LogEntry `json:"logs"`
	Total int              `json:"total"`
}

// waitForLog polls the logs API until the request shows up, then returns it.
func waitForLog(t *testing.T, gw *integrationGateway, method, path string) store.LogEntry {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp := doAdmin(t, gw, http.MethodGet, "/api/logs?limit=50", integrationAdminToken, nil)
		var env logsEnvelope
		err := json.NewDecoder(resp.Body).Decode(&env)
		resp.Body.Close()
		if err == nil {
			for _, e := range env.Logs {
				if e.Method == method && e.Path == path {
					return e
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("log entry for %s %s never appeared via the admin API", method, path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// postGateway forwards one business request through the proxy path.
func postGateway(t *testing.T, gw *integrationGateway, path string, body []byte) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, gw.ts.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new POST %s: %v", path, err)
	}
	req.Header.Set("Authorization", integrationClientCred)
	req.Header.Set("Content-Type", "application/json")
	resp, err := gw.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read POST %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s status = %d, want 200 (body %s)", path, resp.StatusCode, truncate(data))
	}
	return data
}

type upstreamCall struct {
	method   string
	path     string
	query    string
	auth     string
	fakeAuth string
	bodyHash string
}

// fakeUpstream scripts the first request as retryable attempts and answers
// every later request successfully.
type fakeUpstream struct {
	mu      sync.Mutex
	calls   []upstreamCall
	first   []byte
	second  []byte
	success []byte
}

func newFakeUpstream(first, second, success []byte) *fakeUpstream {
	return &fakeUpstream{first: first, second: second, success: success}
}

func (u *fakeUpstream) handler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(body)
	u.mu.Lock()
	u.calls = append(u.calls, upstreamCall{
		method:   r.Method,
		path:     r.URL.Path,
		query:    r.URL.RawQuery,
		auth:     r.Header.Get("Authorization"),
		fakeAuth: r.Header.Get("X-Fake-Auth"),
		bodyHash: hex.EncodeToString(sum[:]),
	})
	n := len(u.calls)
	u.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch n {
	case 1:
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write(u.first)
	case 2:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(u.second)
	default:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(u.success)
	}
}

func (u *fakeUpstream) snapshot() []upstreamCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]upstreamCall(nil), u.calls...)
}

// paddedJSON builds a syntactically valid JSON document of exactly size bytes
// whose payload carries marker. Values are reused by first/second/success.
func paddedJSON(prefix, marker, suffix string, size int) []byte {
	head := prefix + marker
	if size < len(head)+len(suffix) {
		size = len(head) + len(suffix)
	}
	out := make([]byte, 0, size)
	out = append(out, head...)
	out = append(out, bytes.Repeat([]byte("z"), size-len(head)-len(suffix))...)
	out = append(out, suffix...)
	return out
}

func paddedRequest(size int) []byte {
	return paddedJSON(`{"model":"test-model","messages":[{"role":"user","content":"`, "pad", `"}]}`, size)
}

func paddedError(marker string, size int) []byte {
	return paddedJSON(`{"error":"`, marker+`","detail":"`, `"}`, size)
}

func paddedSuccess(size int) []byte {
	return paddedJSON(`{"id":"ok","object":"chat.completion","detail":"`, "final", `"}`, size)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func truncate(b []byte) string {
	const max = 300
	if len(b) > max {
		return fmt.Sprintf("%s... (%d bytes)", b[:max], len(b))
	}
	return string(b)
}

// TestGatewayIntegrationRetryBodiesAndAuth drives one request through the real
// dispatcher and asserts the retry sequence, request fidelity, capture, masking
// and authentication all line up across modules.
func TestGatewayIntegrationRetryBodiesAndAuth(t *testing.T) {
	requestBody := paddedRequest(integrationBodyBytes)
	firstBody := paddedError("attempt-one-error", integrationBodyBytes)
	secondBody := paddedError(integrationMarker, integrationBodyBytes)
	successBody := paddedSuccess(integrationBodyBytes)

	up := newFakeUpstream(firstBody, secondBody, successBody)
	upSrv := httptest.NewServer(http.HandlerFunc(up.handler))
	defer upSrv.Close()

	dir := t.TempDir()
	gw := startIntegrationGateway(t,
		filepath.Join(dir, "config.json"), filepath.Join(dir, "logs"), integrationAdminToken)
	defer gw.ts.Close()
	configureUpstream(t, gw, upSrv.URL)

	// The real root dispatcher must route /v1 to the proxy and the upstream
	// must retry 429 then a body-regex match before delivering success.
	respBody := postGateway(t, gw, "/v1/chat/completions?stream=false&trace=abc", requestBody)
	if !bytes.Equal(respBody, successBody) {
		t.Fatalf("final body mismatch: got %d bytes want %d", len(respBody), len(successBody))
	}

	// Exactly three attempts, each byte-identical on the wire.
	calls := up.snapshot()
	if len(calls) != 3 {
		t.Fatalf("upstream calls = %d, want 3", len(calls))
	}
	wantHash := sha256Hex(requestBody)
	for i, c := range calls {
		if c.method != http.MethodPost {
			t.Fatalf("attempt %d method = %q, want POST", i+1, c.method)
		}
		if c.path != "/v1/chat/completions" {
			t.Fatalf("attempt %d path = %q", i+1, c.path)
		}
		if c.query != "stream=false&trace=abc" {
			t.Fatalf("attempt %d query = %q", i+1, c.query)
		}
		if c.auth != integrationClientCred {
			t.Fatalf("attempt %d Authorization = %q, want client credential", i+1, c.auth)
		}
		if c.fakeAuth != integrationSecret {
			t.Fatalf("attempt %d X-Fake-Auth = %q, want configured secret", i+1, c.fakeAuth)
		}
		if c.bodyHash != wantHash {
			t.Fatalf("attempt %d body hash = %s, want %s", i+1, c.bodyHash, wantHash)
		}
	}

	entry := waitForLog(t, gw, http.MethodPost, "/v1/chat/completions")
	if entry.Status != http.StatusOK || !entry.Retried || entry.AttemptCount != 3 {
		t.Fatalf("entry status=%d retried=%v attempt_count=%d, want 200/true/3",
			entry.Status, entry.Retried, entry.AttemptCount)
	}
	if len(entry.Attempts) != 3 {
		t.Fatalf("entry attempts = %d, want 3", len(entry.Attempts))
	}
	if entry.Attempts[0].Status != http.StatusTooManyRequests ||
		!strings.Contains(entry.Attempts[0].RetryReason, "status 429") {
		t.Fatalf("attempt 1 = %+v, want status 429 reason", entry.Attempts[0])
	}
	if entry.Attempts[1].Status != http.StatusOK ||
		!strings.Contains(entry.Attempts[1].RetryReason, "body matched") {
		t.Fatalf("attempt 2 = %+v, want body-regex reason", entry.Attempts[1])
	}
	if entry.Attempts[2].Status != http.StatusOK || entry.Attempts[2].RetryReason != "" {
		t.Fatalf("attempt 3 = %+v, want clean success", entry.Attempts[2])
	}
	if entry.ResponseBodyPart != "attempt-3" {
		t.Fatalf("response body part = %q, want attempt-3", entry.ResponseBodyPart)
	}
	if !entry.RequestBodyComplete || !entry.ResponseBodyComplete {
		t.Fatalf("capture completeness request=%v response=%v, want true/true",
			entry.RequestBodyComplete, entry.ResponseBodyComplete)
	}

	// Raw body downloads must be byte-exact, including NUL/high bytes.
	id := entry.ID
	if got := fetchAdminBody(t, gw, "/api/logs/"+id+"/body/request"); !bytes.Equal(got, requestBody) {
		t.Fatalf("request body download mismatch: got %d bytes hash %s, want hash %s",
			len(got), sha256Hex(got), sha256Hex(requestBody))
	}
	if got := fetchAdminBody(t, gw, "/api/logs/"+id+"/body/attempt-1"); !bytes.Equal(got, firstBody) {
		t.Fatalf("attempt-1 download mismatch: hash %s want %s", sha256Hex(got), sha256Hex(firstBody))
	}
	if got := fetchAdminBody(t, gw, "/api/logs/"+id+"/body/attempt-2"); !bytes.Equal(got, secondBody) {
		t.Fatalf("attempt-2 download mismatch: hash %s want %s", sha256Hex(got), sha256Hex(secondBody))
	}
	// "response" aliases the final attempt.
	if got := fetchAdminBody(t, gw, "/api/logs/"+id+"/body/response"); !bytes.Equal(got, successBody) {
		t.Fatalf("response alias mismatch: hash %s want %s", sha256Hex(got), sha256Hex(successBody))
	}
	if got := fetchAdminBody(t, gw, "/api/logs/"+id+"/body/attempt-3"); !bytes.Equal(got, successBody) {
		t.Fatalf("attempt-3 download mismatch: hash %s want %s", sha256Hex(got), sha256Hex(successBody))
	}

	// GET config must mask the configured header secret.
	resp := doAdmin(t, gw, http.MethodGet, "/api/config", integrationAdminToken, nil)
	rawConfig, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if bytes.Contains(rawConfig, []byte(integrationSecret)) {
		t.Fatal("GET /api/config leaked the real upstream secret")
	}
	var masked config.Config
	if err := json.Unmarshal(rawConfig, &masked); err != nil {
		t.Fatalf("decode masked config: %v", err)
	}
	gotMasked := false
	for _, u := range masked.Upstreams {
		if u.ID != "u_local" {
			continue
		}
		gotMasked = u.Headers["X-Fake-Auth"] == "***"
	}
	if !gotMasked {
		t.Fatalf("masked config = %s, want X-Fake-Auth ***", truncate(rawConfig))
	}

	// Raw body downloads require the admin token.
	anonReq, err := http.NewRequest(http.MethodGet, gw.ts.URL+"/api/logs/"+id+"/body/request", nil)
	if err != nil {
		t.Fatal(err)
	}
	anonResp, err := gw.ts.Client().Do(anonReq)
	if err != nil {
		t.Fatalf("anonymous body request: %v", err)
	}
	anonResp.Body.Close()
	if anonResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous body download status = %d, want 401", anonResp.StatusCode)
	}
}

// TestGatewayIntegrationPersistenceReloadAndRouting restarts the gateway from the
// same config/log directories and verifies persisted logs, masked-config
// round-tripping, /api/chat proxy routing and rejected-config safety.
func TestGatewayIntegrationPersistenceReloadAndRouting(t *testing.T) {
	requestBody := paddedRequest(integrationBodyBytes)
	firstBody := paddedError("attempt-one-error", integrationBodyBytes)
	secondBody := paddedError(integrationMarker, integrationBodyBytes)
	successBody := paddedSuccess(integrationBodyBytes)

	up := newFakeUpstream(firstBody, secondBody, successBody)
	upSrv := httptest.NewServer(http.HandlerFunc(up.handler))
	defer upSrv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	logDir := filepath.Join(dir, "logs")

	gw1 := startIntegrationGateway(t, cfgPath, logDir, integrationAdminToken)
	configureUpstream(t, gw1, upSrv.URL)
	if got := postGateway(t, gw1, "/v1/chat/completions", requestBody); !bytes.Equal(got, successBody) {
		t.Fatalf("first lifecycle body mismatch: got %d bytes", len(got))
	}
	logID := waitForLog(t, gw1, http.MethodPost, "/v1/chat/completions").ID
	if got := fetchAdminBody(t, gw1, "/api/logs/"+logID+"/body/request"); !bytes.Equal(got, requestBody) {
		t.Fatal("pre-restart persisted request body mismatch")
	}
	gw1.ts.Close()

	// Reopen from the same on-disk config and log directory.
	gw2 := startIntegrationGateway(t, cfgPath, logDir, integrationAdminToken)
	defer gw2.ts.Close()

	// Prior metadata and full bodies survive the restart.
	resp := doAdmin(t, gw2, http.MethodGet, "/api/logs/"+logID, integrationAdminToken, nil)
	var entry store.LogEntry
	err := json.NewDecoder(resp.Body).Decode(&entry)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("decode reloaded entry: %v", err)
	}
	if entry.ID != logID || entry.Status != http.StatusOK || len(entry.Attempts) != 3 {
		t.Fatalf("reloaded entry id=%q status=%d attempts=%d", entry.ID, entry.Status, len(entry.Attempts))
	}
	if got := fetchAdminBody(t, gw2, "/api/logs/"+logID+"/body/request"); !bytes.Equal(got, requestBody) {
		t.Fatalf("reloaded request body mismatch: hash %s want %s", sha256Hex(got), sha256Hex(requestBody))
	}
	if got := fetchAdminBody(t, gw2, "/api/logs/"+logID+"/body/attempt-1"); !bytes.Equal(got, firstBody) {
		t.Fatalf("reloaded attempt-1 body mismatch: hash %s want %s", sha256Hex(got), sha256Hex(firstBody))
	}
	if got := fetchAdminBody(t, gw2, "/api/logs/"+logID+"/body/response"); !bytes.Equal(got, successBody) {
		t.Fatalf("reloaded response body mismatch: hash %s want %s", sha256Hex(got), sha256Hex(successBody))
	}

	// Masked config round-trip must preserve the real configured secret.
	resp = doAdmin(t, gw2, http.MethodGet, "/api/config", integrationAdminToken, nil)
	maskedRaw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read masked config: %v", err)
	}
	if bytes.Contains(maskedRaw, []byte(integrationSecret)) {
		t.Fatal("reloaded GET /api/config leaked the real secret")
	}
	putResp := doAdmin(t, gw2, http.MethodPut, "/api/config", integrationAdminToken, maskedRaw)
	putBody, _ := io.ReadAll(putResp.Body)
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("masked roundtrip PUT status = %d, want 200 (body %s)", putResp.StatusCode, truncate(putBody))
	}

	// The next forwarded request must carry the real secret from storage.
	postGateway(t, gw2, "/v1/chat/completions?after=roundtrip", requestBody)
	calls := up.snapshot()
	if len(calls) == 0 {
		t.Fatal("no upstream calls recorded")
	}
	if last := calls[len(calls)-1]; last.fakeAuth != integrationSecret {
		t.Fatalf("post-roundtrip X-Fake-Auth = %q, want real secret", last.fakeAuth)
	}

	// /api/chat is a business route and must reach the upstream, not the admin API.
	postGateway(t, gw2, "/api/chat", []byte(`{"model":"test-model","messages":[]}`))
	calls = up.snapshot()
	if last := calls[len(calls)-1]; last.path != "/api/chat" {
		t.Fatalf("/api/chat reached upstream path %q, want /api/chat", last.path)
	}

	// An invalid regex update is rejected and must not disturb routing.
	bad := gw2.cfg.Get()
	bad.Retry.BodyRegexes = []string{"("}
	badRaw, err := json.Marshal(bad)
	if err != nil {
		t.Fatal(err)
	}
	badResp := doAdmin(t, gw2, http.MethodPut, "/api/config", integrationAdminToken, badRaw)
	badBody, _ := io.ReadAll(badResp.Body)
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid regex PUT status = %d, want 400 (body %s)", badResp.StatusCode, truncate(badBody))
	}
	before := len(up.snapshot())
	if got := postGateway(t, gw2, "/v1/chat/completions", requestBody); !bytes.Equal(got, successBody) {
		t.Fatal("route broken after rejected config update")
	}
	if after := len(up.snapshot()); after != before+1 {
		t.Fatalf("upstream calls after rejected update = %d, want %d", after, before+1)
	}
}

// configureProvider installs one enabled provider with an explicit ID and
// stored headers, so dedicated-route pass-through can be tested against a
// deliberately conflicting stored credential.
func configureProvider(t *testing.T, gw *integrationGateway, id, upstreamURL string, headers map[string]string) {
	t.Helper()
	cfg := config.Default()
	cfg.BodySniffTimeoutMS = 1000
	cfg.Upstreams = []config.Upstream{{
		ID:        id,
		Name:      "provider-" + id,
		BaseURL:   upstreamURL,
		Enabled:   true,
		Weight:    100,
		TimeoutMS: 10000,
		Headers:   headers,
	}}
	cfg.Retry = config.Retry{
		MaxAttempts: 3,
		BaseDelayMS: 0,
		MaxDelayMS:  0,
		Jitter:      false,
		StatusCodes: []int{http.StatusTooManyRequests},
		BodyRegexes: []string{integrationMarker},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	resp := doAdmin(t, gw, http.MethodPut, "/api/config", integrationAdminToken, raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("configure provider PUT status = %d, want 200 (body %s)", resp.StatusCode, truncate(body))
	}
}

// providerCall records the credential/credential-adjacent fields of one request
// seen by a provider.
type providerCall struct {
	method   string
	path     string
	query    string
	auth     string
	apiKey   string
	google   string
	bodyHash string
}

type providerRecorder struct {
	mu    sync.Mutex
	calls []providerCall
}

func (p *providerRecorder) handler(statuses []int, bodies [][]byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(body)
		p.mu.Lock()
		n := len(p.calls)
		p.calls = append(p.calls, providerCall{
			method:   r.Method,
			path:     r.URL.Path,
			query:    r.URL.RawQuery,
			auth:     r.Header.Get("Authorization"),
			apiKey:   r.Header.Get("X-API-Key"),
			google:   r.Header.Get("X-Goog-Api-Key"),
			bodyHash: hex.EncodeToString(sum[:]),
		})
		p.mu.Unlock()
		status := statuses[len(statuses)-1]
		if n < len(statuses) {
			status = statuses[n]
		}
		var respBody []byte
		if n < len(bodies) {
			respBody = bodies[n]
		} else if len(bodies) > 0 {
			respBody = bodies[len(bodies)-1]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
	}
}

func (p *providerRecorder) snapshot() []providerCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]providerCall(nil), p.calls...)
}

func (p *providerRecorder) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// TestGatewayIntegrationDedicatedProviderRouting proves the full public
// contract: /{id}/... pins that provider, strips the identifier, passes the
// caller's own key through (ignoring stored overrides and legacy weight/globs),
// retries only that provider, and never falls back for unknown/disabled IDs.
func TestGatewayIntegrationDedicatedProviderRouting(t *testing.T) {
	const vendorKey = "Bearer vendor-openai-key"
	const vendorAPIKey = "vendor-openai-x-api-key"
	const vendorGoogle = "vendor-openai-google-key"
	const requestBody = `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`
	requestHash := sha256Hex([]byte(requestBody))

	a := &providerRecorder{}
	b := &providerRecorder{}
	upA := httptest.NewServer(a.handler(
		[]int{http.StatusTooManyRequests, http.StatusOK},
		[][]byte{[]byte(`{"error":"retry"}`), []byte(`{"ok":"A"}`)},
	))
	defer upA.Close()
	upB := httptest.NewServer(b.handler([]int{http.StatusOK}, [][]byte{[]byte(`{"ok":"B"}`)}))
	defer upB.Close()

	dir := t.TempDir()
	gw := startIntegrationGateway(t,
		filepath.Join(dir, "config.json"), filepath.Join(dir, "logs"), integrationAdminToken)
	defer gw.ts.Close()
	// The stored Authorization deliberately conflicts with the caller's key.
	configureProvider(t, gw, "openai", upA.URL, map[string]string{
		"Authorization": "Bearer STORED-MUST-NOT-BE-USED",
	})
	// A second, healthy provider that must never be reached by the /openai URL.
	cfg := gw.cfg.Get()
	cfg.Upstreams = append(cfg.Upstreams, config.Upstream{
		ID: "other", Name: "other", BaseURL: upB.URL, Enabled: true, Weight: 100,
		TimeoutMS: 10000,
	})
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	putResp := doAdmin(t, gw, http.MethodPut, "/api/config", integrationAdminToken, raw)
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("add second provider status = %d", putResp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodPost, gw.ts.URL+"/openai/v1/chat/completions?trace=abc", bytes.NewReader([]byte(requestBody)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", vendorKey)
	req.Header.Set("X-API-Key", vendorAPIKey)
	req.Header.Set("X-Goog-Api-Key", vendorGoogle)
	req.Header.Set("Content-Type", "application/json")
	resp, err := gw.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("dedicated request: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != `{"ok":"A"}` {
		t.Fatalf("status=%d body=%q want 200/{\"ok\":\"A\"}", resp.StatusCode, got)
	}

	if b.count() != 0 {
		t.Fatalf("second provider received %d calls; dedicated route must not switch provider", b.count())
	}
	calls := a.snapshot()
	if len(calls) != 2 {
		t.Fatalf("provider A calls=%d want 2 (429 then 200)", len(calls))
	}
	for i, c := range calls {
		if c.method != http.MethodPost {
			t.Errorf("call %d method=%q want POST", i+1, c.method)
		}
		if c.path != "/v1/chat/completions" {
			t.Errorf("call %d path=%q want /v1/chat/completions", i+1, c.path)
		}
		if c.query != "trace=abc" {
			t.Errorf("call %d query=%q want trace=abc", i+1, c.query)
		}
		if c.auth != vendorKey {
			t.Errorf("call %d Authorization=%q want caller key (stored override ignored)", i+1, c.auth)
		}
		if c.apiKey != vendorAPIKey {
			t.Errorf("call %d X-API-Key=%q want caller key", i+1, c.apiKey)
		}
		if c.google != vendorGoogle {
			t.Errorf("call %d X-Goog-Api-Key=%q want caller key", i+1, c.google)
		}
		if c.bodyHash != requestHash {
			t.Errorf("call %d body hash=%s want %s", i+1, c.bodyHash, requestHash)
		}
	}

	entry := waitForLog(t, gw, http.MethodPost, "/openai/v1/chat/completions")
	if entry.Path != "/openai/v1/chat/completions" {
		t.Errorf("logged path=%q want original incoming path", entry.Path)
	}
	if len(entry.Attempts) != 2 {
		t.Fatalf("attempts=%d want 2", len(entry.Attempts))
	}
	for _, att := range entry.Attempts {
		if att.UpstreamID != "openai" {
			t.Errorf("attempt upstream=%q want openai (pinned)", att.UpstreamID)
		}
		if !strings.Contains(att.URL, "/v1/chat/completions") {
			t.Errorf("logged attempt URL=%q want stripped path", att.URL)
		}
	}

	// Unknown dedicated-shaped URLs 404 without touching any provider.
	for _, path := range []string{"/ghost/v1/chat/completions", "/ghost/v1beta/models", "/ghost/v2/chat"} {
		r, err := http.Get(gw.ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s status=%d want 404", path, r.StatusCode)
		}
	}
	if a.count() != 2 || b.count() != 0 {
		t.Fatalf("unknown dedicated URLs reached providers: A=%d B=%d", a.count(), b.count())
	}

	// Legacy unprefixed routing still reaches a provider.
	if got := postGateway(t, gw, "/v1/chat/completions", []byte(requestBody)); len(got) == 0 {
		t.Fatal("legacy /v1 route stopped working")
	}
}

// TestGatewayIntegrationBusinessRouteNeedsNoAdminToken proves the business
// router performs no local key lookup/validation: a request carrying only a
// provider key succeeds while the admin API still requires its own token.
func TestGatewayIntegrationBusinessRouteNeedsNoAdminToken(t *testing.T) {
	const providerKey = "Bearer caller-provider-key"
	a := &providerRecorder{}
	upA := httptest.NewServer(a.handler([]int{http.StatusOK}, [][]byte{[]byte(`{"ok":true}`)}))
	defer upA.Close()

	dir := t.TempDir()
	gw := startIntegrationGateway(t,
		filepath.Join(dir, "config.json"), filepath.Join(dir, "logs"), integrationAdminToken)
	defer gw.ts.Close()
	configureProvider(t, gw, "openai", upA.URL, nil)

	// A business request with an admin-token-looking value must pass through,
	// not be rejected, because the business router does not inspect it.
	req, err := http.NewRequest(http.MethodPost, gw.ts.URL+"/openai/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", integrationAdminToken)
	resp, err := gw.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("business request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("business route status=%d want 200 (no local key check)", resp.StatusCode)
	}
	calls := a.snapshot()
	if len(calls) != 1 {
		t.Fatalf("provider calls=%d want 1", len(calls))
	}
	if calls[0].auth != integrationAdminToken {
		t.Fatalf("provider Authorization=%q want the caller's value forwarded verbatim", calls[0].auth)
	}

	// The management API still enforces its own authorization.
	anon := doAdmin(t, gw, http.MethodGet, "/api/config", "", nil)
	anon.Body.Close()
	if anon.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous admin status=%d want 401", anon.StatusCode)
	}
	// And a valid business key does not unlock the admin API.
	adminReq, err := http.NewRequest(http.MethodGet, gw.ts.URL+"/api/config", nil)
	if err != nil {
		t.Fatal(err)
	}
	adminReq.Header.Set("Authorization", providerKey)
	adminResp, err := gw.ts.Client().Do(adminReq)
	if err != nil {
		t.Fatal(err)
	}
	adminResp.Body.Close()
	if adminResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("provider-key admin status=%d want 401", adminResp.StatusCode)
	}
}
