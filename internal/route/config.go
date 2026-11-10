package route

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Config is the routing configuration, normally config.yaml in S3.
type Config struct {
	// Databases maps a name to an MMDB file, relative to the config file.
	Databases map[string]string `yaml:"databases"`
	// Tokens are values read from a database for each client IP.
	Tokens map[string]Token `yaml:"tokens"`
	// Peers label sets of a token's values, such as a peer's ASNs. Each
	// peer is itself a token, usable in rules and targets.
	Peers map[string]Peer `yaml:"peers"`
	// AllowedSuffixes bounds where a redirect may point, e.g. ".example.net".
	AllowedSuffixes []string `yaml:"allowed_suffixes"`
	// Disabled lists targets to blackhole, as written in the config or as
	// rendered. Clients whose target is disabled are refused.
	Disabled []string `yaml:"disabled"`
	// Hosts maps an incoming hostname to its rules, checked in order.
	Hosts map[string][]Rule `yaml:"hosts"`
}

// Token reads one field from a database. Path walks into the record; a
// number in the path indexes into an array.
type Token struct {
	DB      string   `yaml:"db"`
	Path    []string `yaml:"path"`
	Default string   `yaml:"default"`
}

// Peer gives sets of a token's values a label, such as AS64500 and AS64501
// as "partner". A client whose value is in one of the sets gets that label;
// any other client gets Default.
type Peer struct {
	Token   string            `yaml:"token"`
	Default string            `yaml:"default"`
	Labels  map[string]Values `yaml:"labels"`
}

// Rule matches when every token in When has one of the listed values, then
// picks one of Targets by weight. A target may use {token} placeholders.
type Rule struct {
	When    map[string]Values `yaml:"when"`
	Targets map[string]int    `yaml:"targets"`
}

// Values is one value or a list of values, kept exactly as written so that
// YAML never turns a country code like NO into a boolean.
type Values []string

// UnmarshalYAML accepts a scalar or a sequence of scalars. The decoder
// resolves aliases before calling it.
func (v *Values) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*v = Values{n.Value}
		return nil
	case yaml.SequenceNode:
		out := make(Values, 0, len(n.Content))
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: when values must be a scalar or a list of scalars", c.Line)
			}
			out = append(out, c.Value)
		}
		*v = out
		return nil
	}
	return fmt.Errorf("line %d: when values must be a scalar or a list of scalars", n.Line)
}

// ParseConfig decodes YAML into a Config. Unknown fields are errors, so a
// typo fails loudly instead of silently dropping a rule. Semantic checks
// happen in New.
func ParseConfig(b []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config: empty config")
		}
		return nil, fmt.Errorf("config: %w", err)
	}
	for _, name := range sortedKeys(cfg.Databases) {
		if p := cfg.Databases[name]; !isRelative(p) {
			return nil, fmt.Errorf("config: databases.%s: %q must be a relative path inside the config's directory", name, p)
		}
	}
	return &cfg, nil
}

func isRelative(p string) bool {
	return p != "" && !path.IsAbs(p) && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../")
}
