package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ElijahGlover/switchyard/internal/mmdbcsv"
	"github.com/ElijahGlover/switchyard/internal/store"
)

const configKey = "routing/config.yaml"

const config = `
databases: { netmap: netmap.mmdb }
tokens: { location: { db: netmap, path: [location], default: syd } }
allowed_suffixes: [.example.com]
hosts:
  video.example.com:
    - targets: { "{location}.edge.example.com": 1 }
`

// popConfig is config with a different target, to see a change land.
var popConfig = strings.Replace(config, ".edge.", ".pop.", 1)

// fakeS3 is an in-memory Getter. It meets the same contract as store.S3
// (getter_contract_test.go), and counts requests so tests can check what
// the store costs, not how it calls.
type fakeS3 struct {
	mu        sync.Mutex
	objects   map[string]fakeObject
	failures  map[string]error
	version   int
	requests  int // every Get
	downloads int // Gets that returned a body
	hang      chan struct{}
}

type fakeObject struct{ body, etag string }

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string]fakeObject{}, failures: map[string]error{}}
}

func (f *fakeS3) put(key, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version++
	f.objects[key] = fakeObject{body: body, etag: fmt.Sprintf(`"v%d"`, f.version)}
}

// fail makes Gets of key return err until it is called again with nil.
func (f *fakeS3) fail(key string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[key] = err
}

// hangNext makes the next Get hang until its context ends, like a request
// caught by a Lambda freeze. The channel receives once it is hanging.
func (f *fakeS3) hangNext() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hang = make(chan struct{}, 1)
	return f.hang
}

func (f *fakeS3) Get(ctx context.Context, key, etag string) ([]byte, string, error) {
	f.mu.Lock()
	hang := f.hang
	f.hang = nil
	f.mu.Unlock()
	if hang != nil {
		hang <- struct{}{}
		<-ctx.Done()
		return nil, "", ctx.Err()
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if err := f.failures[key]; err != nil {
		return nil, "", err
	}
	o, ok := f.objects[key]
	if !ok {
		return nil, "", fmt.Errorf("%s: no such key", key)
	}
	if etag == o.etag {
		return nil, "", store.ErrNotModified
	}
	f.downloads++
	return []byte(o.body), o.etag, nil
}

func (f *fakeS3) counts() (requests, downloads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, f.downloads
}

// body is what the bucket holds at key.
func (f *fakeS3) body(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objects[key].body
}

func mmdb(t testing.TB, csv string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := mmdbcsv.Build(strings.NewReader(csv), &buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// bucket is a fake bucket holding config and its database under routing/,
// and nowhere else.
func bucket(t *testing.T) *fakeS3 {
	t.Helper()
	s3 := newFakeS3()
	s3.put(configKey, config)
	s3.put("routing/netmap.mmdb", mmdb(t, "prefix,location\n203.0.113.0/24,bne\n"))
	return s3
}

// loaded is a store that has made its first load from bucket.
func loaded(t *testing.T) (*store.Store, *fakeS3) {
	t.Helper()
	s3 := bucket(t)
	s := store.New(s3, configKey)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("first load: %v", err)
	}
	return s, s3
}

// target is where the store currently sends a client at 203.0.113.7.
func target(t *testing.T, s *store.Store) string {
	t.Helper()
	cur := s.Current()
	if cur == nil {
		t.Fatal("no routes loaded")
	}
	got, err := cur.Router.Resolve("video.example.com", netip.MustParseAddr("203.0.113.7"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return got
}

// errorLog collects what the default logger writes at ERROR and above
// until the test ends.
func errorLog(t testing.TB) *syncBuffer {
	t.Helper()
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// syncBuffer is a bytes.Buffer that Run's goroutine can write to while the
// test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// sha256Hex is what sha256sum prints for body.
func sha256Hex(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func TestCurrentGivesEachObjectsHash(t *testing.T) {
	s, s3 := loaded(t)

	var got []string
	for _, v := range s.Current().Versions {
		got = append(got, v.Name+" "+v.SHA256)
	}

	want := []string{"netmap " + sha256Hex(s3.body("routing/netmap.mmdb")), "config " + sha256Hex(config)}
	if !slices.Equal(got, want) {
		t.Errorf("versions = %q, want %q", got, want)
	}
}

// A version's load time is when this container got it, so an object that
// did not change keeps its time across a refresh.
func TestCurrentGivesWhenEachObjectWasLoaded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, s3 := loaded(t)
		start := time.Now()
		time.Sleep(time.Minute)
		s3.put(configKey, popConfig)
		if err := s.Refresh(context.Background()); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		got := map[string]time.Time{}
		for _, v := range s.Current().Versions {
			got[v.Name] = v.Loaded
		}

		want := map[string]time.Time{"netmap": start, "config": start.Add(time.Minute)}
		if !maps.EqualFunc(got, want, time.Time.Equal) {
			t.Errorf("loaded = %v, want %v", got, want)
		}
	})
}

func TestLoadsDatabasesRelativeToTheConfig(t *testing.T) {
	// The bucket only has the database at routing/netmap.mmdb, so routing
	// to bne proves it was found next to routing/config.yaml.
	s, _ := loaded(t)
	if got := target(t, s); got != "bne.edge.example.com" {
		t.Errorf("got %s, want bne.edge.example.com", got)
	}
}

func TestHasNoRoutesBeforeTheFirstLoad(t *testing.T) {
	if r := store.New(bucket(t), configKey).Current(); r != nil {
		t.Error("got routes before any load, want nil")
	}
}

func TestReportsAFailedFirstLoad(t *testing.T) {
	s3 := bucket(t)
	s3.fail(configKey, errors.New("access denied"))
	err := store.New(s3, configKey).Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Errorf("got %v, want the access denied error", err)
	}
}

func TestDoesNotDownloadUnchangedObjectsAgain(t *testing.T) {
	s, s3 := loaded(t)
	_, before := s3.counts()

	s.Refresh(context.Background())

	if _, after := s3.counts(); after != before {
		t.Errorf("downloaded %d unchanged objects again, want 0", after-before)
	}
}

func TestAppliesAChangedConfig(t *testing.T) {
	s, s3 := loaded(t)
	s3.put(configKey, popConfig)

	s.Refresh(context.Background())

	if got := target(t, s); got != "bne.pop.example.com" {
		t.Errorf("got %s, want bne.pop.example.com", got)
	}
}

func TestAppliesAChangedDatabase(t *testing.T) {
	s, s3 := loaded(t)
	s3.put("routing/netmap.mmdb", mmdb(t, "prefix,location\n203.0.113.0/24,mel\n"))

	s.Refresh(context.Background())

	if got := target(t, s); got != "mel.edge.example.com" {
		t.Errorf("got %s, want mel.edge.example.com", got)
	}
}

func TestKeepsServingThroughABadConfig(t *testing.T) {
	tests := []struct{ name, body string }{
		{"invalid", "hosts: {}\n"},
		{"unparseable", "hosts: [\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, s3 := loaded(t)
			s3.put(configKey, tt.body)

			s.Refresh(context.Background())

			if got := target(t, s); got != "bne.edge.example.com" {
				t.Errorf("got %s, want the previous bne.edge.example.com", got)
			}
		})
	}
}

func TestReportsABadConfig(t *testing.T) {
	s, s3 := loaded(t)
	s3.put(configKey, "hosts: {}\n")

	err := s.Refresh(context.Background())

	if err == nil || !strings.Contains(err.Error(), configKey) {
		t.Errorf("got %v, want an error naming %s", err, configKey)
	}
}

func TestAppliesAFixAfterABadConfig(t *testing.T) {
	s, s3 := loaded(t)
	s3.put(configKey, "hosts: {}\n")
	s.Refresh(context.Background())

	s3.put(configKey, popConfig)
	s.Refresh(context.Background())

	if got := target(t, s); got != "bne.pop.example.com" {
		t.Errorf("got %s, want bne.pop.example.com", got)
	}
}

func TestKeepsServingWhenS3Fails(t *testing.T) {
	s, s3 := loaded(t)
	s3.fail(configKey, errors.New("timeout"))

	s.Refresh(context.Background())

	if got := target(t, s); got != "bne.edge.example.com" {
		t.Errorf("got %s, want the previous bne.edge.example.com", got)
	}
}

func TestRetriesARefreshThatFailedPartWay(t *testing.T) {
	s, s3 := loaded(t)

	// The new config is fetched, then the database fetch fails. Once S3
	// recovers the new config must still land, even though the store
	// already saw its ETag.
	s3.put(configKey, popConfig)
	s3.fail("routing/netmap.mmdb", errors.New("timeout"))
	s.Refresh(context.Background())

	s3.fail("routing/netmap.mmdb", nil)
	s.Refresh(context.Background())

	if got := target(t, s); got != "bne.pop.example.com" {
		t.Errorf("got %s, want bne.pop.example.com", got)
	}
}

func TestRouterDoesNotWaitForARefreshInFlight(t *testing.T) {
	s, s3 := loaded(t)
	hanging := s3.hangNext()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Refresh(ctx)
	<-hanging

	answered := make(chan bool, 1)
	go func() { answered <- s.Current() != nil }()

	select {
	case ok := <-answered:
		if !ok {
			t.Error("got no routes, want the current ones")
		}
	case <-time.After(time.Second):
		t.Fatal("Router waited for the refresh in flight")
	}
}

func TestRunAppliesChangesEachInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, s3 := loaded(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go s.Run(ctx, 10*time.Second)
		s3.put(configKey, popConfig)

		time.Sleep(10 * time.Second)
		synctest.Wait()

		if got := target(t, s); got != "bne.pop.example.com" {
			t.Errorf("got %s, want bne.pop.example.com", got)
		}
	})
}

func TestRunMakesNoS3RequestsBetweenIntervals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, s3 := loaded(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go s.Run(ctx, 10*time.Second)
		before, _ := s3.counts()

		time.Sleep(9 * time.Second)
		synctest.Wait()

		if after, _ := s3.counts(); after != before {
			t.Errorf("made %d S3 requests before the interval, want 0", after-before)
		}
	})
}

func TestRunRecoversFromAFailedFirstLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s3 := bucket(t)
		s3.fail(configKey, errors.New("access denied"))
		s := store.New(s3, configKey)
		s.Refresh(context.Background())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go s.Run(ctx, 10*time.Second)

		s3.fail(configKey, nil)
		time.Sleep(10 * time.Second)
		synctest.Wait()

		if s.Current() == nil {
			t.Error("still no routes after S3 recovered")
		}
	})
}

func TestRunAbandonsAHungFetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, s3 := loaded(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s3.put(configKey, popConfig)
		s3.hangNext()
		go s.Run(ctx, 10*time.Second)

		// The first refresh hangs. It must time out within an interval so
		// the next one can apply the change.
		time.Sleep(30 * time.Second)
		synctest.Wait()

		if got := target(t, s); got != "bne.pop.example.com" {
			t.Errorf("got %s, want bne.pop.example.com", got)
		}
	})
}

func TestRunLogsAFailedRefreshAsAnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		errs := errorLog(t)
		s, s3 := loaded(t)
		s3.fail(configKey, errors.New("access denied"))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go s.Run(ctx, 10*time.Second)

		time.Sleep(10 * time.Second)
		synctest.Wait()

		if got := errs.String(); !strings.Contains(got, "access denied") {
			t.Errorf("got error log %q, want the failed refresh in it", got)
		}
	})
}

func TestRunStopsWhenItsContextEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := loaded(t)
		ctx, cancel := context.WithCancel(context.Background())
		stopped := make(chan struct{})
		go func() {
			s.Run(ctx, 10*time.Second)
			close(stopped)
		}()

		cancel()
		synctest.Wait()

		select {
		case <-stopped:
		default:
			t.Error("Run kept going after its context ended")
		}
	})
}

// TestIsSafeForConcurrentUse only proves something under the race
// detector, which make test and CI turn on.
func TestIsSafeForConcurrentUse(t *testing.T) {
	s, _ := loaded(t)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { s.Refresh(context.Background()) })
		wg.Go(func() { s.Current() })
	}
	wg.Wait()
}
