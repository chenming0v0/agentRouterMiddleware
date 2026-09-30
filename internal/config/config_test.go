package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func tempConfigPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "config.json")
}

// oneUpstream is a minimal valid upstream entry.
func oneUpstream() Upstream {
	return Upstream{
		ID:        "u1",
		Name:      "n1",
		BaseURL:   "http://upstream.example/v1",
		Enabled:   true,
		Weight:    1,
		TimeoutMS: DefaultUpstreamTimeoutMS,
	}
}

// validConfig returns a fresh, fully valid configuration.
func validConfig() *Config {
	c := Default()
	c.Upstreams = []Upstream{oneUpstream()}
	return c
}

// Load must create a usable default config when the file is missing.
func TestLoadCreatesDefaultsWhenMissing(t *testing.T) {
	path := tempConfigPath(t)
	st, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := st.Get().Listen; got != DefaultListen {
		t.Errorf("Listen=%q want %q", got, DefaultListen)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file not created: %v", err)
	}
}

// The default listen address must not be :8080.
func TestDefaultListenIs18851(t *testing.T) {
	if DefaultListen != "127.0.0.1:18851" {
		t.Fatalf("DefaultListen=%q want 127.0.0.1:18851", DefaultListen)
	}
	st, err := Load(tempConfigPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Get().Listen; got != "127.0.0.1:18851" {
		t.Errorf("default Listen=%q want 127.0.0.1:18851", got)
	}
}

// A malformed file must not crash startup: the store is still usable.
func TestLoadInvalidFileReturnsUsableDefaults(t *testing.T) {
	path := tempConfigPath(t)
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Load(path)
	if err == nil {
		t.Fatal("expected load error for malformed config")
	}
	if st == nil || st.Get() == nil {
		t.Fatal("expected a usable default store alongside the error")
	}
	if got := st.Get().Listen; got != DefaultListen {
		t.Errorf("Listen=%q want default %q", got, DefaultListen)
	}
}

// Legacy files that omit the newer timing/full-capture fields must load with
// defaults rather than being rejected.
func TestLoadDefaultsAbsentLegacyFields(t *testing.T) {
	path := tempConfigPath(t)
	legacy := `{
  "listen": "127.0.0.1:19999",
  "log_limit": 5,
  "max_request_body_bytes": 1234,
  "max_buffer_bytes": 5678,
  "upstreams": []
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Load(path)
	if err != nil {
		t.Fatalf("Load(legacy): %v", err)
	}
	got := st.Get()
	if got.Listen != "127.0.0.1:19999" || got.LogLimit != 5 {
		t.Errorf("explicit legacy fields not honored: %+v", got)
	}
	if got.MaxRequestBodyBytes != 1234 || got.MaxBufferBytes != 5678 {
		t.Errorf("explicit legacy limits not honored: %+v", got)
	}
	if got.BodySniffTimeoutMS != DefaultBodySniffTimeoutMS {
		t.Errorf("BodySniffTimeoutMS=%d want absent->default %d", got.BodySniffTimeoutMS, DefaultBodySniffTimeoutMS)
	}
	if got.ErrorBodyTimeoutMS != DefaultErrorBodyTimeoutMS {
		t.Errorf("ErrorBodyTimeoutMS=%d want absent->default %d", got.ErrorBodyTimeoutMS, DefaultErrorBodyTimeoutMS)
	}
	if got.MaxLogBodyBytes != 0 {
		t.Errorf("MaxLogBodyBytes=%d want absent->0 (full capture)", got.MaxLogBodyBytes)
	}
	if got.Retry.MaxAttempts != 3 || got.Retry.RetryOnNetworkError {
		t.Errorf("absent retry should default to MaxAttempts=3/network-retry=false: %+v", got.Retry)
	}
}

// A rejected Set must leave both the in-memory and on-disk config untouched.
func TestSetInvalidRegexLeavesStateAndDiskUnchanged(t *testing.T) {
	path := tempConfigPath(t)
	st, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	valid := validConfig()
	valid.Listen = ":9000"
	valid.LogLimit = 7
	valid.MaxRequestBodyBytes = 1024
	valid.MaxBufferBytes = 1024
	valid.MaxLogBodyBytes = 512
	valid.Retry = Retry{MaxAttempts: 1, BaseDelayMS: 0, MaxDelayMS: 0}
	if _, err := st.Set(valid); err != nil {
		t.Fatalf("Set(valid): %v", err)
	}
	before := st.Get()
	diskBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	bad := valid.Clone()
	bad.Retry.BodyRegexes = []string{"("}
	if _, err := st.Set(bad); err == nil {
		t.Fatal("expected Set to reject invalid body regex")
	}

	after := st.Get()
	if !reflect.DeepEqual(before, after) {
		t.Errorf("in-memory config changed after rejected Set:\nbefore=%+v\nafter=%+v", before, after)
	}
	diskAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(diskBefore, diskAfter) {
		t.Errorf("on-disk config changed after rejected Set")
	}
}

// Get must hand back a deep copy so callers cannot mutate shared state.
func TestGetReturnsDeepCopy(t *testing.T) {
	st, err := Load(tempConfigPath(t))
	if err != nil {
		t.Fatal(err)
	}
	c := st.Get()
	if len(c.Upstreams) == 0 {
		t.Fatal("expected the built-in sample upstream")
	}
	c.Upstreams[0].Headers["Authorization"] = "hacked"
	c.Upstreams[0].PathGlobs[0] = "hacked"
	c.Retry.StatusCodes[0] = 999
	c.Retry.BodyRegexes[0] = "hacked"

	again := st.Get()
	if again.Upstreams[0].Headers["Authorization"] == "hacked" {
		t.Error("Upstream.Headers aliased the store")
	}
	if again.Upstreams[0].PathGlobs[0] == "hacked" {
		t.Error("Upstream.PathGlobs aliased the store")
	}
	if again.Retry.StatusCodes[0] == 999 {
		t.Error("Retry.StatusCodes aliased the store")
	}
	if again.Retry.BodyRegexes[0] == "hacked" {
		t.Error("Retry.BodyRegexes aliased the store")
	}
}

// Explicit zero backoff is legitimate, and MaxLogBodyBytes 0 means full capture;
// both must round-trip through Set and Load.
func TestZeroBackoffAndFullCapturePreserved(t *testing.T) {
	path := tempConfigPath(t)
	st, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c := validConfig()
	c.Retry = Retry{MaxAttempts: 3, BaseDelayMS: 0, MaxDelayMS: 0}
	c.MaxLogBodyBytes = 0
	got, err := st.Set(c)
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got.Retry.MaxAttempts != 3 || got.Retry.BaseDelayMS != 0 || got.Retry.MaxDelayMS != 0 {
		t.Errorf("retry=%+v want MaxAttempts=3 with explicit zero delays", got.Retry)
	}
	if got.MaxLogBodyBytes != 0 {
		t.Errorf("MaxLogBodyBytes=%d want explicit 0 preserved", got.MaxLogBodyBytes)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	r := reloaded.Get()
	if r.Retry.MaxAttempts != 3 || r.Retry.BaseDelayMS != 0 || r.Retry.MaxDelayMS != 0 || r.MaxLogBodyBytes != 0 {
		t.Errorf("reloaded=%+v want zero backoff and full capture", r)
	}
}

// Explicit zero/negative required fields are rejected, not silently repaired.
func TestSetRejectsZeroRequiredFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(c *Config)
	}{
		{"zero_listen", func(c *Config) { c.Listen = "" }},
		{"zero_log_limit", func(c *Config) { c.LogLimit = 0 }},
		{"negative_log_limit", func(c *Config) { c.LogLimit = -1 }},
		{"zero_max_request_body", func(c *Config) { c.MaxRequestBodyBytes = 0 }},
		{"zero_max_buffer", func(c *Config) { c.MaxBufferBytes = 0 }},
		{"negative_max_log_body", func(c *Config) { c.MaxLogBodyBytes = -1 }},
		{"zero_sniff_timeout", func(c *Config) { c.BodySniffTimeoutMS = 0 }},
		{"zero_error_body_timeout", func(c *Config) { c.ErrorBodyTimeoutMS = 0 }},
		{"zero_max_attempts", func(c *Config) { c.Retry.MaxAttempts = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tempConfigPath(t)
			st, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			before := st.Get()
			diskBefore, _ := os.ReadFile(path)

			c := validConfig()
			tc.mutate(c)
			if _, err := st.Set(c); err == nil {
				t.Fatalf("Set accepted invalid config (%s)", tc.name)
			}
			if !reflect.DeepEqual(before, st.Get()) {
				t.Error("in-memory config changed after rejected Set")
			}
			diskAfter, _ := os.ReadFile(path)
			if !bytes.Equal(diskBefore, diskAfter) {
				t.Error("on-disk config changed after rejected Set")
			}
		})
	}
}

// Set writes atomically and a fresh Load observes the persisted values.
func TestSetPersistsAndReloads(t *testing.T) {
	path := tempConfigPath(t)
	st1, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c := validConfig()
	c.Listen = ":7777"
	c.LogLimit = 11
	c.MaxRequestBodyBytes = 2048
	c.MaxBufferBytes = 4096
	c.MaxLogBodyBytes = 128
	c.Retry = Retry{MaxAttempts: 2, BaseDelayMS: 0, MaxDelayMS: 0}
	if _, err := st1.Set(c); err != nil {
		t.Fatalf("Set: %v", err)
	}
	st2, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := st2.Get()
	if got.Listen != ":7777" || got.LogLimit != 11 || got.MaxLogBodyBytes != 128 {
		t.Errorf("reloaded config=%+v", got)
	}
	if got.BodySniffTimeoutMS != DefaultBodySniffTimeoutMS || got.ErrorBodyTimeoutMS != DefaultErrorBodyTimeoutMS {
		t.Errorf("reloaded timing fields=%d/%d want %d/%d", got.BodySniffTimeoutMS, got.ErrorBodyTimeoutMS,
			DefaultBodySniffTimeoutMS, DefaultErrorBodyTimeoutMS)
	}
}

// An invalid upstream must be rejected without mutating current state.
func TestSetRejectsInvalidUpstreamAndKeepsState(t *testing.T) {
	path := tempConfigPath(t)
	st, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	good := validConfig()
	if _, err := st.Set(good); err != nil {
		t.Fatalf("Set(valid): %v", err)
	}
	before := st.Get()
	diskBefore, _ := os.ReadFile(path)

	bad := validConfig()
	bad.Upstreams[0].BaseURL = "ftp://nope"
	if _, err := st.Set(bad); err == nil {
		t.Fatal("expected Set to reject non-http upstream base_url")
	}
	if !reflect.DeepEqual(before, st.Get()) {
		t.Error("config state changed after rejected Set")
	}
	diskAfter, _ := os.ReadFile(path)
	if !bytes.Equal(diskBefore, diskAfter) {
		t.Error("on-disk config changed after rejected Set")
	}
}

// Strict validation covers URL, header, retry and timing boundaries; every
// rejection must leave memory and disk untouched.
func TestSetStrictValidationRejectsAndPreserves(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(c *Config)
	}{
		{"blank_listen", func(c *Config) { c.Listen = "   " }},
		{"max_lt_base", func(c *Config) { c.Retry.BaseDelayMS = 100; c.Retry.MaxDelayMS = 50 }},
		{"too_many_attempts", func(c *Config) { c.Retry.MaxAttempts = 21 }},
		{"status_below_range", func(c *Config) { c.Retry.StatusCodes = []int{99} }},
		{"status_above_range", func(c *Config) { c.Retry.StatusCodes = []int{600} }},
		{"invalid_regex", func(c *Config) { c.Retry.BodyRegexes = []string{"("} }},
		{"userinfo_url", func(c *Config) { c.Upstreams[0].BaseURL = "http://user:pass@upstream.example/v1" }},
		{"fragment_url", func(c *Config) { c.Upstreams[0].BaseURL = "http://upstream.example/v1#frag" }},
		{"relative_url", func(c *Config) { c.Upstreams[0].BaseURL = "/relative" }},
		{"zero_upstream_timeout", func(c *Config) { c.Upstreams[0].TimeoutMS = 0 }},
		{"bad_header_name", func(c *Config) { c.Upstreams[0].Headers = map[string]string{"Bad Header": "x"} }},
		{"crlf_header_value", func(c *Config) { c.Upstreams[0].Headers = map[string]string{"X-Test": "a\r\nInjected: 1"} }},
		{"duplicate_ids", func(c *Config) { u := c.Upstreams[0]; c.Upstreams = []Upstream{u, u} }},
		{"duplicate_names", func(c *Config) {
			u := c.Upstreams[0]
			v := u
			v.ID = "u2"
			c.Upstreams = []Upstream{u, v}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tempConfigPath(t)
			st, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			before := st.Get()
			diskBefore, _ := os.ReadFile(path)

			c := validConfig()
			tc.mutate(c)
			if _, err := st.Set(c); err == nil {
				t.Fatalf("Set accepted invalid config (%s)", tc.name)
			}
			if !reflect.DeepEqual(before, st.Get()) {
				t.Error("in-memory config changed after rejected Set")
			}
			diskAfter, _ := os.ReadFile(path)
			if !bytes.Equal(diskBefore, diskAfter) {
				t.Error("on-disk config changed after rejected Set")
			}
		})
	}
}

// The config file must never be group/world readable, whether created by Load
// or rewritten by Set.
func TestConfigFilePermissions0600(t *testing.T) {
	path := tempConfigPath(t)
	st, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	assertFilePerm(t, path, 0o600)

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("reload: %v", err)
	}
	assertFilePerm(t, path, 0o600)

	if _, err := st.Set(validConfig()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	assertFilePerm(t, path, 0o600)
}

func assertFilePerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s perm=%o want %o", path, got, want)
	}
}

// A config whose listen field is only whitespace must be rejected.
func TestValidateRejectsWhitespaceListen(t *testing.T) {
	c := validConfig()
	c.Listen = "\t\n"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("Validate err=%v want listen-required error", err)
	}
}

// The upstream ID doubles as the /{id}/... URL identifier, so only a single,
// unambiguous, unreserved path segment is accepted.
func TestValidUpstreamID(t *testing.T) {
	valid := []string{
		"u1", "openai", "u_sample", "A-b_9", "0", "a",
		"550e8400-e29b-41d4-a716-446655440000",
		strings.Repeat("a", 64),
	}
	for _, id := range valid {
		if !ValidUpstreamID(id) {
			t.Errorf("ValidUpstreamID(%q) = false, want true", id)
		}
	}
	invalid := []string{
		"", " ", "-bad", "_bad", ".", "..", "bad.", "bad/id", "bad id",
		`bad\slash`, "bad%2f", "bad?x", "bad#x", "bad:port",
		"api", "API", "Api", "assets", "v1", "V1beta", "V2",
		strings.Repeat("a", 65),
	}
	for _, id := range invalid {
		if ValidUpstreamID(id) {
			t.Errorf("ValidUpstreamID(%q) = true, want false", id)
		}
	}
}

// Set must reject an upstream whose ID is not a safe URL identifier.
func TestSetRejectsInvalidUpstreamID(t *testing.T) {
	for _, id := range []string{"bad/id", "bad id", "bad.id", "-lead", "api", "assets", "v1", "v2", "v1beta"} {
		t.Run(id, func(t *testing.T) {
			st, err := Load(tempConfigPath(t))
			if err != nil {
				t.Fatal(err)
			}
			c := validConfig()
			c.Upstreams[0].ID = id
			if _, err := st.Set(c); err == nil {
				t.Fatalf("Set accepted invalid upstream id %q", id)
			}
		})
	}
}

// A missing ID is still auto-generated, and the generated value must itself be
// a valid URL identifier.
func TestGeneratedMissingIDIsValid(t *testing.T) {
	st, err := Load(tempConfigPath(t))
	if err != nil {
		t.Fatal(err)
	}
	c := validConfig()
	c.Upstreams[0].ID = ""
	saved, err := st.Set(c)
	if err != nil {
		t.Fatalf("Set with missing id: %v", err)
	}
	got := saved.Upstreams[0].ID
	if got == "" {
		t.Fatal("missing upstream id was not generated")
	}
	if !ValidUpstreamID(got) {
		t.Fatalf("generated id %q is not a valid URL identifier", got)
	}
}
