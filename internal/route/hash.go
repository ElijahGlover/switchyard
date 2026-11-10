package route

import (
	"math"
	"net/netip"
)

// The hash is fixed, not seeded per process, so that every Lambda container
// sends a client to the same target. TestResolveIsStable pins it: changing
// anything here moves every viewer to a different CDN on deploy.

// score is weighted rendezvous hashing. Each client ranks every target by
// a hash of the pair scaled by weight and takes the highest, so a client
// keeps its target across Lambda containers, and disabling a target moves
// only the clients that were on it.
func score(key uint64, target string, weight int) float64 {
	h := mix(fnv1a(key, target))
	u := (float64(h>>11) + 0.5) / (1 << 53) // uniform in (0, 1)
	return float64(weight) / -math.Log(u)
}

func clientKey(host string, ip netip.Addr) uint64 {
	a := ip.As16()
	return fnv1a(fnv1a(fnvOffset, a[:]), host)
}

const (
	fnvOffset = 14695981039346656037
	fnvPrime  = 1099511628211
)

func fnv1a[T string | []byte](h uint64, b T) uint64 {
	for i := 0; i < len(b); i++ {
		h ^= uint64(b[i])
		h *= fnvPrime
	}
	return h
}

// mix is the splitmix64 finaliser. FNV alone clusters on inputs that differ
// only in their last bytes, which would skew the weights.
func mix(h uint64) uint64 {
	h ^= h >> 30
	h *= 0xbf58476d1ce4e5b9
	h ^= h >> 27
	h *= 0x94d049bb133111eb
	h ^= h >> 31
	return h
}
