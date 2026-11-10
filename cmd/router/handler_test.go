package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ElijahGlover/switchyard/internal/mmdbcsv"
	"github.com/ElijahGlover/switchyard/internal/route"
	"github.com/ElijahGlover/switchyard/internal/store"
	"github.com/aws/aws-lambda-go/events"
	"go.yaml.in/yaml/v3"
)

const config = `
databases: { netmap: netmap.mmdb }
tokens: { location: { db: netmap, path: [location], default: syd } }
allowed_suffixes: [.example.com, .example.net]
disabled: [cdn-a.example.net]
hosts:
  video.example.com:
    - targets: { "{location}.edge.example.com": 1 }
  down.example.com:
    - targets: { cdn-a.example.net: 1 }
  split.example.com:
    - targets: { direct.example.com: 70, cdn-b.example.net: 30 }
  unrouted.example.com:
    - when: { location: per }
      targets: { direct.example.com: 1 }
  abc123.lambda-url.ap-southeast-2.on.aws:
    - targets: { direct.example.com: 1 }
`

// fakeRoutes stands in for the store; a nil router means nothing loaded
// yet. Refresh swaps in next, as a refresh that found a change would.
type fakeRoutes struct {
	r, next  *route.Router
	versions []store.Version
	err      error
}

func (f *fakeRoutes) Current() *store.Snapshot {
	if f.r == nil {
		return nil
	}
	return &store.Snapshot{Router: f.r, Versions: f.versions}
}

func (f *fakeRoutes) Refresh(context.Context) error {
	if f.err != nil {
		return f.err
	}
	if f.next != nil {
		f.r = f.next
	}
	return nil
}

// testRouter sends 203.0.113.0/24 to bne and everyone else to syd.
func testRouter(t testing.TB) *route.Router {
	t.Helper()
	return routerFor(t, "prefix,location\n203.0.113.0/24,bne\n")
}

func routerFor(t testing.TB, netmap string) *route.Router {
	t.Helper()
	var db bytes.Buffer
	if err := mmdbcsv.Build(strings.NewReader(netmap), &db); err != nil {
		t.Fatal(err)
	}
	cfg, err := route.ParseConfig([]byte(config))
	if err != nil {
		t.Fatal(err)
	}
	r, err := route.New(cfg, map[string][]byte{"netmap": db.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// urlRequest is a Function URL event. The host header is always the Function
// URL's own domain; a proxy in front sets X-Forwarded-Host.
func urlRequest(method, forwardedHost, ip, path, query string) events.LambdaFunctionURLRequest {
	headers := map[string]string{"host": "abc123.lambda-url.ap-southeast-2.on.aws"}
	if forwardedHost != "" {
		headers["x-forwarded-host"] = forwardedHost
	}
	return events.LambdaFunctionURLRequest{
		RawPath:        path,
		RawQueryString: query,
		Headers:        headers,
		RequestContext: events.LambdaFunctionURLRequestContext{
			DomainName: "abc123.lambda-url.ap-southeast-2.on.aws",
			HTTP:       events.LambdaFunctionURLRequestContextHTTPDescription{Method: method, SourceIP: ip},
		},
	}
}

func TestHandle(t *testing.T) {
	h := handler{routes: &fakeRoutes{r: testRouter(t)}}
	tests := []struct {
		name       string
		req        events.LambdaFunctionURLRequest
		wantStatus int
		wantLoc    string
	}{
		{"redirects with path and query", urlRequest("GET", "video.example.com", "203.0.113.7", "/live/master.m3u8", "bitrate=high"),
			http.StatusFound, "https://bne.edge.example.com/live/master.m3u8?bitrate=high"},
		{"empty path", urlRequest("GET", "video.example.com", "203.0.113.7", "", ""),
			http.StatusFound, "https://bne.edge.example.com/"},
		{"port and list in forwarded host", urlRequest("GET", "video.example.com:443, proxy.example.com", "203.0.113.7", "/a", ""),
			http.StatusFound, "https://bne.edge.example.com/a"},
		{"HEAD", urlRequest("HEAD", "video.example.com", "198.51.100.1", "/a", ""),
			http.StatusFound, "https://syd.edge.example.com/a"},
		{"POST", urlRequest("POST", "video.example.com", "203.0.113.7", "/a", ""), http.StatusMethodNotAllowed, ""},
		{"no forwarded host uses the Function URL domain", urlRequest("GET", "", "203.0.113.7", "/a", ""),
			http.StatusFound, "https://direct.example.com/a"},
		{"unknown host", urlRequest("GET", "other.example.com", "203.0.113.7", "/a", ""), http.StatusNotFound, ""},
		{"disabled target is blackholed", urlRequest("GET", "down.example.com", "203.0.113.7", "/a", ""), http.StatusUnauthorized, ""},
		{"no rule matches", urlRequest("GET", "unrouted.example.com", "203.0.113.7", "/a", ""), http.StatusServiceUnavailable, ""},
		{"bad source IP", urlRequest("GET", "video.example.com", "not-an-ip", "/a", ""), http.StatusBadRequest, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.handle(context.Background(), tt.req)
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %q)", resp.StatusCode, tt.wantStatus, resp.Body)
			}
			if got := resp.Headers["Location"]; got != tt.wantLoc {
				t.Errorf("Location = %q, want %q", got, tt.wantLoc)
			}
		})
	}
}

// TestHandleResponsesAreNeverCached covers redirects and errors alike: a
// decision belongs to one client, so no shared cache may keep it.
func TestHandleResponsesAreNeverCached(t *testing.T) {
	h := handler{routes: &fakeRoutes{r: testRouter(t)}}
	tests := []struct {
		name string
		req  events.LambdaFunctionURLRequest
	}{
		{"redirect", urlRequest("GET", "video.example.com", "203.0.113.7", "/a", "")},
		{"unknown host", urlRequest("GET", "other.example.com", "203.0.113.7", "/a", "")},
		{"blackholed", urlRequest("GET", "down.example.com", "203.0.113.7", "/a", "")},
		{"no rule matches", urlRequest("GET", "unrouted.example.com", "203.0.113.7", "/a", "")},
		{"bad method", urlRequest("POST", "video.example.com", "203.0.113.7", "/a", "")},
		{"bad source IP", urlRequest("GET", "video.example.com", "not-an-ip", "/a", "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.handle(context.Background(), tt.req)
			if got := resp.Headers["Cache-Control"]; got != "private, no-store" {
				t.Errorf("Cache-Control = %q, want private, no-store", got)
			}
		})
	}
}

// A Function URL labels a response without a Content-Type as JSON, which a
// redirect's empty body is not.
func TestHandleRedirectIsNotLabelledJSON(t *testing.T) {
	h := handler{routes: &fakeRoutes{r: testRouter(t)}}
	resp := h.handle(context.Background(), urlRequest("GET", "video.example.com", "203.0.113.7", "/a", ""))
	if got := resp.Headers["Content-Type"]; got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8, so Lambda does not add application/json", got)
	}
}

func TestHandleBlackholeSaysForbidden(t *testing.T) {
	h := handler{routes: &fakeRoutes{r: testRouter(t)}}
	resp := h.handle(context.Background(), urlRequest("GET", "down.example.com", "203.0.113.7", "/a", ""))
	if resp.Body != "forbidden\n" {
		t.Errorf("body = %q, want forbidden, and nothing about which target was disabled", resp.Body)
	}
}

func TestHandleBeforeRoutesHaveLoaded(t *testing.T) {
	h := handler{routes: &fakeRoutes{}}
	resp := h.handle(context.Background(), urlRequest("GET", "video.example.com", "203.0.113.7", "/a", ""))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// Client addresses on a Function URL. testRouter sends 203.0.113.0/24 to
// bne, so a request that lands on bne used that address.

func TestPublicFunctionURLTrustsOnlyTheSourceIP(t *testing.T) {
	h := handler{routes: &fakeRoutes{r: testRouter(t)}}
	req := urlRequest("GET", "video.example.com", "198.51.100.1", "/a", "")
	req.Headers["x-forwarded-for"] = "203.0.113.7"
	req.Headers["cloudfront-viewer-address"] = "203.0.113.7:443"

	resp := h.handle(context.Background(), req)

	if got := resp.Headers["Location"]; got != "https://syd.edge.example.com/a" {
		t.Errorf("Location = %q, want syd: headers a client sends must not choose its route", got)
	}
}

func TestBehindCloudFrontUsesTheViewerAddress(t *testing.T) {
	h := handler{routes: &fakeRoutes{r: testRouter(t)}, ip: fromCloudFront}
	req := urlRequest("GET", "video.example.com", "198.51.100.200", "/a", "") // CloudFront's edge
	req.Headers["cloudfront-viewer-address"] = "203.0.113.7:46532"

	resp := h.handle(context.Background(), req)

	if got := resp.Headers["Location"]; got != "https://bne.edge.example.com/a" {
		t.Errorf("Location = %q, want bne, from the viewer address", got)
	}
}

func TestBehindCloudFrontRefusesARequestWithoutAViewerAddress(t *testing.T) {
	h := handler{routes: &fakeRoutes{r: testRouter(t)}, ip: fromCloudFront}
	resp := h.handle(context.Background(), urlRequest("GET", "video.example.com", "198.51.100.200", "/a", ""))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestParseIPSource(t *testing.T) {
	tests := []struct {
		value string
		want  ipSource
	}{
		{"", fromSourceIP},
		{"source-ip", fromSourceIP},
		{"cloudfront", fromCloudFront},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			if got, err := parseIPSource(tt.value); err != nil || got != tt.want {
				t.Errorf("got %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func TestParseIPSourceRejectsUnknownValues(t *testing.T) {
	if _, err := parseIPSource("x-forwarded-for"); err == nil {
		t.Error("accepted x-forwarded-for, want an error listing the options")
	}
}

// X-Version says which objects answered: each one's name, the start of its
// SHA-256, and how many whole seconds this container has had it.
func TestVersionHeaderSaysWhatAnswered(t *testing.T) {
	tests := []struct{ name, host string }{
		{"a redirect", "video.example.com"},
		{"an unknown host", "other.example.com"},
		{"a disabled target", "down.example.com"},
		{"no rule matched", "unrouted.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				routes := &fakeRoutes{r: testRouter(t)}
				routes.versions = append(routes.versions, store.Version{Name: "db", SHA256: "7fc0132" + strings.Repeat("0", 57), Loaded: time.Now()})
				time.Sleep(5 * time.Second)
				routes.versions = append(routes.versions, store.Version{Name: "config", SHA256: "76e1998" + strings.Repeat("0", 57), Loaded: time.Now()})
				time.Sleep(1750*time.Second + 900*time.Millisecond)

				resp := handler{routes: routes}.handle(context.Background(), urlRequest("GET", tt.host, "203.0.113.7", "/a", ""))

				want := "db 7fc0132 age 1755; config 76e1998 age 1750"
				if got := resp.Headers["X-Version"]; got != want {
					t.Errorf("X-Version = %q, want %q", got, want)
				}
			})
		})
	}
}

// The debug header. A request that presents the function's debug key is
// shown how its route was chosen; nobody else sees anything new.

const debugKey = "0123456789abcdef0123456789abcdef"

func debugRequest(host, key string) events.LambdaFunctionURLRequest {
	req := urlRequest("GET", host, "203.0.113.7", "/a", "")
	req.Headers["x-switchyard-debug"] = key
	return req
}

func TestDebugHeaderExplainsTheRoute(t *testing.T) {
	h := handler{routes: &fakeRoutes{r: testRouter(t)}, debugKey: debugKey}
	tests := []struct {
		name, host, want string
	}{
		{"a redirect", "video.example.com",
			"rule=0; target=bne.edge.example.com; candidates=bne.edge.example.com:1; location=bne"},
		{"a weighted pick", "split.example.com",
			"rule=0; target=direct.example.com; candidates=cdn-b.example.net:30,direct.example.com:70; location=bne"},
		{"a disabled target", "down.example.com",
			"rule=0; target=cdn-a.example.net; candidates=cdn-a.example.net:1; location=bne"},
		{"no rule matched", "unrouted.example.com", "rule=none; location=bne"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.handle(context.Background(), debugRequest(tt.host, debugKey))
			if got := resp.Headers["X-Switchyard-Debug"]; got != tt.want {
				t.Errorf("X-Switchyard-Debug = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDebugHeaderNeedsTheKey(t *testing.T) {
	tests := []struct {
		name, functionKey, sent string
	}{
		{"wrong key", debugKey, "not-the-key"},
		{"no key sent", debugKey, ""},
		{"part of the key", debugKey, debugKey[:16]},
		{"the function has no key", "", ""},
		{"a key sent to a function without one", "", debugKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := handler{routes: &fakeRoutes{r: testRouter(t)}, debugKey: tt.functionKey}
			resp := h.handle(context.Background(), debugRequest("video.example.com", tt.sent))
			if got, ok := resp.Headers["X-Switchyard-Debug"]; ok {
				t.Errorf("got X-Switchyard-Debug %q, want none without the function's key", got)
			}
		})
	}
}

func TestALBDebugHeaderExplainsTheRoute(t *testing.T) {
	h := handler{routes: &fakeRoutes{r: testRouter(t)}, debugKey: debugKey}
	req := albRequest("203.0.113.7", nil)
	req.Headers["x-switchyard-debug"] = debugKey

	resp := h.handleALB(context.Background(), req)

	want := "rule=0; target=bne.edge.example.com; candidates=bne.edge.example.com:1; location=bne"
	if got := resp.Headers["X-Switchyard-Debug"]; got != want {
		t.Errorf("X-Switchyard-Debug = %q, want %q", got, want)
	}
}

// ALB events. The load balancer sends the viewer's Host, and appends the
// address that connected to it to X-Forwarded-For.

func albRequest(xff string, query map[string]string) events.ALBTargetGroupRequest {
	return events.ALBTargetGroupRequest{
		HTTPMethod:            "GET",
		Path:                  "/a",
		QueryStringParameters: query,
		Headers:               map[string]string{"host": "video.example.com", "x-forwarded-for": xff},
	}
}

func TestALBUsesTheLastForwardedForEntry(t *testing.T) {
	h := handler{routes: &fakeRoutes{r: testRouter(t)}}
	tests := []struct {
		name, xff, want string
	}{
		{"the load balancer's entry", "203.0.113.7", "https://bne.edge.example.com/a"},
		{"entries the client sent are ignored", "203.0.113.7, 198.51.100.1", "https://syd.edge.example.com/a"},
		{"however many there are", "198.51.100.1,198.51.100.2, 203.0.113.7", "https://bne.edge.example.com/a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.handleALB(context.Background(), albRequest(tt.xff, nil))
			if got := resp.Headers["Location"]; got != tt.want {
				t.Errorf("Location = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestALBRefusesARequestWithoutForwardedFor(t *testing.T) {
	h := handler{routes: &fakeRoutes{r: testRouter(t)}}
	req := albRequest("", nil)
	delete(req.Headers, "x-forwarded-for")
	if resp := h.handleALB(context.Background(), req); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestALBRebuildsTheQueryInKeyOrder(t *testing.T) {
	// The ALB event has no raw query string, only a map, so the redirect
	// rebuilds it. Values are passed on as the ALB received them.
	h := handler{routes: &fakeRoutes{r: testRouter(t)}}
	resp := h.handleALB(context.Background(), albRequest("203.0.113.7", map[string]string{"b": "2", "a": "x%20y"}))
	if got := resp.Headers["Location"]; got != "https://bne.edge.example.com/a?a=x%20y&b=2" {
		t.Errorf("Location = %q, want the query rebuilt as a=x%%20y&b=2", got)
	}
}

func multiValueALBRequest() events.ALBTargetGroupRequest {
	return events.ALBTargetGroupRequest{
		HTTPMethod:                      "GET",
		Path:                            "/a",
		MultiValueQueryStringParameters: map[string][]string{"q": {"1", "2"}},
		MultiValueHeaders: map[string][]string{
			"host":            {"video.example.com"},
			"x-forwarded-for": {"198.51.100.1", "198.51.100.2, 203.0.113.7"},
		},
	}
}

func TestALBMultiValueRequestsAreRouted(t *testing.T) {
	h := handler{routes: &fakeRoutes{r: testRouter(t)}}
	resp := h.handleALB(context.Background(), multiValueALBRequest())
	if got := resp.MultiValueHeaders["Location"]; len(got) != 1 || got[0] != "https://bne.edge.example.com/a?q=1&q=2" {
		t.Errorf("Location = %q, want [https://bne.edge.example.com/a?q=1&q=2]", got)
	}
}

func TestALBRepliesInTheRequestsHeaderStyle(t *testing.T) {
	// A target group with multi-value headers on must be answered with
	// multiValueHeaders, and one without must not be.
	h := handler{routes: &fakeRoutes{r: testRouter(t)}}
	tests := []struct {
		name       string
		req        events.ALBTargetGroupRequest
		wantMulti  bool
		wantSingle bool
	}{
		{"single-value", albRequest("203.0.113.7", nil), false, true},
		{"multi-value", multiValueALBRequest(), true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.handleALB(context.Background(), tt.req)
			if gotMulti, gotSingle := resp.MultiValueHeaders != nil, resp.Headers != nil; gotMulti != tt.wantMulti || gotSingle != tt.wantSingle {
				t.Errorf("multiValueHeaders set %v, headers set %v; want %v and %v", gotMulti, gotSingle, tt.wantMulti, tt.wantSingle)
			}
		})
	}
}

func TestALBResponsesCarryAStatusDescription(t *testing.T) {
	h := handler{routes: &fakeRoutes{r: testRouter(t)}}
	if resp := h.handleALB(context.Background(), albRequest("203.0.113.7", nil)); resp.StatusDescription != "302 Found" {
		t.Errorf("statusDescription = %q, want 302 Found", resp.StatusDescription)
	}
}

// invoke takes the raw event Lambda delivers and answers in that event's
// own format.

const warmUpPayload = `{"warm": true}`

// functionURLPayload is a Function URL event as Lambda sends it, trimmed.
const functionURLPayload = `{
  "version": "2.0",
  "rawPath": "/live/master.m3u8",
  "rawQueryString": "",
  "headers": {"host": "abc123.lambda-url.ap-southeast-2.on.aws", "x-forwarded-host": "video.example.com"},
  "requestContext": {
    "domainName": "abc123.lambda-url.ap-southeast-2.on.aws",
    "http": {"method": "GET", "path": "/live/master.m3u8", "sourceIp": "203.0.113.7"}
  },
  "isBase64Encoded": false
}`

// albPayload is an ALB event as the load balancer sends it, trimmed.
const albPayload = `{
  "requestContext": {"elb": {"targetGroupArn": "arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:targetgroup/router/6d0ecf831eec9f09"}},
  "httpMethod": "GET",
  "path": "/live/master.m3u8",
  "queryStringParameters": {},
  "headers": {"host": "video.example.com", "x-forwarded-for": "198.51.100.1, 203.0.113.7"},
  "body": "",
  "isBase64Encoded": false
}`

func TestInvokeAnswersFunctionURLEventsAsFunctionURLResponses(t *testing.T) {
	h := handler{routes: &fakeRoutes{r: testRouter(t)}}
	out, _ := h.invoke(context.Background(), json.RawMessage(functionURLPayload))
	resp, ok := out.(events.LambdaFunctionURLResponse)
	if !ok || resp.Headers["Location"] != "https://bne.edge.example.com/live/master.m3u8" {
		t.Errorf("got %#v, want a Function URL redirect to bne", out)
	}
}

func TestInvokeAnswersALBEventsAsALBResponses(t *testing.T) {
	h := handler{routes: &fakeRoutes{r: testRouter(t)}}
	out, _ := h.invoke(context.Background(), json.RawMessage(albPayload))
	resp, ok := out.(events.ALBTargetGroupResponse)
	if !ok || resp.Headers["Location"] != "https://bne.edge.example.com/live/master.m3u8" {
		t.Errorf("got %#v, want an ALB redirect to bne", out)
	}
}

func TestInvokeRejectsAnEventItCannotRead(t *testing.T) {
	h := handler{routes: &fakeRoutes{r: testRouter(t)}}
	if _, err := h.invoke(context.Background(), json.RawMessage(`[1, 2]`)); err == nil {
		t.Error("accepted a JSON array, want an error")
	}
}

func TestWarmUpRefreshesTheRoutes(t *testing.T) {
	routes := &fakeRoutes{r: testRouter(t), next: routerFor(t, "prefix,location\n203.0.113.0/24,mel\n")}
	h := handler{routes: routes}

	if _, err := h.invoke(context.Background(), json.RawMessage(warmUpPayload)); err != nil {
		t.Fatalf("warm-up: %v", err)
	}

	out, _ := h.invoke(context.Background(), json.RawMessage(functionURLPayload))
	if resp, _ := out.(events.LambdaFunctionURLResponse); resp.Headers["Location"] != "https://mel.edge.example.com/live/master.m3u8" {
		t.Errorf("after warm-up got %#v, want the refreshed mel route", out)
	}
}

func TestWarmUpReportsAFailedRefresh(t *testing.T) {
	h := handler{routes: &fakeRoutes{r: testRouter(t), err: errors.New("access denied")}}
	if _, err := h.invoke(context.Background(), json.RawMessage(warmUpPayload)); err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Errorf("got %v, want the refresh error, so failed warm-ups show in Lambda metrics", err)
	}
}

// templates are the stacks in deploy/, one for each front door.
var templates = []string{"function-url.yaml", "alb.yaml", "cloudfront.yaml"}

// readTemplate decodes a template from deploy/ into tmpl. Only the fields
// tmpl names are decoded, so CloudFormation tags such as !Ref elsewhere in
// the file do not matter.
func readTemplate(t *testing.T, name string, tmpl any) {
	t.Helper()
	b, err := os.ReadFile("../../deploy/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, tmpl); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// TestTemplateSchedulesAWarmUp checks the contract between each template
// and the handler: if the schedule's input stopped being a warm-up, every
// ping would quietly be treated as a bad request.
func TestTemplateSchedulesAWarmUp(t *testing.T) {
	for _, name := range templates {
		t.Run(name, func(t *testing.T) {
			var tmpl struct {
				Resources struct {
					WarmUp struct {
						Properties struct {
							Target struct {
								Input string `yaml:"Input"`
							} `yaml:"Target"`
						} `yaml:"Properties"`
					} `yaml:"WarmUp"`
				} `yaml:"Resources"`
			}
			readTemplate(t, name, &tmpl)
			next := testRouter(t)
			routes := &fakeRoutes{next: next}

			handler{routes: routes}.invoke(context.Background(), json.RawMessage(tmpl.Resources.WarmUp.Properties.Target.Input))

			if routes.r != next {
				t.Errorf("the WarmUp schedule's input %q did not refresh the routes", tmpl.Resources.WarmUp.Properties.Target.Input)
			}
		})
	}
}

// frontDoor reads how a template exposes the function: its Function URL's
// auth type, empty when it has none, and the CLIENT_IP it sets.
func frontDoor(t *testing.T, name string) (auth, clientIP string) {
	t.Helper()
	var tmpl struct {
		Resources map[string]struct {
			Type       string `yaml:"Type"`
			Properties struct {
				AuthType    string `yaml:"AuthType"`
				Environment struct {
					Variables struct {
						ClientIP string `yaml:"CLIENT_IP"`
					} `yaml:"Variables"`
				} `yaml:"Environment"`
			} `yaml:"Properties"`
		} `yaml:"Resources"`
	}
	readTemplate(t, name, &tmpl)
	for _, r := range tmpl.Resources {
		switch r.Type {
		case "AWS::Lambda::Url":
			auth = r.Properties.AuthType
		case "AWS::Lambda::Function":
			clientIP = r.Properties.Environment.Variables.ClientIP
		}
	}
	return auth, clientIP
}

// TestTemplatesTrustOnlyTheirFrontDoor: a template that reads the client
// address from a header must keep its Function URL private, or any client
// could send that header and choose its own address.
func TestTemplatesTrustOnlyTheirFrontDoor(t *testing.T) {
	tests := []struct {
		template, wantAuth, wantClientIP string
	}{
		{"function-url.yaml", "NONE", "source-ip"},
		{"cloudfront.yaml", "AWS_IAM", "cloudfront"},
		// ALB events always use the last X-Forwarded-For entry, and with no
		// Function URL there is no other way in.
		{"alb.yaml", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.template, func(t *testing.T) {
			auth, clientIP := frontDoor(t, tt.template)
			if auth != tt.wantAuth || clientIP != tt.wantClientIP {
				t.Errorf("Function URL auth %q with CLIENT_IP %q, want %q with %q", auth, clientIP, tt.wantAuth, tt.wantClientIP)
			}
		})
	}
}

// TestCloudFrontTemplateForwardsWhatTheHandlerReads sends a viewer request
// through the template's origin request policy. CloudFront passes on only
// the headers and query strings the policy names, and the request must
// still be routed as the viewer sent it.
func TestCloudFrontTemplateForwardsWhatTheHandlerReads(t *testing.T) {
	var tmpl struct {
		Resources struct {
			OriginRequestPolicy struct {
				Properties struct {
					OriginRequestPolicyConfig struct {
						HeadersConfig struct {
							Headers []string `yaml:"Headers"`
						} `yaml:"HeadersConfig"`
						QueryStringsConfig struct {
							QueryStringBehavior string `yaml:"QueryStringBehavior"`
						} `yaml:"QueryStringsConfig"`
					} `yaml:"OriginRequestPolicyConfig"`
				} `yaml:"Properties"`
			} `yaml:"OriginRequestPolicy"`
		} `yaml:"Resources"`
	}
	readTemplate(t, "cloudfront.yaml", &tmpl)
	policy := tmpl.Resources.OriginRequestPolicy.Properties.OriginRequestPolicyConfig

	// The viewer request after the template's viewer-request function has
	// copied Host into X-Forwarded-Host and CloudFront has added the
	// viewer's address. sourceIp is CloudFront's edge.
	viewer := map[string]string{
		"cloudfront-viewer-address": "203.0.113.7:46532",
		"x-forwarded-host":          "video.example.com",
		"x-switchyard-debug":        debugKey,
	}
	req := urlRequest("GET", "", "198.51.100.1", "/live/master.m3u8", "")
	for _, name := range policy.HeadersConfig.Headers {
		if v, ok := viewer[strings.ToLower(name)]; ok {
			req.Headers[strings.ToLower(name)] = v
		}
	}
	if policy.QueryStringsConfig.QueryStringBehavior == "all" {
		req.RawQueryString = "bitrate=high"
	}
	resp := handler{routes: &fakeRoutes{r: testRouter(t)}, ip: fromCloudFront, debugKey: debugKey}.handle(context.Background(), req)

	t.Run("routes on the viewer's address, host and query", func(t *testing.T) {
		if got, want := resp.Headers["Location"], "https://bne.edge.example.com/live/master.m3u8?bitrate=high"; got != want {
			t.Errorf("Location = %q (status %d), want %q", got, resp.StatusCode, want)
		}
	})
	t.Run("passes on the debug key", func(t *testing.T) {
		if resp.Headers["X-Switchyard-Debug"] == "" {
			t.Errorf("no X-Switchyard-Debug header in %v, want the route explained", resp.Headers)
		}
	})
}

func BenchmarkHandle(b *testing.B) {
	sha := strings.Repeat("7fc0132", 10)[:64]
	h := handler{routes: &fakeRoutes{r: testRouter(b), versions: []store.Version{
		{Name: "asn", SHA256: sha, Loaded: time.Now()},
		{Name: "netmap", SHA256: sha, Loaded: time.Now()},
		{Name: "config", SHA256: sha, Loaded: time.Now()},
	}}}
	req := urlRequest("GET", "video.example.com", "203.0.113.7", "/live/master.m3u8", "")
	for b.Loop() {
		if resp := h.handle(context.Background(), req); resp.StatusCode != http.StatusFound {
			b.Fatalf("status = %d, want 302", resp.StatusCode)
		}
	}
}
