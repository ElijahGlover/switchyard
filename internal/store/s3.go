package store

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3 is a Getter for one bucket.
type S3 struct {
	Client *s3.Client
	Bucket string
}

// Get fetches key with a conditional GET when etag is set.
func (g S3) Get(ctx context.Context, key, etag string) ([]byte, string, error) {
	in := &s3.GetObjectInput{Bucket: aws.String(g.Bucket), Key: aws.String(key)}
	if etag != "" {
		in.IfNoneMatch = aws.String(etag)
	}
	out, err := g.Client.GetObject(ctx, in)
	if err != nil {
		// The SDK reports 304 as an error rather than an empty response.
		var re interface{ HTTPStatusCode() int }
		if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotModified {
			return nil, "", ErrNotModified
		}
		return nil, "", err
	}
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, "", err
	}
	return body, aws.ToString(out.ETag), nil
}
