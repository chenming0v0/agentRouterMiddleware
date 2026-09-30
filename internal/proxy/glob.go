package proxy

import (
	"regexp"
	"strings"
	"sync"
)

type globResult struct {
	re  *regexp.Regexp
	err error
}

var globCache sync.Map // pattern string -> globResult

// globMatch reports whether s matches pattern, where '*' matches any run of
// characters (including '/') and '?' matches exactly one character. The match
// is anchored at both ends. path.Match is deliberately not used because its
// '*' does not cross '/', so "/v1/*" would not match "/v1/chat/completions".
func globMatch(pattern, s string) bool {
	if v, ok := globCache.Load(pattern); ok {
		g := v.(globResult)
		return g.err == nil && g.re.MatchString(s)
	}
	re, err := compileGlob(pattern)
	globCache.Store(pattern, globResult{re: re, err: err})
	return err == nil && re.MatchString(s)
}

// compileGlob translates the glob into an anchored regexp by escaping every
// literal rune and mapping '*' and '?' to regexp wildcards.
func compileGlob(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
