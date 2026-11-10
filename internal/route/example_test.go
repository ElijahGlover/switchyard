package route_test

import (
	"bytes"
	"fmt"
	"net/netip"
	"strings"

	"github.com/ElijahGlover/switchyard/internal/mmdbcsv"
	"github.com/ElijahGlover/switchyard/internal/route"
)

func ExampleRouter_Resolve() {
	var netmap bytes.Buffer
	if err := mmdbcsv.Build(strings.NewReader("prefix,location\n203.0.113.0/24,bne\n"), &netmap); err != nil {
		fmt.Println(err)
		return
	}
	cfg, err := route.ParseConfig([]byte(`
databases: { netmap: netmap.mmdb }
tokens: { location: { db: netmap, path: [location], default: syd } }
allowed_suffixes: [.example.com]
hosts:
  video.example.com:
    - targets: { "{location}.edge.example.com": 1 }
`))
	if err != nil {
		fmt.Println(err)
		return
	}
	r, err := route.New(cfg, map[string][]byte{"netmap": netmap.Bytes()})
	if err != nil {
		fmt.Println(err)
		return
	}

	for _, ip := range []string{"203.0.113.7", "198.51.100.7"} {
		target, err := r.Resolve("video.example.com", netip.MustParseAddr(ip))
		if err != nil {
			fmt.Println(err)
			continue
		}
		fmt.Println(ip, "->", target)
	}
	// Output:
	// 203.0.113.7 -> bne.edge.example.com
	// 198.51.100.7 -> syd.edge.example.com
}

func ExampleRouter_Explain() {
	var netmap bytes.Buffer
	if err := mmdbcsv.Build(strings.NewReader("prefix,network,location\n203.0.113.0/24,onnet,bne\n"), &netmap); err != nil {
		fmt.Println(err)
		return
	}
	cfg, err := route.ParseConfig([]byte(`
databases: { netmap: netmap.mmdb }
tokens:
  network:  { db: netmap, path: [network],  default: offnet }
  location: { db: netmap, path: [location], default: syd }
allowed_suffixes: [.example.com, .example.net]
hosts:
  video.example.com:
    - when: { network: onnet }
      targets: { "{location}.edge.example.com": 1 }
    - targets: { cdn-a.example.net: 70, cdn-b.example.net: 30 }
`))
	if err != nil {
		fmt.Println(err)
		return
	}
	r, err := route.New(cfg, map[string][]byte{"netmap": netmap.Bytes()})
	if err != nil {
		fmt.Println(err)
		return
	}

	d, err := r.Explain("video.example.com", netip.MustParseAddr("198.51.100.7"))
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("tokens:", d.Tokens)
	fmt.Println("rule:", d.Rule)
	fmt.Println("candidates:", d.Candidates)
	fmt.Println("target:", d.Target)
	// Output:
	// tokens: map[location:syd network:offnet]
	// rule: 1
	// candidates: [{cdn-a.example.net 70} {cdn-b.example.net 30}]
	// target: cdn-b.example.net
}

func ExampleNew() {
	cfg, err := route.ParseConfig([]byte(`
allowed_suffixes: [.example.com]
hosts:
  video.example.com:
    - when: { pop: syd }
      targets: { "{pop}.edge.example.com": 0 }
`))
	if err != nil {
		fmt.Println(err)
		return
	}
	// Every problem is reported at once.
	_, err = route.New(cfg, nil)
	fmt.Println(err)
	// Output:
	// hosts.video.example.com[0].when: unknown token "pop"
	// hosts.video.example.com[0].targets["{pop}.edge.example.com"]: weight must be at least 1
	// hosts.video.example.com[0].targets["{pop}.edge.example.com"]: unknown token "pop"
}
