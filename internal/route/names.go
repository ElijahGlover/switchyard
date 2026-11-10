package route

import (
	"maps"
	"slices"
	"strings"
)

// isLabel reports whether s is a lowercase DNS label.
func isLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// isHostname reports whether s is a lowercase hostname.
func isHostname(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for l := range strings.SplitSeq(s, ".") {
		if !isLabel(l) {
			return false
		}
	}
	return true
}

func isTokenName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// sortedKeys gives map keys in a fixed order, so errors and tie-breaks are
// deterministic.
func sortedKeys[M ~map[string]V, V any](m M) []string {
	return slices.Sorted(maps.Keys(m))
}

// orList joins options as "a, b or c".
func orList(opts []string) string {
	if len(opts) < 2 {
		return strings.Join(opts, "")
	}
	return strings.Join(opts[:len(opts)-1], ", ") + " or " + opts[len(opts)-1]
}
