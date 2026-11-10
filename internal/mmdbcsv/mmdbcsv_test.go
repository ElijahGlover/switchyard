package mmdbcsv_test

import (
	"bytes"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/ElijahGlover/switchyard/internal/mmdbcsv"
	"github.com/oschwald/maxminddb-golang/v2"
)

func build(t *testing.T, csv string) *maxminddb.Reader {
	t.Helper()
	var buf bytes.Buffer
	if err := mmdbcsv.Build(strings.NewReader(csv), &buf); err != nil {
		t.Fatalf("Build: %v", err)
	}
	db, err := maxminddb.OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatalf("OpenBytes: %v", err)
	}
	return db
}

func lookup(t *testing.T, db *maxminddb.Reader, ip string) map[string]string {
	t.Helper()
	res := db.Lookup(netip.MustParseAddr(ip))
	if !res.Found() {
		return nil
	}
	var rec map[string]string
	if err := res.Decode(&rec); err != nil {
		t.Fatalf("Decode %s: %v", ip, err)
	}
	return rec
}

func TestBuildMapsColumnsToFields(t *testing.T) {
	db := build(t, `prefix,network,location
203.0.113.0/24,onnet,bne
2001:db8:100::/40,onnet,mel
`)
	tests := []struct {
		name, ip string
		want     map[string]string
	}{
		{"IPv4", "203.0.113.7", map[string]string{"network": "onnet", "location": "bne"}},
		{"IPv6", "2001:db8:100::1", map[string]string{"network": "onnet", "location": "mel"}},
		{"unlisted address", "198.51.100.1", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lookup(t, db, tt.ip); !equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildMostSpecificPrefixWinsWhateverTheOrder(t *testing.T) {
	db := build(t, `prefix,location
203.0.113.0/25,bne
203.0.113.0/24,syd
`)
	tests := []struct{ name, ip, want string }{
		{"inside the /25", "203.0.113.7", "bne"},
		{"rest of the /24", "203.0.113.200", "syd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lookup(t, db, tt.ip)["location"]; got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildEmptyCellOmitsField(t *testing.T) {
	db := build(t, `prefix,network,location
203.0.113.0/24,onnet,
`)
	got := lookup(t, db, "203.0.113.7")
	if _, ok := got["location"]; ok || got["network"] != "onnet" {
		t.Errorf("record = %v, want only network=onnet", got)
	}
}

func TestBuildRejectsBadInput(t *testing.T) {
	tests := []struct {
		name, csv, want string
	}{
		{"empty", "", "empty input"},
		{"no fields", "prefix\n203.0.113.0/24\n", "at least one field"},
		{"first column", "network,prefix\nonnet,203.0.113.0/24\n", `first column must be "prefix"`},
		{"duplicate column", "prefix,a,a\n203.0.113.0/24,x,y\n", `duplicate column "a"`},
		{"blank column", "prefix, \n203.0.113.0/24,x\n", "column 2 has no name"},
		{"bad prefix", "prefix,a\nnot-a-prefix,x\n", "line 2"},
		{"host bits", "prefix,a\n203.0.113.7/24,x\n", "use 203.0.113.0/24"},
		{"duplicate prefix", "prefix,a\n203.0.113.0/24,x\n203.0.113.0/24,y\n", "already listed on line 2"},
		{"short row", "prefix,a,b\n203.0.113.0/24,x\n", "line 2"},
		{"no rows", "prefix,a\n", "no networks"},
		{"unparseable header", "\"prefix,a\n", "quote"},
		{"IPv4-mapped prefix", "prefix,a\n::ffff:203.0.113.0/120,x\n", "insert ::ffff:203.0.113.0/120"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mmdbcsv.Build(strings.NewReader(tt.csv), &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Build error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestBuildReportsWriteErrors(t *testing.T) {
	err := mmdbcsv.Build(strings.NewReader("prefix,a\n203.0.113.0/24,x\n"), failingWriter{})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("got %v, want the write error", err)
	}
}

func equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
