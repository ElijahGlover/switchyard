// Package store keeps a route.Router loaded from S3 and refreshes it in the
// background when the config or a database changes. Requests read the
// current Router without ever waiting on S3.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ElijahGlover/switchyard/internal/route"
)

// ErrNotModified is returned by a Getter when the object still has the ETag
// it was asked about.
var ErrNotModified = errors.New("store: not modified")

// Getter fetches objects by key.
type Getter interface {
	// Get returns the object's body and ETag. If etag is not empty and is
	// still current, it returns ErrNotModified instead.
	Get(ctx context.Context, key, etag string) (body []byte, newETag string, err error)
}

// Store holds the current Router. Databases are read relative to the config
// key, so routing/config.yaml naming netmap.mmdb reads routing/netmap.mmdb.
type Store struct {
	get       Getter
	configKey string
	current   atomic.Pointer[Snapshot]

	mu      sync.Mutex        // one refresh at a time
	objects map[string]object // the objects router was built from
}

// Snapshot is the Router a refresh built, with the objects it was built
// from. It never changes once made, so the two always agree.
type Snapshot struct {
	Router *route.Router
	// Versions holds the databases by name, then the config.
	Versions []Version
}

// Version is one object as this container holds it.
type Version struct {
	Name   string    // the database's name in the config, or "config"
	SHA256 string    // in hex
	Loaded time.Time // when this container got this version of it
}

// New returns a Store with no routes until the first Refresh.
func New(g Getter, configKey string) *Store {
	return &Store{get: g, configKey: configKey, objects: map[string]object{}}
}

// Current returns the current Snapshot, or nil before the first successful
// load. It never waits, even while a refresh is downloading.
func (s *Store) Current() *Snapshot {
	return s.current.Load()
}

// Refresh revalidates the config and databases with S3 and swaps in a new
// Router if anything changed. On failure the current Router keeps serving.
func (s *Store) Refresh(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refresh(ctx)
}

// Run refreshes every interval until ctx ends. Each attempt is given one
// interval to finish, so a fetch that hangs (as one caught by a Lambda
// freeze can) is abandoned and the next tick tries again. Lambda freezes
// the process between requests, so ticks only fire while the container is
// serving traffic.
func (s *Store) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			attempt, cancel := context.WithTimeout(ctx, every)
			if err := s.Refresh(attempt); err != nil {
				slog.ErrorContext(ctx, "refresh failed, keeping current routes", "err", err)
			}
			cancel()
		}
	}
}

type object struct {
	etag   string
	body   []byte
	sha256 string
	loaded time.Time
}

func (o object) version(name string) Version {
	return Version{Name: name, SHA256: o.sha256, Loaded: o.loaded}
}

// refresh revalidates every object by ETag and rebuilds the Router if any
// changed. Objects are committed only together with a Router built from
// them, so a refresh that fails part way is retried in full next time.
func (s *Store) refresh(ctx context.Context) error {
	next := map[string]object{}
	changed := false
	fetch := func(key string) ([]byte, error) {
		old, ok := s.objects[key]
		body, etag, err := s.get.Get(ctx, key, old.etag)
		if ok && errors.Is(err, ErrNotModified) {
			next[key] = old
			return old.body, nil
		}
		if err != nil {
			return nil, fmt.Errorf("get %s: %w", key, err)
		}
		sum := sha256.Sum256(body)
		next[key] = object{etag: etag, body: body, sha256: hex.EncodeToString(sum[:]), loaded: time.Now()}
		changed = true
		return body, nil
	}

	b, err := fetch(s.configKey)
	if err != nil {
		return err
	}
	cfg, err := route.ParseConfig(b)
	if err != nil {
		return fmt.Errorf("%s: %w", s.configKey, err)
	}
	dbs := make(map[string][]byte, len(cfg.Databases))
	var versions []Version
	for _, name := range slices.Sorted(maps.Keys(cfg.Databases)) {
		key := path.Join(path.Dir(s.configKey), cfg.Databases[name])
		b, err := fetch(key)
		if err != nil {
			return err
		}
		dbs[name] = b
		versions = append(versions, next[key].version(name))
	}
	versions = append(versions, next[s.configKey].version("config"))
	if !changed && s.current.Load() != nil {
		return nil
	}
	r, err := route.New(cfg, dbs)
	if err != nil {
		return fmt.Errorf("%s: %w", s.configKey, err)
	}
	s.current.Store(&Snapshot{Router: r, Versions: versions})
	s.objects = next
	return nil
}
