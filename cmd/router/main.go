// Command router is the Lambda function. It reads its config and databases
// from S3 and answers each request with a redirect to the chosen target.
//
// Environment:
//
//	CONFIG_BUCKET     bucket holding the config and databases
//	CONFIG_KEY        key of config.yaml, e.g. routing/config.yaml
//	REFRESH_INTERVAL  how often to revalidate with S3 (default 10s)
//	CLIENT_IP         where a Function URL request's client address comes
//	                  from: source-ip (default, a public Function URL) or
//	                  cloudfront (CloudFront-Viewer-Address, when the URL
//	                  accepts requests from CloudFront alone). ALB events
//	                  always use the last X-Forwarded-For entry.
//	DEBUG_KEY         when set, a request that sends it in X-Switchyard-Debug
//	                  gets an X-Switchyard-Debug header showing how its route
//	                  was chosen. Unset, nobody can.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/ElijahGlover/switchyard/internal/store"
	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	bucket, key := os.Getenv("CONFIG_BUCKET"), os.Getenv("CONFIG_KEY")
	if bucket == "" || key == "" {
		fatal("CONFIG_BUCKET and CONFIG_KEY must be set")
	}
	interval := 10 * time.Second
	if v := os.Getenv("REFRESH_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			fatal("REFRESH_INTERVAL must be a positive duration such as 10s", "value", v)
		}
		interval = d
	}

	ip, err := parseIPSource(os.Getenv("CLIENT_IP"))
	if err != nil {
		fatal(err.Error())
	}

	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		fatal("load AWS config", "err", err)
	}
	routes := store.New(store.S3{Client: s3.NewFromConfig(cfg), Bucket: bucket}, key)

	// Load during Lambda init, before the first request. If it fails,
	// requests get a 503 until the background refresh succeeds.
	load, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := routes.Refresh(load); err != nil {
		slog.Error("initial load failed", "err", err)
	}
	cancel()

	// Requests never wait on S3: the refresh runs beside them.
	go routes.Run(ctx, interval)
	lambda.Start(handler{routes: routes, ip: ip, debugKey: os.Getenv("DEBUG_KEY")}.invoke)
}

func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}
