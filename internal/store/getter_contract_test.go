package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ElijahGlover/switchyard/internal/store"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// getterContract is the behaviour store.Store relies on from a Getter. The fake
// in store_test.go and the S3 adapter both run it, so the store's tests stay
// honest about what S3 does.
type getterContract struct {
	// newGetter returns a Getter holding one object, and the key of an
	// object that does not exist.
	newGetter func(t *testing.T, key, body string) (g store.Getter, missing string)
}

func (c getterContract) test(t *testing.T) {
	ctx := context.Background()
	const key, body = "routing/config.yaml", "hosts: {}\n"

	t.Run("returns the body and an ETag", func(t *testing.T) {
		g, _ := c.newGetter(t, key, body)
		got, etag, err := g.Get(ctx, key, "")
		if err != nil || string(got) != body || etag == "" {
			t.Errorf("got %q, %q, %v; want the body, an ETag and no error", got, etag, err)
		}
	})
	t.Run("current ETag gives ErrNotModified", func(t *testing.T) {
		g, _ := c.newGetter(t, key, body)
		_, etag, _ := g.Get(ctx, key, "")
		if _, _, err := g.Get(ctx, key, etag); !errors.Is(err, store.ErrNotModified) {
			t.Errorf("got %v, want ErrNotModified", err)
		}
	})
	t.Run("stale ETag gives the body", func(t *testing.T) {
		g, _ := c.newGetter(t, key, body)
		got, _, err := g.Get(ctx, key, `"stale"`)
		if err != nil || string(got) != body {
			t.Errorf("got %q, %v; want the body", got, err)
		}
	})
	t.Run("missing key is an error, not ErrNotModified", func(t *testing.T) {
		g, missing := c.newGetter(t, key, body)
		if _, _, err := g.Get(ctx, missing, ""); err == nil || errors.Is(err, store.ErrNotModified) {
			t.Errorf("got %v, want a not-found error", err)
		}
	})
}

func TestFakeS3MeetsGetterContract(t *testing.T) {
	getterContract{newGetter: func(t *testing.T, key, body string) (store.Getter, string) {
		f := newFakeS3()
		f.put(key, body)
		return f, "routing/missing.yaml"
	}}.test(t)
}

// TestS3MeetsGetterContract runs the real SDK against an HTTP server that
// answers like S3, including the 304 the SDK reports as an error.
func TestS3MeetsGetterContract(t *testing.T) {
	getterContract{newGetter: func(t *testing.T, key, body string) (store.Getter, string) {
		etag := fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(body)))
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/bucket/"+key {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
				return
			}
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
			w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		client := s3.New(s3.Options{
			Region:       "ap-southeast-2",
			BaseEndpoint: aws.String(srv.URL),
			UsePathStyle: true,
			Credentials:  aws.AnonymousCredentials{},
		})
		return store.S3{Client: client, Bucket: "bucket"}, "routing/missing.yaml"
	}}.test(t)
}

// TestS3MeetsGetterContractOnRealS3 runs the contract against a real
// bucket. It writes under switchyard-contract-test/ and is skipped unless
// SWITCHYARD_TEST_BUCKET is set:
//
//	SWITCHYARD_TEST_BUCKET=my-bucket go test -run OnRealS3 ./internal/store/
func TestS3MeetsGetterContractOnRealS3(t *testing.T) {
	bucket := os.Getenv("SWITCHYARD_TEST_BUCKET")
	if bucket == "" {
		t.Skip("SWITCHYARD_TEST_BUCKET not set")
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	client := s3.NewFromConfig(cfg)
	getterContract{newGetter: func(t *testing.T, key, body string) (store.Getter, string) {
		key = "switchyard-contract-test/" + key
		_, err := client.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader(body),
		})
		if err != nil {
			t.Fatal(err)
		}
		return prefixed{store.S3{Client: client, Bucket: bucket}, "switchyard-contract-test/"}, "routing/missing.yaml"
	}}.test(t)
}

// prefixed lets the real-S3 run use the contract's keys inside a test folder.
type prefixed struct {
	g      store.Getter
	prefix string
}

func (p prefixed) Get(ctx context.Context, key, etag string) ([]byte, string, error) {
	return p.g.Get(ctx, p.prefix+key, etag)
}
