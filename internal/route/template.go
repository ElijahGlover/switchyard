package route

import (
	"errors"
	"fmt"
	"strings"
)

// target is a hostname template, such as "{location}.edge.example.com",
// with its weight in the pick.
type target struct {
	template string
	parts    []part
	weight   int
}

type part struct {
	literal string
	token   int // index into the token values, or -1 for a literal
}

func (t target) render(vals []string) string {
	if len(t.parts) == 1 && t.parts[0].token < 0 {
		return t.parts[0].literal
	}
	var b strings.Builder
	for _, p := range t.parts {
		if p.token < 0 {
			b.WriteString(p.literal)
		} else {
			b.WriteString(vals[p.token])
		}
	}
	return b.String()
}

func parseTemplate(s string, index map[string]int) ([]part, error) {
	var parts []part
	for s != "" {
		open := strings.IndexByte(s, '{')
		lit := s
		if open >= 0 {
			lit = s[:open]
		}
		if strings.Contains(lit, "}") {
			return nil, errors.New("unexpected }")
		}
		if lit != "" {
			parts = append(parts, part{literal: lit, token: -1})
		}
		if open < 0 {
			break
		}
		end := strings.IndexByte(s[open:], '}')
		if end < 0 {
			return nil, errors.New("unclosed {")
		}
		name := s[open+1 : open+end]
		i, ok := index[name]
		if !ok {
			return nil, fmt.Errorf("unknown token %q", name)
		}
		parts = append(parts, part{token: i})
		s = s[open+end+1:]
	}
	return parts, nil
}
