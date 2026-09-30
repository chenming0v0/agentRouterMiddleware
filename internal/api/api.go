// Package api exposes the middleware's administrative REST API.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agentrouter/internal/config"
	"agentrouter/internal/store"
)

const (
	// maxAPIBodyBytes bounds JSON request bodies for the admin API.
	maxAPIBodyBytes = 4 << 20
	// defaultPingInterval is the SSE heartbeat period.
	defaultPingInterval = 15 * time.Second
	// maskToken replaces configured header secrets in GET /api/config and is
	// resolved back to the stored value by PUT /api/config.
	maskToken = "***"
)

// adminExactPaths lists the fixed admin endpoints. Everything else under
// /api/ (for example /api/chat) is not an admin path and must reach the proxy.
var adminExactPaths = map[string]bool{
	"/api/health":         true,
	"/api/config":         true,
	"/api/logs":           true,
	"/api/logs/stream":    true,
	"/api/stats":          true,
	"/api/regex/test":     true,
	"/api/upstreams/test": true,
}

// IsAdminPath reports whether path belongs to the admin API namespace. The
// root dispatcher and Server share this single source of truth so a path can
// never be treated as admin by one side and proxied by the other.
func IsAdminPath(path string) bool {
	if adminExactPaths[path] {
		return true
	}
	// Detail namespace: /api/logs/{id} and /api/logs/{id}/body/{part}. The bare
	// trailing-slash form stays admin so it 404s instead of being proxied.
	return strings.HasPrefix(path, "/api/logs/")
}

// probeClient never follows redirects: any HTTP response, including 3xx/4xx,
// proves the upstream is reachable.
var probeClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Server implements the /api/* endpoints. The zero value is not usable; call New.
type Server struct {
	cfg  *config.Store
	logs *store.LogStore
	// token is the optional admin bearer token. When empty the API is
	// restricted to loopback peers.
	token string
	ping  time.Duration
}

// New builds the API server. token may be empty to enable loopback-only mode.
func New(cfg *config.Store, logs *store.LogStore, token string) *Server {
	return &Server{cfg: cfg, logs: logs, token: token, ping: defaultPingInterval}
}

// ServeHTTP routes the admin API. Only known admin paths are accepted; any
// other path returns 404 rather than being treated as configuration.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "/api/health" { // public, read-only
		s.serveHealth(w, r)
		return
	}
	if !IsAdminPath(p) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if !s.authorize(w, r) {
		return
	}
	switch {
	case p == "/api/config":
		s.serveConfig(w, r)
	case p == "/api/logs":
		s.serveLogs(w, r)
	case p == "/api/logs/stream":
		s.serveLogStream(w, r)
	case p == "/api/stats":
		s.serveStats(w, r)
	case p == "/api/regex/test":
		s.serveRegexTest(w, r)
	case p == "/api/upstreams/test":
		s.serveUpstreamTest(w, r)
	case strings.HasPrefix(p, "/api/logs/"):
		s.serveLogDetail(w, r, p)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) serveHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	writeJSON(w, http.StatusOK, okBody{OK: true})
}

func (s *Server) serveConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, maskConfig(s.cfg.Get()))
	case http.MethodPut:
		s.updateConfig(w, r)
	default:
		methodNotAllowed(w, "GET, PUT")
	}
}

// maskConfig replaces every configured header value with a literal "***" so
// upstream credentials never leave the process. Keys and ids are preserved.
func maskConfig(c *config.Config) *config.Config {
	for i := range c.Upstreams {
		for name := range c.Upstreams[i].Headers {
			c.Upstreams[i].Headers[name] = maskToken
		}
	}
	return c
}

// updateConfig validates and persists before touching any live state, so a
// rejected configuration is never partially applied.
func (s *Server) updateConfig(w http.ResponseWriter, r *http.Request) {
	var envelope putEnvelope
	envelope.Config = config.Default()
	envelope.Config.Upstreams = nil
	if err := decodeJSON(w, r, &envelope); err != nil {
		writeError(w, statusForDecode(err, http.StatusBadRequest), err.Error())
		return
	}
	current := s.cfg.Get()
	upstreams, err := resolveUpstreams(envelope.Upstreams, current.Upstreams)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	envelope.Config.Upstreams = upstreams

	saved, err := s.cfg.Set(envelope.Config)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.logs.SetLimit(saved.LogLimit)
	writeJSON(w, http.StatusOK, okBody{OK: true})
}

// putEnvelope shadows Config.Upstreams with the raw JSON so header presence and
// null can be distinguished from an explicit empty object.
type putEnvelope struct {
	*config.Config
	Upstreams json.RawMessage `json:"upstreams"`
}

// resolveUpstreams replaces the provider list. An absent or null value keeps
// the current list; an explicit array is decoded per entry.
func resolveUpstreams(raw json.RawMessage, current []config.Upstream) ([]config.Upstream, error) {
	if isAbsentOrNull(raw) {
		return cloneUpstreams(current), nil
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(raw, &raws); err != nil {
		return nil, fmt.Errorf("upstreams: %w", err)
	}
	out := make([]config.Upstream, 0, len(raws))
	for i, rawUp := range raws {
		up, err := resolveUpstream(rawUp, current)
		if err != nil {
			return nil, fmt.Errorf("upstream %d: %w", i, err)
		}
		out = append(out, up)
	}
	return out, nil
}

type upstreamEnvelope struct {
	*config.Upstream
	Headers json.RawMessage `json:"headers"`
}

func resolveUpstream(raw json.RawMessage, current []config.Upstream) (config.Upstream, error) {
	up := config.Upstream{TimeoutMS: config.DefaultUpstreamTimeoutMS}
	var envelope upstreamEnvelope
	envelope.Upstream = &up
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return config.Upstream{}, err
	}
	if isAbsentOrNull(envelope.Headers) {
		// Omitted or null headers preserve the stored map.
		up.Headers = headersFor(up.ID, current)
		return up, nil
	}
	var supplied map[string]string
	if err := json.Unmarshal(envelope.Headers, &supplied); err != nil {
		return config.Upstream{}, fmt.Errorf("headers: %w", err)
	}
	existing := headersFor(up.ID, current)
	resolved := make(map[string]string, len(supplied))
	for name, value := range supplied {
		if value != maskToken {
			resolved[name] = value
			continue
		}
		old, ok := lookupHeader(existing, name)
		if !ok {
			return config.Upstream{}, fmt.Errorf("header %q is masked with %q but upstream %q has no stored value", name, maskToken, up.ID)
		}
		resolved[name] = old
	}
	up.Headers = resolved
	return up, nil
}

func isAbsentOrNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

func headersFor(id string, current []config.Upstream) map[string]string {
	for i := range current {
		if current[i].ID == id {
			return cloneHeaders(current[i].Headers)
		}
	}
	return nil
}

func lookupHeader(headers map[string]string, name string) (string, bool) {
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

func cloneHeaders(headers map[string]string) map[string]string {
	if headers == nil {
		return nil
	}
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		out[k] = v
	}
	return out
}

func cloneUpstreams(in []config.Upstream) []config.Upstream {
	out := make([]config.Upstream, len(in))
	for i := range in {
		out[i] = in[i].Clone()
	}
	return out
}

func (s *Server) serveLogs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listLogs(w, r)
	case http.MethodDelete:
		s.logs.Clear()
		writeJSON(w, http.StatusOK, okBody{OK: true})
	default:
		methodNotAllowed(w, "GET, DELETE")
	}
}

func (s *Server) listLogs(w http.ResponseWriter, r *http.Request) {
	limit := config.DefaultLogLimit
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "limit must be a non-negative integer")
			return
		}
		limit = n
	}
	entries := s.logs.List(limit)
	writeJSON(w, http.StatusOK, logsBody{Logs: entries, Total: s.logs.Count()})
}

func (s *Server) serveStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	writeJSON(w, http.StatusOK, s.logs.Stats())
}

// serveLogStream streams new log entries as Server-Sent Events.
func (s *Server) serveLogStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	// Subscribe before flushing headers so the client cannot observe a stream
	// that misses an entry published in between.
	ch, cancel := s.logs.Subscribe()
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(s.ping)
	defer ticker.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case e, open := <-ch:
			if !open {
				return
			}
			data, err := json.Marshal(e)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// serveLogDetail handles /api/logs/{id} metadata and
// /api/logs/{id}/body/{part} raw captures.
func (s *Server) serveLogDetail(w http.ResponseWriter, r *http.Request, p string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	rest := strings.TrimPrefix(p, "/api/logs/")
	id, part, hasBody := cutBodyPart(rest)
	if !validLogID(id) {
		writeError(w, http.StatusBadRequest, "invalid log id")
		return
	}
	if !hasBody {
		entry, ok := s.logs.Get(id)
		if !ok {
			writeError(w, http.StatusNotFound, "log entry not found")
			return
		}
		writeJSON(w, http.StatusOK, entry)
		return
	}
	if !validBodyPart(part) {
		writeError(w, http.StatusBadRequest, "invalid body part")
		return
	}
	s.streamLogBody(w, r, id, part)
}

// streamLogBody copies the raw captured body to the client. It works even for
// an incomplete capture; the metadata flags explain the state.
func (s *Server) streamLogBody(w http.ResponseWriter, r *http.Request, id, part string) {
	rc, size, err := s.logs.OpenBody(id, part)
	if err != nil {
		writeError(w, http.StatusNotFound, "body not available")
		return
	}
	defer func() { _ = rc.Close() }()

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	if size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// cutBodyPart splits "{id}/body/{part}" but leaves a bare id untouched.
func cutBodyPart(rest string) (id, part string, hasBody bool) {
	const sep = "/body/"
	if i := strings.Index(rest, sep); i >= 0 {
		return rest[:i], rest[i+len(sep):], true
	}
	return rest, "", false
}

// validLogID accepts only opaque identifier characters, which makes path
// traversal and nested segments impossible.
func validLogID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// validBodyPart accepts "request", "response" and canonical "attempt-N".
func validBodyPart(part string) bool {
	if part == "request" || part == "response" {
		return true
	}
	n, ok := strings.CutPrefix(part, "attempt-")
	if !ok {
		return false
	}
	i, err := strconv.Atoi(n)
	return err == nil && i >= 1 && strconv.Itoa(i) == n
}

func (s *Server) serveRegexTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	var in regexTestInput
	if err := decodeJSON(w, r, &in); err != nil {
		writeError(w, statusForDecode(err, http.StatusBadRequest), err.Error())
		return
	}
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		writeJSON(w, http.StatusOK, regexTestResult{Matched: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, regexTestResult{Matched: re.MatchString(in.Text)})
}

func (s *Server) serveUpstreamTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	var in upstreamTestInput
	if err := decodeJSON(w, r, &in); err != nil {
		writeError(w, statusForDecode(err, http.StatusBadRequest), err.Error())
		return
	}
	start := time.Now()
	status, err := probe(r.Context(), in.BaseURL)
	result := upstreamTestResult{
		OK:        err == nil,
		Status:    status,
		LatencyMS: int(time.Since(start).Milliseconds()),
	}
	if err != nil {
		result.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, result)
}

// probe issues a GET with a hard 10s timeout and no redirect following. Any
// HTTP status counts as reachable.
func probe(ctx context.Context, raw string) (int, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return 0, errors.New("base_url must be an absolute http/https URL")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return 0, err
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

// authorize enforces bearer-token access when configured, and loopback-only
// access otherwise. X-Forwarded-For is deliberately ignored so a remote peer
// cannot spoof loopback.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	if !originAllowed(r.Header.Get("Origin"), r.Host) {
		writeError(w, http.StatusForbidden, "cross-origin request rejected")
		return false
	}
	if s.token != "" {
		if BearerMatches(r.Header.Get("Authorization"), s.token) {
			return true
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "missing or invalid admin token; send Authorization: Bearer <token>")
		return false
	}
	if isLoopbackAddr(r.RemoteAddr) && isLoopbackHost(r.Host) {
		return true
	}
	writeError(w, http.StatusForbidden,
		"admin API is restricted to loopback; set AGENTROUTER_ADMIN_TOKEN or -admin-token to enable remote access")
	return false
}

// BearerMatches reports whether header carries token as a bearer credential,
// using a constant-time comparison.
func BearerMatches(header, token string) bool {
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	got := strings.TrimSpace(header[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// originAllowed accepts an absent Origin and otherwise requires the Origin
// host to match the request Host exactly (no wildcard CORS).
func originAllowed(origin, host string) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

func isLoopbackAddr(remoteAddr string) bool {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	return isLoopbackHost(host)
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// decodeJSON bounds the body and rejects any trailing content or extra JSON
// values, since json.Unmarshal does not accept data after the top-level value.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxAPIBodyBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func statusForDecode(err error, fallback int) int {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	return fallback
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorBody{Error: message})
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

type okBody struct {
	OK bool `json:"ok"`
}

type errorBody struct {
	Error string `json:"error"`
}

type logsBody struct {
	Logs  []store.LogEntry `json:"logs"`
	Total int              `json:"total"`
}

type regexTestInput struct {
	Pattern string `json:"pattern"`
	Text    string `json:"text"`
}

type regexTestResult struct {
	Matched bool   `json:"matched"`
	Error   string `json:"error"`
}

type upstreamTestInput struct {
	BaseURL string `json:"base_url"`
}

type upstreamTestResult struct {
	OK        bool   `json:"ok"`
	Status    int    `json:"status"`
	LatencyMS int    `json:"latency_ms"`
	Error     string `json:"error"`
}
