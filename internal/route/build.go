package route

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/oschwald/maxminddb-golang/v2"
)

// New validates cfg against the databases it names and builds a Router.
// dbs holds each database's bytes, keyed by its name under databases. Every
// problem found is reported, not just the first.
func New(cfg *Config, dbs map[string][]byte) (*Router, error) {
	b := &builder{
		r:       &Router{hosts: map[string][]rule{}, disabled: map[string]bool{}},
		index:   map[string]int{},
		choices: map[int][]string{},
	}
	readers := b.openDatabases(cfg.Databases, dbs)
	b.addTokens(cfg, readers)
	b.addPeers(cfg)
	b.r.names = make([]string, len(b.index))
	for name, i := range b.index {
		b.r.names[i] = name
	}
	b.addSuffixes(cfg.AllowedSuffixes)
	for _, d := range cfg.Disabled {
		b.r.disabled[strings.ToLower(d)] = true
	}
	b.addHosts(cfg.Hosts)

	if len(b.errs) > 0 {
		return nil, errors.Join(b.errs...)
	}
	return b.r, nil
}

// builder builds a Router from a config, collecting every problem it finds
// so that New can report them all at once.
type builder struct {
	r    *Router
	errs []error
	// index places each token and peer in the token values.
	index map[string]int
	// defaults are the token values for a client no database knows, which
	// every target must render to a safe hostname with.
	defaults []string
	// choices are each peer's possible values, its labels and default, so a
	// rule that matches a peer on anything else is a typo.
	choices map[int][]string
}

func (b *builder) fail(format string, args ...any) {
	b.errs = append(b.errs, fmt.Errorf(format, args...))
}

func (b *builder) openDatabases(files map[string]string, dbs map[string][]byte) map[string]*maxminddb.Reader {
	readers := map[string]*maxminddb.Reader{}
	for _, name := range sortedKeys(files) {
		raw, ok := dbs[name]
		if !ok {
			b.fail("databases.%s: not loaded", name)
			continue
		}
		rd, err := maxminddb.OpenBytes(raw)
		if err != nil {
			b.fail("databases.%s: %w", name, err)
			continue
		}
		readers[name] = rd
	}
	return readers
}

func (b *builder) addTokens(cfg *Config, readers map[string]*maxminddb.Reader) {
	byDB := map[string]*database{}
	for i, name := range sortedKeys(cfg.Tokens) {
		t := cfg.Tokens[name]
		b.index[name] = i
		b.defaults = append(b.defaults, t.Default)
		if !isTokenName(name) {
			b.fail("tokens.%s: name must be lowercase letters, digits or underscores", name)
		}
		if !isLabel(t.Default) {
			b.fail("tokens.%s: default %q is not a DNS label (a-z, 0-9, hyphen)", name, t.Default)
		}
		if len(t.Path) == 0 {
			b.fail("tokens.%s: path is empty", name)
		}
		if _, ok := cfg.Databases[t.DB]; !ok {
			b.fail("tokens.%s: unknown database %q", name, t.DB)
			continue
		}
		rd := readers[t.DB]
		if rd == nil {
			continue // already reported under databases
		}
		db := byDB[t.DB]
		if db == nil {
			db = &database{reader: rd}
			byDB[t.DB] = db
		}
		db.tokens = append(db.tokens, token{index: i, path: lookupPath(t.Path), def: t.Default})
	}
	for _, name := range sortedKeys(byDB) {
		b.r.dbs = append(b.r.dbs, *byDB[name])
	}
}

// addPeers places each peer after the tokens, so a peer works like a token
// from then on.
func (b *builder) addPeers(cfg *Config) {
	for _, name := range sortedKeys(cfg.Peers) {
		p, at := cfg.Peers[name], "peers."+name
		if _, clash := cfg.Tokens[name]; clash {
			b.fail("%s: a token has the same name", at)
			continue
		}
		idx := len(b.index)
		b.index[name] = idx
		b.defaults = append(b.defaults, p.Default)
		if !isTokenName(name) {
			b.fail("%s: name must be lowercase letters, digits or underscores", at)
		}
		if !isLabel(p.Default) {
			b.fail("%s: default %q is not a DNS label (a-z, 0-9, hyphen)", at, p.Default)
		}
		if _, ok := cfg.Tokens[p.Token]; !ok {
			b.fail("%s: unknown token %q", at, p.Token)
			continue
		}
		if len(p.Labels) == 0 {
			b.fail("%s: no labels", at)
		}
		labels := b.peerLabels(at, p.Labels)
		b.choices[idx] = append(sortedKeys(p.Labels), p.Default)
		slices.Sort(b.choices[idx])
		b.r.peers = append(b.r.peers, peer{index: idx, token: b.index[p.Token], labels: labels, def: p.Default})
	}
}

// peerLabels maps each token value a peer lists to its label.
func (b *builder) peerLabels(at string, sets map[string]Values) map[string]string {
	labels := map[string]string{}
	for _, label := range sortedKeys(sets) {
		if !isLabel(label) {
			b.fail("%s.labels: %q is not a DNS label (a-z, 0-9, hyphen)", at, label)
		}
		for _, v := range sets[label] {
			lv := strings.ToLower(v)
			if !isLabel(lv) {
				b.fail("%s.labels.%s: %q can never match: token values are DNS labels", at, label, v)
			}
			if other, dup := labels[lv]; dup {
				b.fail("%s: %q is under both %s and %s", at, lv, other, label)
			}
			labels[lv] = label
		}
	}
	return labels
}

func (b *builder) addSuffixes(suffixes []string) {
	for _, s := range suffixes {
		s = strings.ToLower(s)
		if !strings.HasPrefix(s, ".") || !isHostname(s[1:]) {
			b.fail("allowed_suffixes: %q must start with a dot and be a valid hostname", s)
			continue
		}
		b.r.suffixes = append(b.r.suffixes, s)
	}
}

func (b *builder) addHosts(hosts map[string][]Rule) {
	if len(hosts) == 0 {
		b.fail("hosts: none configured")
	}
	for _, host := range sortedKeys(hosts) {
		h := strings.ToLower(host)
		if !isHostname(h) {
			b.fail("hosts.%s: not a valid hostname", host)
		}
		if _, dup := b.r.hosts[h]; dup {
			b.fail("hosts.%s: listed twice (hostnames are case-insensitive)", host)
		}
		rules := hosts[host]
		if len(rules) == 0 {
			b.fail("hosts.%s: no rules", host)
		}
		compiled := make([]rule, 0, len(rules))
		for i, rl := range rules {
			compiled = append(compiled, b.compileRule(fmt.Sprintf("hosts.%s[%d]", host, i), rl))
		}
		b.r.hosts[h] = compiled
	}
}

// compileRule compiles rl, found at the config path at.
func (b *builder) compileRule(at string, rl Rule) rule {
	var c rule
	for _, name := range sortedKeys(rl.When) {
		if cd, ok := b.compileCond(at, name, rl.When[name]); ok {
			c.when = append(c.when, cd)
		}
	}
	if len(rl.Targets) == 0 {
		b.fail("%s: no targets", at)
	}
	for _, tmpl := range sortedKeys(rl.Targets) {
		if t, ok := b.compileTarget(fmt.Sprintf("%s.targets[%q]", at, tmpl), tmpl, rl.Targets[tmpl]); ok {
			c.targets = append(c.targets, t)
		}
	}
	return c
}

// compileCond compiles a rule's match on the token or peer called name.
func (b *builder) compileCond(at, name string, want Values) (cond, bool) {
	idx, ok := b.index[name]
	if !ok {
		b.fail("%s.when: unknown token %q", at, name)
		return cond{}, false
	}
	values := make([]string, 0, len(want))
	for _, v := range want {
		lv := strings.ToLower(v)
		switch opts, isPeer := b.choices[idx]; {
		case isPeer && !slices.Contains(opts, lv):
			b.fail("%s.when.%s: %q can never match: %s is %s", at, name, v, name, orList(opts))
		case !isLabel(lv):
			b.fail("%s.when.%s: %q can never match: token values are DNS labels", at, name, v)
		}
		values = append(values, lv)
	}
	return cond{token: idx, values: values}, true
}

// compileTarget compiles a target, checking that it renders to a safe
// hostname for a client no database knows.
func (b *builder) compileTarget(at, tmpl string, weight int) (target, bool) {
	if weight < 1 {
		b.fail("%s: weight must be at least 1", at)
	}
	tmpl = strings.ToLower(tmpl)
	parts, err := parseTemplate(tmpl, b.index)
	if err != nil {
		b.fail("%s: %w", at, err)
		return target{}, false
	}
	t := target{template: tmpl, parts: parts, weight: weight}
	switch name := t.render(b.defaults); {
	case !isHostname(name):
		b.fail("%s: renders %q with token defaults, which is not a valid hostname", at, name)
	case !b.r.underSuffix(name):
		b.fail("%s: %s is not under an allowed suffix", at, name)
	}
	return t, true
}

// lookupPath converts a config path to a maxminddb path, where numbers
// index into arrays.
func lookupPath(p []string) []any {
	out := make([]any, len(p))
	for i, s := range p {
		if n, err := strconv.Atoi(s); err == nil {
			out[i] = n
		} else {
			out[i] = s
		}
	}
	return out
}
