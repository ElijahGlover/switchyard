package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElijahGlover/switchyard/internal/mmdbcsv"
)

func writeFixture(t *testing.T, config string) string {
	t.Helper()
	dir := t.TempDir()
	var db bytes.Buffer
	if err := mmdbcsv.Build(strings.NewReader("prefix,location\n203.0.113.0/24,bne\n"), &db); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "netmap.mmdb"), db.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(file, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

const config = `
databases: { netmap: data/netmap.mmdb }
tokens: { location: { db: netmap, path: [location], default: syd } }
peers: { region: { token: location, default: other, labels: { east: [bne] } } }
allowed_suffixes: [.example.com]
hosts:
  video.example.com:
    - targets: { "{location}.edge.example.com": 1 }
`

func TestRun(t *testing.T) {
	file := writeFixture(t, config)
	tests := []struct {
		name, host, ip, want string
	}{
		{"validates", "", "", "ok: 1 hosts, 1 tokens, 1 peers, 1 databases\n"},
		{"resolves", "video.example.com", "203.0.113.7", "bne.edge.example.com\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := run(file, tt.host, tt.ip, &out); err != nil {
				t.Fatalf("run: %v", err)
			}
			if out.String() != tt.want {
				t.Errorf("output = %q, want %q", out.String(), tt.want)
			}
		})
	}
}

func TestRunReportsProblems(t *testing.T) {
	tests := []struct {
		name, config, host, ip, want string
	}{
		{"invalid config", strings.Replace(config, ".example.com]", ".example.net]", 1), "", "", "not under an allowed suffix"},
		{"unparseable config", "hosts: [\n", "", "", "yaml"},
		{"missing database", strings.Replace(config, "data/netmap.mmdb", "data/missing.mmdb", 1), "", "", "missing.mmdb"},
		{"bad IP", config, "video.example.com", "not-an-ip", "not-an-ip"},
		{"unknown host", config, "other.example.com", "203.0.113.7", "unknown host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := writeFixture(t, tt.config)
			err := run(file, tt.host, tt.ip, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("got %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestRunNeedsHostAndIPTogether(t *testing.T) {
	file := writeFixture(t, config)
	if err := run(file, "video.example.com", "", &bytes.Buffer{}); err == nil {
		t.Error("run accepted -host without -ip")
	}
}
