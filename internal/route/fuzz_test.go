package route_test

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ElijahGlover/switchyard/internal/mmdbcsv"
	"github.com/ElijahGlover/switchyard/internal/route"
)

const fuzzConfig = `
databases: { netmap: netmap.mmdb }
tokens: { zone: { db: netmap, path: [zone], default: com } }
allowed_suffixes: [.example.com]
hosts:
  video.example.com:
    - targets: { "cdn.example.{zone}": 1, "{zone}.edge.example.com": 1 }
    - targets: { fallback.example.com: 1 }
`

// safeTarget is written independently of the router's own checks: lowercase
// DNS labels of at most 63 characters, ending in .example.com.
var safeTarget = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+example\.com$`)

// zoneDB is the netmap database for fuzzConfig, holding value as the zone
// for 203.0.113.0/24. A value no database can hold skips the input.
func zoneDB(t *testing.T, value string) map[string][]byte {
	t.Helper()
	var in bytes.Buffer
	w := csv.NewWriter(&in)
	w.Write([]string{"prefix", "zone"})
	w.Write([]string{"203.0.113.0/24", value})
	w.Flush()
	var db bytes.Buffer
	if err := mmdbcsv.Build(&in, &db); err != nil {
		t.Skip("not a buildable database:", err)
	}
	return map[string][]byte{"netmap": db.Bytes()}
}

// zoneSeeds are database values at the edges of what a token accepts.
var zoneSeeds = []string{
	"bne", "BNE", "com", "net", "example.net", "a.b", "-x", "x-", " padded ", "",
	strings.Repeat("a", 63), strings.Repeat("a", 64), "x/../y", "İstanbul", "a\x00b", "{zone}",
}

// FuzzResolveNeverLeavesAllowedSuffixes is the property behind the safety
// claim in the README: whatever string a database holds, the answer is a
// valid hostname under an allowed suffix.
func FuzzResolveNeverLeavesAllowedSuffixes(f *testing.F) {
	for _, seed := range zoneSeeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		r := newRouter(t, fuzzConfig, zoneDB(t, value))
		got, err := r.Resolve("video.example.com", netip.MustParseAddr("203.0.113.7"))
		if err != nil {
			t.Fatalf("value %q: %v", value, err)
		}
		if !safeTarget.MatchString(got) || len(got) > 253 {
			t.Errorf("value %q produced unsafe target %q", value, got)
		}
	})
}

// FuzzExplainMatchesResolve checks that Explain shows the choice Resolve
// makes: the same answer, picked from candidates that are all safe. The
// disabled target is what the token's default renders to.
func FuzzExplainMatchesResolve(f *testing.F) {
	for _, seed := range zoneSeeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		r := newRouter(t, fuzzConfig+"disabled: [cdn.example.com]\n", zoneDB(t, value))
		ip := netip.MustParseAddr("203.0.113.7")
		want, wantErr := r.Resolve("video.example.com", ip)
		got, err := r.Explain("video.example.com", ip)

		if fmt.Sprint(err) != fmt.Sprint(wantErr) {
			t.Fatalf("value %q: Explain error %v, Resolve error %v", value, err, wantErr)
		}
		if wantErr == nil && got.Target != want {
			t.Errorf("value %q: Explain target %q, Resolve target %q", value, got.Target, want)
		}
		if !slices.ContainsFunc(got.Candidates, func(c route.Candidate) bool { return c.Target == got.Target }) {
			t.Errorf("value %q: target %q is not among candidates %v", value, got.Target, got.Candidates)
		}
		for _, c := range got.Candidates {
			if !safeTarget.MatchString(c.Target) {
				t.Errorf("value %q: unsafe candidate %q", value, c.Target)
			}
		}
	})
}

// FuzzConfig checks that no config, however malformed, can panic the
// parser, the validator or a lookup.
func FuzzConfig(f *testing.F) {
	f.Add([]byte(baseConfig))
	f.Add([]byte(fuzzConfig))
	f.Add([]byte(`hosts: { a.example.com: [ { targets: { "{x": 1 } } ] }`))
	f.Add([]byte("tokens: { a: { db: netmap, path: [0, -1, x] } }"))
	dbs := fixtures(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		cfg, err := route.ParseConfig(b)
		if err != nil {
			return
		}
		r, err := route.New(cfg, dbs)
		if err != nil {
			return
		}
		for host := range cfg.Hosts {
			r.Resolve(host, netip.MustParseAddr("203.0.113.7"))
			r.Resolve(host, netip.MustParseAddr("2001:db8:100::1"))
		}
	})
}
