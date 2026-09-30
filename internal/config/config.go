// Package config defines the middleware configuration model and its
// concurrency-safe, atomically persisted store.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Defaults apply to new configurations and fields absent from a loaded file.
// Set validates explicit values without replacing invalid zero/negative values.
const (
	DefaultListen              = "127.0.0.1:18851"
	DefaultLogLimit            = 50
	DefaultMaxRequestBodyBytes = int64(64 << 20)
	DefaultMaxBufferBytes      = int64(8 << 20)
	DefaultMaxLogBodyBytes     = 0 // Complete capture; persistent stores stream to disk.
	DefaultUpstreamTimeoutMS   = 300000
	DefaultBodySniffTimeoutMS  = 250
	DefaultErrorBodyTimeoutMS  = 2000
	maxByteLimit               = 1 << 30
)

// Upstream is a single LLM API provider sitting behind the middleware.
type Upstream struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	BaseURL    string            `json:"base_url"`
	Enabled    bool              `json:"enabled"`
	Weight     int               `json:"weight"`
	TimeoutMS  int               `json:"timeout_ms"`
	PathGlobs  []string          `json:"path_globs"`
	ModelGlobs []string          `json:"model_globs"`
	Headers    map[string]string `json:"headers"`
}

// Clone returns a deep copy safe to hand to callers.
func (u Upstream) Clone() Upstream {
	cp := u
	cp.PathGlobs = append([]string(nil), u.PathGlobs...)
	cp.ModelGlobs = append([]string(nil), u.ModelGlobs...)
	if u.Headers != nil {
		cp.Headers = make(map[string]string, len(u.Headers))
		for k, v := range u.Headers {
			cp.Headers[k] = v
		}
	}
	return cp
}

// Retry controls when and how a failed attempt is retried.
type Retry struct {
	MaxAttempts         int      `json:"max_attempts"`
	BaseDelayMS         int      `json:"base_delay_ms"`
	MaxDelayMS          int      `json:"max_delay_ms"`
	Jitter              bool     `json:"jitter"`
	RetryOnNetworkError bool     `json:"retry_on_network_error"`
	StatusCodes         []int    `json:"status_codes"`
	BodyRegexes         []string `json:"body_regexes"`
}

// Config is the complete middleware configuration.
type Config struct {
	Listen              string     `json:"listen"`
	LogLimit            int        `json:"log_limit"`
	MaxRequestBodyBytes int64      `json:"max_request_body_bytes"`
	MaxBufferBytes      int64      `json:"max_buffer_bytes"`
	MaxLogBodyBytes     int        `json:"max_log_body_bytes"`
	BodySniffTimeoutMS  int        `json:"body_sniff_timeout_ms"`
	ErrorBodyTimeoutMS  int        `json:"error_body_timeout_ms"`
	Upstreams           []Upstream `json:"upstreams"`
	Retry               Retry      `json:"retry"`
}

func defaultRetry() Retry {
	return Retry{
		MaxAttempts:         3,
		BaseDelayMS:         500,
		MaxDelayMS:          8000,
		Jitter:              true,
		RetryOnNetworkError: false,
		StatusCodes:         []int{400, 404, 429, 500, 502, 503, 504},
		BodyRegexes:         []string{"(?i)rate limit", "(?i)no available channel", "(?i)无可用渠道"},
	}
}

// Default returns the built-in configuration, including one disabled sample upstream.
func Default() *Config {
	return &Config{
		Listen:              DefaultListen,
		LogLimit:            DefaultLogLimit,
		MaxRequestBodyBytes: DefaultMaxRequestBodyBytes,
		MaxBufferBytes:      DefaultMaxBufferBytes,
		MaxLogBodyBytes:     DefaultMaxLogBodyBytes,
		BodySniffTimeoutMS:  DefaultBodySniffTimeoutMS,
		ErrorBodyTimeoutMS:  DefaultErrorBodyTimeoutMS,
		Upstreams: []Upstream{{
			ID:         "u_sample",
			Name:       "newapi-main",
			BaseURL:    "https://api.example.com",
			Enabled:    false,
			Weight:     100,
			TimeoutMS:  DefaultUpstreamTimeoutMS,
			PathGlobs:  []string{"/v1/*"},
			ModelGlobs: []string{"gpt-4o*"},
			Headers:    map[string]string{"Authorization": "Bearer sk-xxx"},
		}},
		Retry: defaultRetry(),
	}
}

// Clone returns a deep copy so callers cannot mutate shared state.
func (c *Config) Clone() *Config {
	cp := *c
	cp.Upstreams = make([]Upstream, len(c.Upstreams))
	for i, u := range c.Upstreams {
		cp.Upstreams[i] = u.Clone()
	}
	cp.Retry.StatusCodes = append([]int(nil), c.Retry.StatusCodes...)
	cp.Retry.BodyRegexes = append([]string(nil), c.Retry.BodyRegexes...)
	return &cp
}

// generateIDs is the only normalization performed by Set.
func (c *Config) generateIDs() {
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if u.ID == "" {
			u.ID = "u_" + randHex(8)
		}
	}
}

// Validate checks the configuration, generating nothing and never mutating it.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("config is required")
	}
	if strings.TrimSpace(c.Listen) == "" {
		return errors.New("listen is required")
	}
	if c.LogLimit < 1 || c.LogLimit > 1000 {
		return errors.New("log_limit must be between 1 and 1000")
	}
	if c.MaxRequestBodyBytes < 1 || c.MaxRequestBodyBytes > maxByteLimit {
		return errors.New("max_request_body_bytes must be between 1 and 1073741824")
	}
	if c.MaxBufferBytes < 1 || c.MaxBufferBytes > maxByteLimit {
		return errors.New("max_buffer_bytes must be between 1 and 1073741824")
	}
	if c.MaxLogBodyBytes < 0 || c.MaxLogBodyBytes > maxByteLimit {
		return errors.New("max_log_body_bytes must be between 0 (complete) and 1073741824")
	}
	if c.BodySniffTimeoutMS < 1 || c.BodySniffTimeoutMS > 60000 {
		return errors.New("body_sniff_timeout_ms must be between 1 and 60000")
	}
	if c.ErrorBodyTimeoutMS < 1 || c.ErrorBodyTimeoutMS > 60000 {
		return errors.New("error_body_timeout_ms must be between 1 and 60000")
	}
	ids := make(map[string]bool, len(c.Upstreams))
	names := make(map[string]bool, len(c.Upstreams))
	for i := range c.Upstreams {
		if err := c.Upstreams[i].validate(ids, names); err != nil {
			return err
		}
	}
	rt := &c.Retry
	if rt.MaxAttempts < 1 || rt.MaxAttempts > 20 {
		return errors.New("retry.max_attempts must be between 1 and 20")
	}
	if rt.BaseDelayMS < 0 || rt.BaseDelayMS > 3600000 {
		return errors.New("retry.base_delay_ms must be between 0 and 3600000")
	}
	if rt.MaxDelayMS < 0 || rt.MaxDelayMS > 3600000 {
		return errors.New("retry.max_delay_ms must be between 0 and 3600000")
	}
	if rt.MaxDelayMS < rt.BaseDelayMS {
		return errors.New("retry.max_delay_ms must be >= retry.base_delay_ms")
	}
	for _, code := range rt.StatusCodes {
		if code < 100 || code > 599 {
			return fmt.Errorf("retry status code %d must be between 100 and 599", code)
		}
	}
	for _, pattern := range rt.BodyRegexes {
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("invalid body regex %q: %w", pattern, err)
		}
	}
	return nil
}

func (u *Upstream) validate(ids, names map[string]bool) error {
	if strings.TrimSpace(u.ID) == "" {
		return errors.New("upstream id is required")
	}
	if !ValidUpstreamID(u.ID) {
		return fmt.Errorf("upstream id %q must be 1-64 URL-safe characters "+
			"(letters, digits, '_' or '-') and not a reserved prefix (api, assets, v1, v1beta, v2)", u.ID)
	}
	if ids[u.ID] {
		return fmt.Errorf("duplicate upstream id %q", u.ID)
	}
	ids[u.ID] = true
	if strings.TrimSpace(u.Name) == "" {
		return fmt.Errorf("upstream %q: name is required", u.ID)
	}
	if names[u.Name] {
		return fmt.Errorf("duplicate upstream name %q", u.Name)
	}
	names[u.Name] = true
	if u.Weight < 0 || u.Weight > 1000000 {
		return fmt.Errorf("upstream %q: weight must be between 0 and 1000000", u.Name)
	}
	if u.TimeoutMS < 1 || u.TimeoutMS > 86400000 {
		return fmt.Errorf("upstream %q: timeout_ms must be between 1 and 86400000", u.Name)
	}
	parsed, err := url.Parse(u.BaseURL)
	if err != nil || parsed.Hostname() == "" || parsed.Opaque != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || strings.Contains(u.BaseURL, "#") {
		return fmt.Errorf("upstream %q: base_url must be an absolute http/https URL without userinfo or fragment", u.Name)
	}
	for name, value := range u.Headers {
		if !validHeaderName(name) || !validHeaderValue(value) {
			return fmt.Errorf("upstream %q: invalid configured header %q", u.Name, name)
		}
	}
	return nil
}

// reservedFirstSegments are first path segments the router interprets
// specially. An upstream ID must never collide with one (case-insensitive), or
// it could shadow a management/static route or a legacy API prefix.
var reservedFirstSegments = map[string]bool{
	"api":    true,
	"assets": true,
	"v1":     true,
	"v1beta": true,
	"v2":     true,
}

// upstreamIDPattern accepts a single URL-safe path segment: it must start with
// an alphanumeric character and contain only letters, digits, '-' or '_'.
var upstreamIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidUpstreamID reports whether id is usable as the URL identifier in
// /{id}/... — a single safe path segment that is not a reserved prefix.
func ValidUpstreamID(id string) bool {
	return upstreamIDPattern.MatchString(id) && !reservedFirstSegments[strings.ToLower(id)]
}

func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			continue
		}
		return false
	}
	return true
}

func validHeaderValue(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 127 || s[i] < 32 && s[i] != '\t' {
			return false
		}
	}
	return true
}

// Store owns the live configuration and persists it atomically.
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  *Config
}

// Load reads the configuration from path. A missing file is created with
// defaults. On any parse/validation failure the store still returns a usable
// default configuration together with the error, so startup never crashes.
func Load(path string) (*Store, error) {
	s := &Store{path: path, cfg: Default()}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, s.save(Default())
		}
		return s, err
	}
	c := Default()
	// Decode upstreams separately: reusing the sample slice would leak its
	// credentials into the first legacy upstream with omitted headers.
	var decoded = struct {
		*Config
		Upstreams json.RawMessage `json:"upstreams"`
	}{Config: c}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return s, fmt.Errorf("parse config %s: %w", path, err)
	}
	if strings.TrimSpace(string(data)) == "null" {
		return s, errors.New("config must be a JSON object")
	}
	if decoded.Upstreams != nil {
		var upstreams []json.RawMessage
		if err := json.Unmarshal(decoded.Upstreams, &upstreams); err != nil {
			return s, fmt.Errorf("parse upstreams: %w", err)
		}
		c.Upstreams = make([]Upstream, len(upstreams))
		for i, raw := range upstreams {
			c.Upstreams[i].TimeoutMS = DefaultUpstreamTimeoutMS
			if err := json.Unmarshal(raw, &c.Upstreams[i]); err != nil {
				return s, fmt.Errorf("parse upstream %d: %w", i, err)
			}
		}
	}
	c.generateIDs()
	if err := c.Validate(); err != nil {
		return s, fmt.Errorf("invalid config %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return s, fmt.Errorf("secure config %s: %w", path, err)
	}
	s.cfg = c
	return s, nil
}

// Get returns a deep copy of the current configuration.
func (s *Store) Get() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Clone()
}

// Set generates missing IDs, validates, persists and installs a configuration.
// On failure nothing is changed and the stored config is left untouched.
func (s *Store) Set(c *Config) (*Config, error) {
	if c == nil {
		return nil, errors.New("config is required")
	}
	cp := c.Clone()
	cp.generateIDs()
	if err := cp.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.save(cp); err != nil {
		return nil, err
	}
	s.cfg = cp
	return cp.Clone(), nil
}

// save writes the config to a temp file in the same directory and renames it,
// making the update atomic.
func (s *Store) save(c *Config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".agentrouter-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.path)
}

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)[:n]
}
