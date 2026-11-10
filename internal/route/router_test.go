package route_test

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/ElijahGlover/switchyard/internal/mmdbcsv"
	"github.com/ElijahGlover/switchyard/internal/route"
	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

const netmapCSV = `prefix,network,location
203.0.113.0/25,onnet,bne
203.0.113.128/25,onnet,Bris Bane
198.51.100.0/24,onnet,
198.51.100.0/25,onnet,PER
2001:db8:100::/40,onnet,mel
`

const asnCSV = `prefix,asn
192.0.2.0/25,AS64500
2001:db8:200::/40,AS64501
`

const baseConfig = `
databases:
  asn: asn.mmdb
  netmap: netmap.mmdb
tokens:
  asn:      { db: asn,    path: [asn],      default: none }
  network:  { db: netmap, path: [network],  default: offnet }
  location: { db: netmap, path: [location], default: syd }
allowed_suffixes: [.example.com, .example.net]
hosts:
  video.example.com:
    - when: { network: onnet }
      targets: { "{location}.edge.example.com": 100 }
    - when: { asn: [AS64500, AS64501] }
      targets: { cdn-b.example.net: 100 }
    - targets: { cdn-a.example.net: 70, cdn-b.example.net: 30 }
`

func mmdb(t testing.TB, csv string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := mmdbcsv.Build(strings.NewReader(csv), &buf); err != nil {
		t.Fatalf("mmdbcsv.Build: %v", err)
	}
	return buf.Bytes()
}

func fixtures(t testing.TB) map[string][]byte {
	return map[string][]byte{"asn": mmdb(t, asnCSV), "netmap": mmdb(t, netmapCSV)}
}

func newRouter(t testing.TB, yaml string, dbs map[string][]byte) *route.Router {
	t.Helper()
	cfg, err := route.ParseConfig([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	r, err := route.New(cfg, dbs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func resolve(t *testing.T, r *route.Router, host, ip string) string {
	t.Helper()
	got, err := r.Resolve(host, netip.MustParseAddr(ip))
	if err != nil {
		t.Fatalf("Resolve(%s, %s): %v", host, ip, err)
	}
	return got
}

func TestResolve(t *testing.T) {
	r := newRouter(t, baseConfig, fixtures(t))
	tests := []struct {
		name, host, ip, want string
	}{
		{"custom tokens build the hostname", "video.example.com", "203.0.113.7", "bne.edge.example.com"},
		{"missing field takes the default", "video.example.com", "198.51.100.200", "syd.edge.example.com"},
		{"token values are lowercased", "video.example.com", "198.51.100.7", "per.edge.example.com"},
		{"value that is not a DNS label takes the default", "video.example.com", "203.0.113.200", "syd.edge.example.com"},
		{"IPv6", "video.example.com", "2001:db8:100::1", "mel.edge.example.com"},
		{"IPv4-mapped IPv6", "video.example.com", "::ffff:203.0.113.7", "bne.edge.example.com"},
		{"ASN rule matches case-insensitively", "video.example.com", "192.0.2.10", "cdn-b.example.net"},
		{"ASN list matches IPv6", "video.example.com", "2001:db8:200::1", "cdn-b.example.net"},
		{"host is case-insensitive", "VIDEO.Example.com", "203.0.113.7", "bne.edge.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolve(t, r, tt.host, tt.ip); got != tt.want {
				t.Errorf("Resolve(%s, %s) = %s, want %s", tt.host, tt.ip, got, tt.want)
			}
		})
	}
}

func TestResolveUnknownHost(t *testing.T) {
	r := newRouter(t, baseConfig, fixtures(t))
	_, err := r.Resolve("other.example.com", netip.MustParseAddr("203.0.113.7"))
	if !errors.Is(err, route.ErrUnknownHost) {
		t.Errorf("err = %v, want ErrUnknownHost", err)
	}
}

func TestResolveBlackholesDisabledTargets(t *testing.T) {
	tests := []struct {
		name, disabled, ip string
	}{
		{"a rendered name blackholes one PoP", "[bne.edge.example.com]", "203.0.113.7"},
		{"a template blackholes every PoP", `["{location}.edge.example.com"]`, "2001:db8:100::1"},
		{"no falling through to a later rule", "[cdn-b.example.net]", "192.0.2.10"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRouter(t, baseConfig+"disabled: "+tt.disabled+"\n", fixtures(t))
			got, err := r.Resolve("video.example.com", netip.MustParseAddr(tt.ip))
			if !errors.Is(err, route.ErrBlackholed) {
				t.Errorf("got %q, %v; want ErrBlackholed", got, err)
			}
		})
	}
}

func TestResolveKeepsServingTargetsThatAreNotDisabled(t *testing.T) {
	r := newRouter(t, baseConfig+"disabled: [bne.edge.example.com]\n", fixtures(t))
	if got := resolve(t, r, "video.example.com", "2001:db8:100::1"); got != "mel.edge.example.com" {
		t.Errorf("got %s, want mel.edge.example.com", got)
	}
}

func TestResolveNoTargetWhenNoRuleMatches(t *testing.T) {
	r := newRouter(t, `
databases: { netmap: netmap.mmdb }
tokens: { network: { db: netmap, path: [network], default: offnet } }
allowed_suffixes: [.example.com]
hosts:
  video.example.com:
    - when: { network: onnet }
      targets: { edge.example.com: 1 }
`, fixtures(t))
	_, err := r.Resolve("video.example.com", netip.MustParseAddr("192.0.2.200"))
	if !errors.Is(err, route.ErrNoTarget) {
		t.Errorf("got %v, want ErrNoTarget", err)
	}
}

// clients returns n addresses that match no database, so they reach the
// weighted commercial rule.
func clients(n int) []netip.Addr {
	ips := make([]netip.Addr, n)
	ip := netip.MustParseAddr("2001:db8:ff00::")
	for i := range ips {
		ip = ip.Next()
		ips[i] = ip
	}
	return ips
}

func TestResolveHonoursWeights(t *testing.T) {
	r := newRouter(t, baseConfig, fixtures(t))
	const n = 20000
	counts := map[string]int{}
	for _, ip := range clients(n) {
		got, err := r.Resolve("video.example.com", ip)
		if err != nil {
			t.Fatal(err)
		}
		counts[got]++
	}
	share := float64(counts["cdn-a.example.net"]) / n
	if share < 0.68 || share > 0.72 {
		t.Errorf("cdn-a share = %.3f, want 0.70 ± 0.02 (counts %v)", share, counts)
	}
}

// TestResolveIsStable pins clients to targets. Every Lambda container must
// give a client the same answer, so the hash cannot be seeded per process.
// If this fails after changing the hash, every viewer will move CDN on
// deploy: update the table only if that is intended.
func TestResolveIsStable(t *testing.T) {
	r := newRouter(t, baseConfig, fixtures(t))
	golden := []struct{ ip, want string }{
		{"192.0.2.129", "cdn-a.example.net"},
		{"192.0.2.130", "cdn-a.example.net"},
		{"192.0.2.131", "cdn-a.example.net"},
		{"192.0.2.132", "cdn-b.example.net"},
		{"192.0.2.133", "cdn-a.example.net"},
		{"192.0.2.134", "cdn-b.example.net"},
		{"192.0.2.135", "cdn-a.example.net"},
		{"192.0.2.136", "cdn-b.example.net"},
		{"192.0.2.137", "cdn-b.example.net"},
		{"192.0.2.138", "cdn-a.example.net"},
		{"192.0.2.139", "cdn-a.example.net"},
		{"192.0.2.140", "cdn-a.example.net"},
	}
	for _, g := range golden {
		t.Run(g.ip, func(t *testing.T) {
			if got := resolve(t, r, "video.example.com", g.ip); got != g.want {
				t.Errorf("got %s, want %s", got, g.want)
			}
		})
	}
}

// TestResolveDisablingBlackholesOnlyThatTargetsClients: a blackhole drops
// the disabled target's own clients and moves nobody else, so no other
// target takes on load it was not sized for.
func TestResolveDisablingBlackholesOnlyThatTargetsClients(t *testing.T) {
	cfg := `
allowed_suffixes: [.example.net]
hosts:
  video.example.com:
    - targets: { a.example.net: 50, b.example.net: 30, c.example.net: 20 }
`
	before := newRouter(t, cfg, nil)
	after := newRouter(t, cfg+"disabled: [c.example.net]\n", nil)
	blackholed := 0
	for _, ip := range clients(5000) {
		was, _ := before.Resolve("video.example.com", ip)
		now, err := after.Resolve("video.example.com", ip)
		switch {
		case was == "c.example.net" && !errors.Is(err, route.ErrBlackholed):
			t.Fatalf("%s was on c and got %q, %v; want ErrBlackholed", ip, now, err)
		case was == "c.example.net":
			blackholed++
		case now != was:
			t.Fatalf("%s moved from %s to %q (%v), but only c was disabled", ip, was, now, err)
		}
	}
	if blackholed == 0 {
		t.Error("no client was on c.example.net, so the test proved nothing")
	}
}

const peerCSV = `prefix,asn
192.0.2.0/25,AS64500
192.0.2.128/25,AS64501
198.51.100.0/24,AS64496
203.0.113.0/24,AS64499
`

const peerConfig = `
databases: { asn: asn.mmdb }
tokens: { asn: { db: asn, path: [asn], default: none } }
peers:
  peer:
    token: asn
    default: other
    labels:
      partner: [AS64500, AS64501]
      vpn: AS64496
allowed_suffixes: [.example.com]
hosts:
  video.example.com:
    - when: { peer: partner }
      targets: { partner.example.com: 1 }
    - targets: { "{peer}.edge.example.com": 1 }
`

func TestResolvePeersLabelTheirASNs(t *testing.T) {
	r := newRouter(t, peerConfig, map[string][]byte{"asn": mmdb(t, peerCSV)})
	tests := []struct {
		name, ip, want string
	}{
		{"an ASN of the peer matches its label", "192.0.2.10", "partner.example.com"},
		{"so does every other ASN of it", "192.0.2.200", "partner.example.com"},
		{"a label can shape a hostname", "198.51.100.7", "vpn.edge.example.com"},
		{"an ASN of no peer gets the default", "203.0.113.7", "other.edge.example.com"},
		{"no ASN at all gets the default", "2001:db8:ff00::1", "other.edge.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolve(t, r, "video.example.com", tt.ip); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

// typedMMDB builds a database with non-string fields, the way third-party
// databases store ASNs and nested records.
func typedMMDB(t testing.TB, prefix string, record mmdbtype.Map) []byte {
	t.Helper()
	tree, err := mmdbwriter.New(mmdbwriter.Options{IncludeReservedNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	_, network, err := net.ParseCIDR(prefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Insert(network, record); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := tree.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestResolveReadsTypedFields(t *testing.T) {
	geo := typedMMDB(t, "203.0.113.0/24", mmdbtype.Map{
		"autonomous_system_number": mmdbtype.Uint32(64500),
		"subdivisions":             mmdbtype.Slice{mmdbtype.Map{"iso_code": mmdbtype.String("QLD")}},
		"network":                  mmdbtype.String("onnet"),
	})
	r := newRouter(t, `
databases: { geo: geo.mmdb }
tokens:
  asn:   { db: geo, path: [autonomous_system_number], default: none }
  state: { db: geo, path: [subdivisions, 0, iso_code], default: xx }
  deep:  { db: geo, path: [network, name], default: unset }
allowed_suffixes: [.example.com]
hosts:
  asn.example.com:
    - when: { asn: "64500" }
      targets: { matched.example.com: 1 }
    - targets: { unmatched.example.com: 1 }
  state.example.com:
    - targets: { "{state}.edge.example.com": 1 }
  deep.example.com:
    - targets: { "{deep}.edge.example.com": 1 }
`, map[string][]byte{"geo": geo})

	tests := []struct {
		name, host, want string
	}{
		{"a number becomes a token", "asn.example.com", "matched.example.com"},
		{"a number in the path indexes an array", "state.example.com", "qld.edge.example.com"},
		{"a path through a string takes the default", "deep.example.com", "unset.edge.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolve(t, r, tt.host, "203.0.113.7"); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestResolveNeverLetsDataShapeAnUnsafeTarget(t *testing.T) {
	long := strings.Repeat("a", 60) // a valid label, but "edge-" makes it 65
	netmap := mmdb(t, "prefix,zone,pop\n"+
		"203.0.113.0/24,net,bne\n"+
		"198.51.100.0/24,com,"+long+"\n")
	r := newRouter(t, `
databases: { netmap: netmap.mmdb }
tokens:
  zone: { db: netmap, path: [zone], default: com }
  pop:  { db: netmap, path: [pop],  default: syd }
allowed_suffixes: [.example.com]
hosts:
  zone.example.com:
    - targets: { "cdn.example.{zone}": 1 }
    - targets: { fallback.example.com: 1 }
  pop.example.com:
    - targets: { "edge-{pop}.example.com": 1 }
    - targets: { fallback.example.com: 1 }
  long.example.com:
    - targets: { "{pop}.{pop}.{pop}.{pop}.example.com": 1 }
    - targets: { fallback.example.com: 1 }
`, map[string][]byte{"netmap": netmap})

	tests := []struct {
		name, host, ip, want string
	}{
		{"data inside the suffix is used", "zone.example.com", "198.51.100.7", "cdn.example.com"},
		{"data that leaves the suffix falls through", "zone.example.com", "203.0.113.7", "fallback.example.com"},
		{"short label is used", "pop.example.com", "203.0.113.7", "edge-bne.example.com"},
		{"label made too long falls through", "pop.example.com", "198.51.100.7", "fallback.example.com"},
		{"short hostname is used", "long.example.com", "203.0.113.7", "bne.bne.bne.bne.example.com"},
		{"hostname over 253 characters falls through", "long.example.com", "198.51.100.7", "fallback.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolve(t, r, tt.host, tt.ip); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNewRejects(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{"unknown database", `
tokens: { pop: { db: nope, path: [pop], default: syd } }`, `tokens.pop: unknown database "nope"`},
		{"database not loaded", `
databases: { extra: extra.mmdb }`, "databases.extra: not loaded"},
		{"token name", `
databases: { netmap: netmap.mmdb }
tokens: { Pop: { db: netmap, path: [pop], default: syd } }`, "tokens.Pop: name must"},
		{"token without path", `
databases: { netmap: netmap.mmdb }
tokens: { pop: { db: netmap, default: syd } }`, "tokens.pop: path is empty"},
		{"default not a label", `
databases: { netmap: netmap.mmdb }
tokens: { pop: { db: netmap, path: [pop], default: "not valid" } }`, "tokens.pop: default"},
		{"suffix without dot", `
allowed_suffixes: [example.net]`, `allowed_suffixes: "example.net" must start with a dot`},
		{"host without rules", `
allowed_suffixes: [.example.net]
hosts: { video.example.com: [] }`, "hosts.video.example.com: no rules"},
		{"rule without targets", `
allowed_suffixes: [.example.net]
hosts: { video.example.com: [ { targets: {} } ] }`, "hosts.video.example.com[0]: no targets"},
		{"weight", `
allowed_suffixes: [.example.net]
hosts: { video.example.com: [ { targets: { a.example.net: 0 } } ] }`, "weight must be at least 1"},
		{"when unknown token", `
allowed_suffixes: [.example.net]
hosts: { video.example.com: [ { when: { pop: syd }, targets: { a.example.net: 1 } } ] }`, `when: unknown token "pop"`},
		{"when value can never match", `
databases: { netmap: netmap.mmdb }
tokens: { pop: { db: netmap, path: [pop], default: syd } }
allowed_suffixes: [.example.net]
hosts: { video.example.com: [ { when: { pop: "AS 64500" }, targets: { a.example.net: 1 } } ] }`, "can never match"},
		{"template unknown token", `
allowed_suffixes: [.example.net]
hosts: { video.example.com: [ { targets: { "{pop}.example.net": 1 } } ] }`, `unknown token "pop"`},
		{"template unclosed", `
databases: { netmap: netmap.mmdb }
tokens: { pop: { db: netmap, path: [pop], default: syd } }
allowed_suffixes: [.example.net]
hosts: { video.example.com: [ { targets: { "{pop.example.net": 1 } } ] }`, "unclosed {"},
		{"empty token name", `
databases: { netmap: netmap.mmdb }
tokens: { "": { db: netmap, path: [pop], default: syd } }`, "tokens.: name must"},
		{"invalid host", `
allowed_suffixes: [.example.net]
hosts: { video_example.com: [ { targets: { a.example.net: 1 } } ] }`, "hosts.video_example.com: not a valid hostname"},
		{"host listed twice", `
allowed_suffixes: [.example.net]
hosts:
  video.example.com: [ { targets: { a.example.net: 1 } } ]
  VIDEO.example.com: [ { targets: { a.example.net: 1 } } ]`, "listed twice"},
		{"template stray brace", `
allowed_suffixes: [.example.net]
hosts: { video.example.com: [ { targets: { "pop}.example.net": 1 } } ] }`, "unexpected }"},
		{"template renders an invalid hostname", `
databases: { netmap: netmap.mmdb }
tokens: { pop: { db: netmap, path: [pop], default: syd } }
allowed_suffixes: [.example.net]
hosts: { video.example.com: [ { targets: { "{pop}-.example.net": 1 } } ] }`, `renders "syd-.example.net" with token defaults, which is not a valid hostname`},
		{"peer named like a token", `
databases: { netmap: netmap.mmdb }
tokens: { pop: { db: netmap, path: [pop], default: syd } }
peers: { pop: { token: pop, default: other, labels: { east: [bne] } } }`, "peers.pop: a token has the same name"},
		{"peer name", `
databases: { netmap: netmap.mmdb }
tokens: { pop: { db: netmap, path: [pop], default: syd } }
peers: { Region: { token: pop, default: other, labels: { east: [bne] } } }`, "peers.Region: name must"},
		{"peer of an unknown token", `
peers: { region: { token: pop, default: other, labels: { east: [bne] } } }`, `peers.region: unknown token "pop"`},
		{"peer default not a label", `
databases: { netmap: netmap.mmdb }
tokens: { pop: { db: netmap, path: [pop], default: syd } }
peers: { region: { token: pop, default: "not valid", labels: { east: [bne] } } }`, "peers.region: default"},
		{"peer without labels", `
databases: { netmap: netmap.mmdb }
tokens: { pop: { db: netmap, path: [pop], default: syd } }
peers: { region: { token: pop, default: other } }`, "peers.region: no labels"},
		{"peer label not a DNS label", `
databases: { netmap: netmap.mmdb }
tokens: { pop: { db: netmap, path: [pop], default: syd } }
peers: { region: { token: pop, default: other, labels: { "East Coast": [bne] } } }`, `peers.region.labels: "East Coast" is not a DNS label`},
		{"peer value that can never match", `
databases: { netmap: netmap.mmdb }
tokens: { pop: { db: netmap, path: [pop], default: syd } }
peers: { region: { token: pop, default: other, labels: { east: ["b n e"] } } }`, `peers.region.labels.east: "b n e" can never match`},
		{"value under two labels", `
databases: { netmap: netmap.mmdb }
tokens: { pop: { db: netmap, path: [pop], default: syd } }
peers: { region: { token: pop, default: other, labels: { east: [bne, syd], south: [SYD] } } }`, `peers.region: "syd" is under both east and south`},
		{"when on a label the peer lacks", `
databases: { netmap: netmap.mmdb }
tokens: { pop: { db: netmap, path: [pop], default: syd } }
peers: { region: { token: pop, default: other, labels: { east: [bne] } } }
allowed_suffixes: [.example.net]
hosts: { video.example.com: [ { when: { region: est }, targets: { a.example.net: 1 } } ] }`, `when.region: "est" can never match: region is east or other`},
		{"target outside allowed suffixes", `
allowed_suffixes: [.example.net]
hosts: { video.example.com: [ { targets: { cdn.example.com: 1 } } ] }`, "not under an allowed suffix"},
	}
	dbs := map[string][]byte{"netmap": mmdb(t, netmapCSV)}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := route.ParseConfig([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			_, err = route.New(cfg, dbs)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("New error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestNewRejectsCorruptDatabase(t *testing.T) {
	cfg, err := route.ParseConfig([]byte(`
databases: { netmap: netmap.mmdb }
tokens: { pop: { db: netmap, path: [pop], default: syd } }
`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = route.New(cfg, map[string][]byte{"netmap": []byte("not an mmdb")})
	if err == nil || !strings.Contains(err.Error(), "databases.netmap") {
		t.Errorf("New error = %v, want a databases.netmap error", err)
	}
}

func BenchmarkResolve(b *testing.B) {
	r := newRouter(b, baseConfig, fixtures(b))
	ips := []netip.Addr{
		netip.MustParseAddr("203.0.113.7"),
		netip.MustParseAddr("192.0.2.10"),
		netip.MustParseAddr("192.0.2.200"),
		netip.MustParseAddr("2001:db8:100::1"),
	}
	for i := 0; b.Loop(); i++ {
		if _, err := r.Resolve("video.example.com", ips[i%len(ips)]); err != nil {
			b.Fatal(fmt.Errorf("Resolve: %w", err))
		}
	}
}
