package route_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/ElijahGlover/switchyard/internal/route"
)

const aliasConfig = `
databases:
  netmap: netmap.mmdb
tokens:
  country: { db: netmap, path: [country], default: xx }
hosts:
  video.example.com:
    - when: { country: NO }
      targets: { "{country}.edge.example.com": 100 }
    - when: { country: &anz [AU, NZ] }
      targets: &commercial
        cdn-a.example.net: 70
    - when: { country: *anz }
      targets: *commercial
`

func parse(t *testing.T, yaml string) *route.Config {
	t.Helper()
	cfg, err := route.ParseConfig([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	return cfg
}

func TestParseConfigReadsMatchValuesAsWritten(t *testing.T) {
	rules := parse(t, aliasConfig).Hosts["video.example.com"]
	tests := []struct {
		name string
		rule int
		want route.Values
	}{
		{"a scalar stays a string, so NO is not false", 0, route.Values{"NO"}},
		{"a list", 1, route.Values{"AU", "NZ"}},
		{"an alias", 2, route.Values{"AU", "NZ"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rules[tt.rule].When["country"]; !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseConfigResolvesTargetAliases(t *testing.T) {
	rules := parse(t, aliasConfig).Hosts["video.example.com"]
	if got := rules[2].Targets["cdn-a.example.net"]; got != 70 {
		t.Errorf("got weight %d, want 70", got)
	}
}

func TestParseConfigRejects(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{"empty", "", "empty config"},
		{"unknown field", "hosts:\n  a.example.com:\n    - targts: {}\n", "targts"},
		{"unquoted template", "hosts:\n  a.example.com:\n    - targets:\n        {location}.edge.example.com: 100\n", "yaml"},
		{"when mapping", "hosts:\n  a.example.com:\n    - when: { network: { a: b } }\n", "scalar or a list"},
		{"when list of mappings", "hosts:\n  a.example.com:\n    - when: { network: [ { a: b } ] }\n", "scalar or a list"},
		{"absolute database path", "databases:\n  asn: /etc/asn.mmdb\n", "relative path"},
		{"escaping database path", "databases:\n  asn: ../asn.mmdb\n", "relative path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := route.ParseConfig([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ParseConfig error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}
