package mmdbcsv_test

import (
	"bytes"
	"fmt"
	"net/netip"
	"strings"

	"github.com/ElijahGlover/switchyard/internal/mmdbcsv"
	"github.com/oschwald/maxminddb-golang/v2"
)

func ExampleBuild() {
	csv := `prefix,network,location
203.0.113.0/24,onnet,syd
203.0.113.0/25,onnet,bne
`
	var buf bytes.Buffer
	if err := mmdbcsv.Build(strings.NewReader(csv), &buf); err != nil {
		fmt.Println(err)
		return
	}
	db, err := maxminddb.OpenBytes(buf.Bytes())
	if err != nil {
		fmt.Println(err)
		return
	}

	for _, ip := range []string{"203.0.113.7", "203.0.113.200"} {
		var rec struct {
			Location string `maxminddb:"location"`
		}
		if err := db.Lookup(netip.MustParseAddr(ip)).Decode(&rec); err != nil {
			fmt.Println(err)
			continue
		}
		fmt.Println(ip, rec.Location)
	}
	// Output:
	// 203.0.113.7 bne
	// 203.0.113.200 syd
}
