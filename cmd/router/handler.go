package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ElijahGlover/switchyard/internal/clientip"
	"github.com/ElijahGlover/switchyard/internal/route"
	"github.com/ElijahGlover/switchyard/internal/store"
	"github.com/aws/aws-lambda-go/events"
)

type routes interface {
	// Current returns the current routes without waiting, or nil before
	// the first load.
	Current() *store.Snapshot
	// Refresh revalidates the routes with S3.
	Refresh(ctx context.Context) error
}

// ipSource is where a Function URL request's client address comes from.
// ALB events always use the last X-Forwarded-For entry.
type ipSource int

const (
	// fromSourceIP is for a public Function URL: the TCP peer.
	fromSourceIP ipSource = iota
	// fromCloudFront is for CloudFront in front of the Function URL, which
	// must then accept requests from CloudFront alone: CloudFront-Viewer-Address.
	fromCloudFront
)

// parseIPSource reads CLIENT_IP: source-ip (the default) or cloudfront.
func parseIPSource(v string) (ipSource, error) {
	switch v {
	case "", "source-ip":
		return fromSourceIP, nil
	case "cloudfront":
		return fromCloudFront, nil
	}
	return 0, fmt.Errorf("CLIENT_IP %q: want source-ip or cloudfront", v)
}

type handler struct {
	routes routes
	ip     ipSource
	// debugKey, when set, is what a request sends in X-Switchyard-Debug to
	// be shown how its route was chosen.
	debugKey string
}

// request is what routing needs, whichever way the request arrived.
type request struct {
	method, host, path, query string
	client                    netip.Addr
	clientErr                 error
	debugKey                  string // what the request sent in X-Switchyard-Debug
}

// response is the router's answer before it is shaped for Lambda.
type response struct {
	status  int
	headers map[string]string
	body    string
}

// invoke handles one raw invocation and answers in the format of the event
// that arrived: a Function URL request, an ALB request, or the scheduled
// warm-up, whose input is {"warm": true}.
//
// A warm-up keeps this container warm and refreshes its routes while it is
// awake, since a frozen container's background refresh cannot run. Only the
// warm-up waits on S3, never a request. A failed refresh is returned so it
// shows in the Lambda error metrics; the current routes keep serving.
func (h handler) invoke(ctx context.Context, payload json.RawMessage) (any, error) {
	var kind struct {
		Warm           bool `json:"warm"`
		RequestContext struct {
			ELB *struct{} `json:"elb"`
		} `json:"requestContext"`
	}
	if err := json.Unmarshal(payload, &kind); err != nil {
		return nil, fmt.Errorf("decode event: %w", err)
	}
	switch {
	case kind.Warm:
		return nil, h.routes.Refresh(ctx)
	case kind.RequestContext.ELB != nil:
		var ev events.ALBTargetGroupRequest
		if err := json.Unmarshal(payload, &ev); err != nil {
			return nil, fmt.Errorf("decode ALB event: %w", err)
		}
		return h.handleALB(ctx, ev), nil
	default:
		var ev events.LambdaFunctionURLRequest
		if err := json.Unmarshal(payload, &ev); err != nil {
			return nil, fmt.Errorf("decode Function URL event: %w", err)
		}
		return h.handle(ctx, ev), nil
	}
}

// handle answers a Function URL request.
func (h handler) handle(ctx context.Context, ev events.LambdaFunctionURLRequest) events.LambdaFunctionURLResponse {
	req := request{
		method:   ev.RequestContext.HTTP.Method,
		host:     functionURLHost(ev),
		path:     ev.RawPath,
		query:    ev.RawQueryString,
		debugKey: ev.Headers["x-switchyard-debug"],
	}
	if h.ip == fromCloudFront {
		req.client, req.clientErr = clientip.ViewerAddress(ev.Headers["cloudfront-viewer-address"])
	} else {
		req.client, req.clientErr = netip.ParseAddr(ev.RequestContext.HTTP.SourceIP)
	}
	res := h.route(ctx, req)
	return events.LambdaFunctionURLResponse{StatusCode: res.status, Headers: res.headers, Body: res.body}
}

// handleALB answers an ALB request. A target group with multi-value
// headers on sends and expects multiValueHeaders; one without uses headers.
func (h handler) handleALB(ctx context.Context, ev events.ALBTargetGroupRequest) events.ALBTargetGroupResponse {
	header := func(name string) []string {
		if ev.MultiValueHeaders != nil {
			return ev.MultiValueHeaders[name]
		}
		if v, ok := ev.Headers[name]; ok {
			return []string{v}
		}
		return nil
	}
	req := request{method: ev.HTTPMethod, path: ev.Path, query: albQuery(ev)}
	if hosts := header("host"); len(hosts) > 0 {
		req.host = stripPort(hosts[len(hosts)-1])
	}
	req.client, req.clientErr = clientip.LastForwardedFor(header("x-forwarded-for")...)
	if keys := header("x-switchyard-debug"); len(keys) > 0 {
		req.debugKey = keys[len(keys)-1]
	}

	res := h.route(ctx, req)
	out := events.ALBTargetGroupResponse{
		StatusCode:        res.status,
		StatusDescription: fmt.Sprintf("%d %s", res.status, http.StatusText(res.status)),
		Body:              res.body,
	}
	if ev.MultiValueHeaders != nil {
		out.MultiValueHeaders = map[string][]string{}
		for k, v := range res.headers {
			out.MultiValueHeaders[k] = []string{v}
		}
	} else {
		out.Headers = res.headers
	}
	return out
}

// route answers with a 302 to the chosen target, keeping the path and
// query.
func (h handler) route(ctx context.Context, req request) response {
	if req.method != http.MethodGet && req.method != http.MethodHead {
		return text(http.StatusMethodNotAllowed, "method not allowed")
	}
	if req.clientErr != nil {
		slog.WarnContext(ctx, "bad client address", "err", req.clientErr)
		return text(http.StatusBadRequest, "bad client address")
	}
	cur := h.routes.Current()
	if cur == nil {
		return text(http.StatusServiceUnavailable, "routes not loaded")
	}
	res := h.answer(ctx, req, cur.Router)
	res.headers["X-Version"] = versionHeader(cur.Versions, time.Now())
	return res
}

// answer is what the routes say about a request.
func (h handler) answer(ctx context.Context, req request, r *route.Router) response {
	// Explain builds more than Resolve, so only a debug request pays for it.
	debug := h.debugging(req.debugKey)
	var d route.Decision
	var err error
	if debug {
		d, err = r.Explain(req.host, req.client)
	} else {
		d.Target, err = r.Resolve(req.host, req.client)
	}

	var res response
	switch {
	case errors.Is(err, route.ErrUnknownHost):
		return text(http.StatusNotFound, "unknown host")
	case errors.Is(err, route.ErrBlackholed):
		res = text(http.StatusUnauthorized, "forbidden")
	case err != nil:
		slog.WarnContext(ctx, "no target", "host", req.host, "err", err)
		res = text(http.StatusServiceUnavailable, "no target available")
	default:
		res = redirect(d.Target, req)
	}
	if debug {
		res.headers["X-Switchyard-Debug"] = debugHeader(d)
	}
	return res
}

func redirect(target string, req request) response {
	loc := "https://" + target + req.path
	if req.path == "" {
		loc += "/"
	}
	if req.query != "" {
		loc += "?" + req.query
	}
	res := plain(http.StatusFound, "")
	res.headers["Location"] = loc
	return res
}

// versionHeader names each object the routes were built from, with the
// start of its SHA-256 and how many whole seconds this container has had
// it, e.g. "db 7fc0132 age 1755; config 76e1998 age 1755".
func versionHeader(versions []store.Version, now time.Time) string {
	// Every request sends it, so it is built in one buffer sized up front.
	b := make([]byte, 0, 32*len(versions))
	for i, v := range versions {
		if i > 0 {
			b = append(b, "; "...)
		}
		b = append(b, v.Name...)
		b = append(b, ' ')
		b = append(b, v.SHA256[:7]...)
		b = append(b, " age "...)
		b = strconv.AppendInt(b, int64(now.Sub(v.Loaded)/time.Second), 10)
	}
	return string(b)
}

// debugging reports whether a request sent the function's debug key. The
// comparison takes as long however much of the key is right.
func (h handler) debugging(key string) bool {
	return h.debugKey != "" && subtle.ConstantTimeCompare([]byte(key), []byte(h.debugKey)) == 1
}

// debugHeader shows how a target was chosen: the deciding rule, the target,
// the candidates with their weights, then each token and peer. Every value
// is a DNS label or hostname, so none can break the header.
func debugHeader(d route.Decision) string {
	parts := []string{"rule=none"}
	if d.Rule >= 0 {
		cands := make([]string, len(d.Candidates))
		for i, c := range d.Candidates {
			cands[i] = fmt.Sprintf("%s:%d", c.Target, c.Weight)
		}
		parts = []string{fmt.Sprintf("rule=%d", d.Rule), "target=" + d.Target, "candidates=" + strings.Join(cands, ",")}
	}
	for _, name := range slices.Sorted(maps.Keys(d.Tokens)) {
		parts = append(parts, name+"="+d.Tokens[name])
	}
	return strings.Join(parts, "; ")
}

// functionURLHost prefers X-Forwarded-Host, because the Host header on a
// Function URL is always the URL's own domain, which is also in the request
// context. A CDN or proxy in front sets it to the viewer's hostname.
func functionURLHost(ev events.LambdaFunctionURLRequest) string {
	h, _, _ := strings.Cut(ev.Headers["x-forwarded-host"], ",")
	h = strings.TrimSpace(h)
	if h == "" {
		h = ev.RequestContext.DomainName
	}
	return stripPort(h)
}

func stripPort(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

// albQuery rebuilds the query string, which the ALB event only gives as a
// map. Keys are sorted so the redirect is stable; values are passed on as
// the ALB received them, still URL-encoded.
func albQuery(ev events.ALBTargetGroupRequest) string {
	multi := ev.MultiValueQueryStringParameters
	if multi == nil {
		multi = make(map[string][]string, len(ev.QueryStringParameters))
		for k, v := range ev.QueryStringParameters {
			multi[k] = []string{v}
		}
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(multi)) {
		for _, v := range multi[k] {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, "&")
}

func text(status int, msg string) response {
	return plain(status, msg+"\n")
}

// plain is the base of every response. A decision belongs to one client, so
// no shared cache may keep it. A Content-Type is set even on a redirect with
// no body, because a Function URL would otherwise call it application/json.
func plain(status int, body string) response {
	return response{
		status:  status,
		headers: map[string]string{"Content-Type": "text/plain; charset=utf-8", "Cache-Control": "private, no-store"},
		body:    body,
	}
}
