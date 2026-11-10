package route_test

import (
	"errors"
	"maps"
	"net/netip"
	"reflect"
	"slices"
	"testing"

	"github.com/ElijahGlover/switchyard/internal/route"
)

func TestExplainGivesEveryTokenAndPeer(t *testing.T) {
	r := newRouter(t, peerConfig, map[string][]byte{"asn": mmdb(t, peerCSV)})
	tests := []struct {
		name, ip string
		want     map[string]string
	}{
		{"values from the database and the peer's label", "192.0.2.10", map[string]string{"asn": "as64500", "peer": "partner"}},
		{"defaults when the database has nothing", "2001:db8:ff00::1", map[string]string{"asn": "none", "peer": "other"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.Explain("video.example.com", netip.MustParseAddr(tt.ip))
			if err != nil {
				t.Fatalf("Explain(%s): %v", tt.ip, err)
			}
			if !maps.Equal(got.Tokens, tt.want) {
				t.Errorf("Explain(%s) tokens = %v, want %v", tt.ip, got.Tokens, tt.want)
			}
		})
	}
}

func TestExplainGivesTheDecidingRule(t *testing.T) {
	r := newRouter(t, baseConfig, fixtures(t))
	tests := []struct {
		name, ip string
		want     int
	}{
		{"the first rule that matches", "203.0.113.7", 0},
		{"a later rule when earlier ones do not match", "192.0.2.10", 1},
		{"the rule with no conditions", "192.0.2.200", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.Explain("video.example.com", netip.MustParseAddr(tt.ip))
			if err != nil {
				t.Fatalf("Explain(%s): %v", tt.ip, err)
			}
			if got.Rule != tt.want {
				t.Errorf("Explain(%s) rule = %d, want %d", tt.ip, got.Rule, tt.want)
			}
		})
	}
}

func TestExplainGivesTheRulesCandidates(t *testing.T) {
	r := newRouter(t, baseConfig, fixtures(t))
	tests := []struct {
		name, ip string
		want     []route.Candidate
	}{
		{"each target with its weight", "192.0.2.200",
			[]route.Candidate{{Target: "cdn-a.example.net", Weight: 70}, {Target: "cdn-b.example.net", Weight: 30}}},
		{"targets as rendered for the client", "203.0.113.7",
			[]route.Candidate{{Target: "bne.edge.example.com", Weight: 100}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.Explain("video.example.com", netip.MustParseAddr(tt.ip))
			if err != nil {
				t.Fatalf("Explain(%s): %v", tt.ip, err)
			}
			if !slices.Equal(got.Candidates, tt.want) {
				t.Errorf("Explain(%s) candidates = %v, want %v", tt.ip, got.Candidates, tt.want)
			}
		})
	}
}

func TestExplainGivesTheTarget(t *testing.T) {
	r := newRouter(t, baseConfig+"disabled: [cdn-b.example.net]\n", fixtures(t))
	tests := []struct {
		name, ip, want string
		wantErr        error
	}{
		{"where the client is sent", "203.0.113.7", "bne.edge.example.com", nil},
		{"a disabled target, which the client is refused", "192.0.2.10", "cdn-b.example.net", route.ErrBlackholed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.Explain("video.example.com", netip.MustParseAddr(tt.ip))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Explain(%s) error = %v, want %v", tt.ip, err, tt.wantErr)
			}
			if got.Target != tt.want {
				t.Errorf("Explain(%s) target = %q, want %q", tt.ip, got.Target, tt.want)
			}
		})
	}
}

// The tokens still explain a client that no rule matched.
func TestExplainWhenNoRuleMatches(t *testing.T) {
	r := newRouter(t, `
databases: { netmap: netmap.mmdb }
tokens: { network: { db: netmap, path: [network], default: offnet } }
allowed_suffixes: [.example.com]
hosts:
  video.example.com:
    - when: { network: onnet }
      targets: { edge.example.com: 1 }
`, fixtures(t))

	got, err := r.Explain("video.example.com", netip.MustParseAddr("192.0.2.200"))
	if !errors.Is(err, route.ErrNoTarget) {
		t.Fatalf("got error %v, want ErrNoTarget", err)
	}

	want := route.Decision{Tokens: map[string]string{"network": "offnet"}, Rule: -1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// A target that renders to an unsafe hostname for this client is never in
// the pick, so it is not a candidate.
func TestExplainLeavesOutTargetsThatRenderUnsafely(t *testing.T) {
	netmap := mmdb(t, "prefix,zone\n203.0.113.0/24,net\n")
	r := newRouter(t, `
databases: { netmap: netmap.mmdb }
tokens: { zone: { db: netmap, path: [zone], default: com } }
allowed_suffixes: [.example.com]
hosts:
  video.example.com:
    - targets: { "cdn.example.{zone}": 1, "{zone}.edge.example.com": 1 }
`, map[string][]byte{"netmap": netmap})

	got, err := r.Explain("video.example.com", netip.MustParseAddr("203.0.113.7"))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	want := []route.Candidate{{Target: "net.edge.example.com", Weight: 1}}
	if !slices.Equal(got.Candidates, want) {
		t.Errorf("candidates = %v, want %v, without cdn.example.net", got.Candidates, want)
	}
}
