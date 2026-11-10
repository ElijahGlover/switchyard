// Package route turns a client IP and hostname into a target hostname.
//
// Tokens are fields read from MMDB databases for the client IP, such as an
// ASN, a network name or a location. Each incoming hostname has rules that
// match on tokens and pick a target by weight. Targets can use tokens as
// placeholders, so "{location}.edge.example.com" sends a client to its PoP.
package route

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/oschwald/maxminddb-golang/v2"
)

// Errors returned by Resolve and Explain.
var (
	ErrUnknownHost = errors.New("route: unknown host")
	ErrNoTarget    = errors.New("route: no rule matched")
	// ErrBlackholed means the client's target is disabled. The client is
	// refused rather than sent elsewhere.
	ErrBlackholed = errors.New("route: target disabled")
)

// Router makes routing decisions from a validated config. It never changes
// after New and is safe for concurrent use.
type Router struct {
	dbs      []database
	peers    []peer
	names    []string // token and peer names, by index into the token values
	hosts    map[string][]rule
	disabled map[string]bool
	suffixes []string
}

// Decision is how a client's target was chosen, as Explain reports it.
type Decision struct {
	// Tokens holds each token and peer value for the client, by name.
	Tokens map[string]string
	// Rule is the index of the deciding rule in the host's list, or -1
	// when no rule decided.
	Rule int
	// Candidates are the targets the deciding rule picked between.
	Candidates []Candidate
	// Target is the client's target, given even when it is disabled.
	Target string
}

// Candidate is a target as rendered for the client, with its weight.
type Candidate struct {
	Target string
	Weight int
}

// Resolve returns the target hostname for a client at ip requesting host.
// The first matching rule decides. If the client's target there is
// disabled, Resolve returns ErrBlackholed: disabling a target drops its own
// clients and moves nobody else. A matching rule is passed over only when
// none of its targets renders to a safe hostname for this client.
func (r *Router) Resolve(host string, ip netip.Addr) (string, error) {
	d, err := r.decide(host, ip)
	if err != nil {
		return "", err
	}
	return d.target, nil
}

// Explain is Resolve with its working shown, for debugging a route. It
// returns the same errors as Resolve. With ErrBlackholed, Target names the
// disabled target; with ErrNoTarget, only Tokens is set.
func (r *Router) Explain(host string, ip netip.Addr) (Decision, error) {
	got, err := r.decide(host, ip)
	d := Decision{Tokens: make(map[string]string, len(got.vals)), Rule: got.rule, Target: got.target}
	for i, v := range got.vals {
		d.Tokens[r.names[i]] = v
	}
	for _, t := range got.targets {
		if n := t.render(got.vals); r.safe(n) {
			d.Candidates = append(d.Candidates, Candidate{Target: n, Weight: t.weight})
		}
	}
	return d, err
}

type database struct {
	reader *maxminddb.Reader
	tokens []token
}

type token struct {
	index int
	path  []any
	def   string
}

type peer struct {
	index  int               // where the label goes in the token values
	token  int               // the token it labels
	labels map[string]string // token value to label
	def    string
}

type rule struct {
	when    []cond
	targets []target
}

type cond struct {
	token  int
	values []string
}

// decision is what Resolve and Explain share, so that the two can never
// disagree. Resolve builds nothing else, which keeps it as cheap as before.
type decision struct {
	vals    []string
	rule    int      // index of the deciding rule, or -1 if none decided
	targets []target // the deciding rule's
	// target is the client's target, set even when it is disabled.
	target string
}

func (r *Router) decide(host string, ip netip.Addr) (decision, error) {
	host = strings.ToLower(host)
	d := decision{rule: -1}
	rules, ok := r.hosts[host]
	if !ok {
		return d, fmt.Errorf("%w: %s", ErrUnknownHost, host)
	}
	ip = ip.Unmap()
	d.vals = r.tokens(ip)
	key := clientKey(host, ip)
	for i, rl := range rules {
		if !rl.matches(d.vals) {
			continue
		}
		t, disabled, ok := r.pick(rl.targets, d.vals, key)
		if !ok {
			continue
		}
		d.rule, d.targets, d.target = i, rl.targets, t
		if disabled {
			return d, fmt.Errorf("%w: %s", ErrBlackholed, t)
		}
		return d, nil
	}
	return d, fmt.Errorf("%w for %s", ErrNoTarget, host)
}

// tokens looks ip up once per database and reads each token from the
// record. A missing record, a missing field or a value that is not a DNS
// label gives the token's default, so bad data can never shape a hostname.
func (r *Router) tokens(ip netip.Addr) []string {
	vals := make([]string, len(r.names))
	for _, db := range r.dbs {
		res := db.reader.Lookup(ip)
		for _, t := range db.tokens {
			vals[t.index] = t.def
			if !res.Found() {
				continue
			}
			var v any
			if err := res.DecodePath(&v, t.path...); err != nil {
				continue
			}
			if s, ok := label(v); ok {
				vals[t.index] = s
			}
		}
	}
	for _, p := range r.peers {
		vals[p.index] = p.def
		if l, ok := p.labels[vals[p.token]]; ok {
			vals[p.index] = l
		}
	}
	return vals
}

func (rl rule) matches(vals []string) bool {
	for _, c := range rl.when {
		if !slices.Contains(c.values, vals[c.token]) {
			return false
		}
	}
	return true
}

// pick returns the client's target among those that render to a safe
// hostname, and whether it is disabled. Disabled targets stay in the pick so
// that disabling one never moves another target's clients.
func (r *Router) pick(targets []target, vals []string, key uint64) (name string, disabled, ok bool) {
	bestScore := -1.0
	for _, t := range targets {
		n := t.render(vals)
		if !r.safe(n) {
			continue
		}
		if s := score(key, n, t.weight); s > bestScore {
			name, bestScore = n, s
			disabled = r.disabled[t.template] || r.disabled[n]
		}
	}
	return name, disabled, name != ""
}

// safe reports whether a rendered target may be redirected to.
func (r *Router) safe(name string) bool {
	return isHostname(name) && r.underSuffix(name)
}

func (r *Router) underSuffix(host string) bool {
	for _, s := range r.suffixes {
		if strings.HasSuffix(host, s) {
			return true
		}
	}
	return false
}

// label turns a decoded MMDB value into a lowercase DNS label.
func label(v any) (string, bool) {
	var s string
	switch v := v.(type) {
	case string:
		s = v
	case bool, int, int32, int64, uint16, uint32, uint64, float32, float64:
		s = fmt.Sprint(v)
	default:
		return "", false
	}
	s = strings.ToLower(s)
	return s, isLabel(s)
}
