package main

import (
	"bytes"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oschwald/maxminddb-golang/v2"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunWritesADatabase(t *testing.T) {
	dir := t.TempDir()
	in, out := filepath.Join(dir, "netmap.csv"), filepath.Join(dir, "netmap.mmdb")
	writeFile(t, in, "prefix,location\n203.0.113.0/24,bne\n")

	if err := run(in, out); err != nil {
		t.Fatalf("run: %v", err)
	}
	db, err := maxminddb.Open(out)
	if err != nil {
		t.Fatalf("open %s: %v", out, err)
	}
	defer db.Close()
	var rec struct {
		Location string `maxminddb:"location"`
	}
	if err := db.Lookup(netip.MustParseAddr("203.0.113.7")).Decode(&rec); err != nil || rec.Location != "bne" {
		t.Errorf("got %q, %v; want bne", rec.Location, err)
	}
}

func badCSV(t *testing.T) (in, out string) {
	t.Helper()
	dir := t.TempDir()
	in, out = filepath.Join(dir, "netmap.csv"), filepath.Join(dir, "netmap.mmdb")
	writeFile(t, out, "previous database")
	writeFile(t, in, "prefix,location\nnot-a-prefix,bne\n")
	return in, out
}

func TestRunNamesTheBadCSV(t *testing.T) {
	in, out := badCSV(t)
	if err := run(in, out); err == nil || !strings.Contains(err.Error(), in) {
		t.Errorf("got %v, want an error naming %s", err, in)
	}
}

func TestRunKeepsTheOldDatabaseWhenTheCSVIsBad(t *testing.T) {
	in, out := badCSV(t)
	run(in, out)
	if got, _ := os.ReadFile(out); !bytes.Equal(got, []byte("previous database")) {
		t.Errorf("bad CSV overwrote %s with %q", out, got)
	}
}
