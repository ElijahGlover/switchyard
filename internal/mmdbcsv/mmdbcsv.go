// Package mmdbcsv builds MMDB files from CSV.
//
// The first column is a network prefix and every other column becomes a
// string field on that network's record:
//
//	prefix,network,location
//	203.0.113.0/24,onnet,bne
//	2001:db8:100::/40,onnet,mel
//
// Empty cells are left out of the record. When prefixes overlap, the most
// specific one wins regardless of the order of the rows.
package mmdbcsv

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

type row struct {
	prefix netip.Prefix
	record mmdbtype.Map
}

// Build reads CSV from r and writes an MMDB database to w.
func Build(r io.Reader, w io.Writer) error {
	cr := csv.NewReader(r)
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return errors.New("mmdbcsv: empty input")
	}
	if err != nil {
		return fmt.Errorf("mmdbcsv: %w", err)
	}
	if err := checkHeader(header); err != nil {
		return fmt.Errorf("mmdbcsv: %w", err)
	}

	var rows []row
	seen := map[netip.Prefix]int{}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("mmdbcsv: %w", err)
		}
		line, _ := cr.FieldPos(0)
		p, err := netip.ParsePrefix(strings.TrimSpace(rec[0]))
		if err != nil {
			return fmt.Errorf("mmdbcsv: line %d: %w", line, err)
		}
		if p != p.Masked() {
			return fmt.Errorf("mmdbcsv: line %d: %s has host bits set, use %s", line, p, p.Masked())
		}
		if first, ok := seen[p]; ok {
			return fmt.Errorf("mmdbcsv: line %d: %s already listed on line %d", line, p, first)
		}
		seen[p] = line

		record := mmdbtype.Map{}
		for i, v := range rec[1:] {
			if v = strings.TrimSpace(v); v != "" {
				record[mmdbtype.String(strings.TrimSpace(header[i+1]))] = mmdbtype.String(v)
			}
		}
		rows = append(rows, row{p, record})
	}
	if len(rows) == 0 {
		return errors.New("mmdbcsv: no networks")
	}

	// The writer replaces everything inside a network on insert, so broader
	// networks go in first and more specific ones overwrite their part.
	slices.SortStableFunc(rows, func(a, b row) int { return depth(a.prefix) - depth(b.prefix) })

	tree, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType: "switchyard-csv",
		// Documentation and private ranges are valid in custom network maps.
		IncludeReservedNetworks: true,
	})
	if err != nil {
		return fmt.Errorf("mmdbcsv: %w", err)
	}
	for _, r := range rows {
		if err := tree.Insert(ipNet(r.prefix), r.record); err != nil {
			return fmt.Errorf("mmdbcsv: insert %s: %w", r.prefix, err)
		}
	}
	if _, err := tree.WriteTo(w); err != nil {
		return fmt.Errorf("mmdbcsv: %w", err)
	}
	return nil
}

func checkHeader(header []string) error {
	if strings.TrimSpace(header[0]) != "prefix" {
		return errors.New(`first column must be "prefix"`)
	}
	if len(header) < 2 {
		return errors.New("header must name at least one field after prefix")
	}
	names := map[string]bool{}
	for i, h := range header[1:] {
		h = strings.TrimSpace(h)
		if h == "" {
			return fmt.Errorf("column %d has no name", i+2)
		}
		if names[h] {
			return fmt.Errorf("duplicate column %q", h)
		}
		names[h] = true
	}
	return nil
}

// depth is the prefix length in the IPv6 tree, where IPv4 lives under ::/96.
func depth(p netip.Prefix) int {
	if p.Addr().Is4() {
		return 96 + p.Bits()
	}
	return p.Bits()
}

func ipNet(p netip.Prefix) *net.IPNet {
	return &net.IPNet{
		IP:   p.Addr().AsSlice(),
		Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen()),
	}
}
