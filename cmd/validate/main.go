// Command validate checks a config and its databases with the same rules the
// router applies before using them, and optionally resolves one request.
//
//	validate examples/config.yaml
//	validate -host video.example.com -ip 203.0.113.7 examples/config.yaml
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/ElijahGlover/switchyard/internal/route"
)

func main() {
	host := flag.String("host", "", "hostname to resolve (needs -ip)")
	ip := flag.String("ip", "", "client IP to resolve (needs -host)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: validate [-host name -ip addr] config.yaml")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(flag.Arg(0), *host, *ip, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(file, host, ip string, w io.Writer) error {
	if (host == "") != (ip == "") {
		return errors.New("-host and -ip go together")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	cfg, err := route.ParseConfig(b)
	if err != nil {
		return err
	}
	dbs := make(map[string][]byte, len(cfg.Databases))
	for name, rel := range cfg.Databases {
		b, err := os.ReadFile(filepath.Join(filepath.Dir(file), filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		dbs[name] = b
	}
	r, err := route.New(cfg, dbs)
	if err != nil {
		return err
	}

	if host == "" {
		_, err := fmt.Fprintf(w, "ok: %d hosts, %d tokens, %d peers, %d databases\n", len(cfg.Hosts), len(cfg.Tokens), len(cfg.Peers), len(cfg.Databases))
		return err
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return err
	}
	target, err := r.Resolve(host, addr)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, target)
	return err
}
