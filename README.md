# switchyard

[![test](https://github.com/ElijahGlover/switchyard/actions/workflows/test.yml/badge.svg)](https://github.com/ElijahGlover/switchyard/actions/workflows/test.yml)

switchyard is a multi-CDN router for AWS Lambda, written in Go. It looks up each client IP in MMDB databases you control, matches the result against rules for the requested hostname, and answers with a `302` to the chosen CDN. The rules and databases live in S3, so changing where traffic goes means uploading a file, not deploying code.

It is built for video. The router makes one small decision per playback session, and the segments go straight to the CDN without touching Lambda. That is why one function can steer far more traffic than it carries.

```
client ─┬─► Function URL ─────────────────┐
        ├─► CloudFront ──► Function URL ──┤
        └─► ALB ──────────────────────────┤
            ┌─────────────────────────────┘
            ▼
            Lambda (Go, arm64)
            │ init: load config.yaml + every MMDB it names
            │ background, every 10s: conditional GET (ETag), validate, swap
            │ request: read the current routes, never wait on S3
            ▼
            302 https://<target><path>?<query>
```

Each front door is its own stack, so you deploy the one you need. See [Building and deploying](#building-and-deploying).

## Why

### Why more than one CDN

A CDN has limits. It may not reach every region a service needs, its internal network may be saturated, or its edge caches may lack the capacity for a large catalogue. It can also fail. A [2025 SMPTE paper on content steering](https://reznik.org/papers/JMI2025_Content_Steering.pdf) gives these as the reasons large streaming operators increasingly use several CDNs, for better reliability, scale and quality of experience. 

### Why not one hostname

One way to add a second CDN is DNS. Players keep one hostname, such as `video.example.com`, and the DNS provider answers each lookup with a CNAME to one CDN or another. The SMPTE paper calls this the simplest method, because the video URL never changes. It has three problems:

- **It sees the resolver, not the viewer.** An authoritative server sees the address of the resolver that asked, which [can be far from the viewer](https://www.rfc-editor.org/rfc/rfc7871). EDNS Client Subnet passes on part of the viewer's address, but only if the resolver sends it, and [Cloudflare's public resolver does not](https://developers.cloudflare.com/1.1.1.1/faq/).
- **Answers outlive their TTL.** A [2023 measurement study](https://ttl-violation-study.github.io) found 8.74% of the resolvers it tested keeping answers longer than their TTL. The SMPTE paper puts a DNS switch at 5 to 15 minutes when a CDN fails.
- **Every CDN needs a certificate for the same name.** When each CDN automates its certificate with a DNS challenge, `_acme-challenge` [can be delegated by CNAME to only one of them](https://www.fastly.com/blog/smarter-acme-challenge-for-multi-cdn-world). A newer challenge, dns-account-01, is designed to fix this.

### Why a redirect

With switchyard, each CDN keeps its own hostname, such as `cdn-a.example.net`, with its own certificate and settings. The player asks the router once, at the start of a session, and gets a `302` to one of them.

- It routes on the viewer's own address, read from the front door, not on a resolver's.
- The answer is marked `no-store`, so there is no cache to wait out. Busy containers pick up a new config within 10 seconds of an upload.
- A viewer gets the same CDN from every container, so they do not bounce between CDNs from one session to the next.
- Every answer can be explained. `cmd/validate` shows which CDN an address gets, and `X-Version` names the config that sent it.

The cost is one extra round trip when a session starts, and a session already playing stays on its CDN. To move those, HLS and DASH now define [content steering](https://reznik.org/papers/JMI2025_Content_Steering.pdf), which the player must support. switchyard does not implement it.

## Features

- **Routes on data you control.** A rule can match anything an MMDB can hold: an ASN, a network name, a PoP. Build your own databases from CSV, or use a third-party one.
- **Changes without a deploy.** While it serves, each container checks S3 for a new config every 10 seconds. A config is fully validated before it goes live, and a bad upload leaves the old one serving.
- **Never waits on S3.** Requests read the current routes from memory, and refreshes run in the background.
- **Sticky.** Targets are picked by weighted rendezvous hashing over the client IP and hostname. The hash is fixed, so a client gets the same CDN from every container, and taking a target out of a rule moves only the clients that were on it.
- **Blackholes, not failover.** Clients of a disabled target get `401`. Nobody else moves, so turning off one CDN never pushes its load onto another.
- **Safe redirects.** Whatever string a database holds, a redirect is a valid hostname under a suffix you allow. A fuzz test holds the router to this.
- **Three front doors.** There are CloudFormation stacks for a public Function URL, an existing ALB and CloudFront. Each reads the client address only from where its front door puts it.
- **Easy to inspect.** Every answer names the config that made it, and a debug key shows how a route was chosen.

## Installation

You need Go 1.26 or later, `make` and `zip`. Deploying also needs the AWS CLI.

```sh
git clone https://github.com/ElijahGlover/switchyard.git
cd switchyard
make check
# ok: 2 hosts, 3 tokens, 1 peers, 2 databases
```

`make check` builds the example databases from CSV and validates the example config. To see where a client would be sent:

```sh
go run ./cmd/validate -host video.example.com -ip 203.0.113.7 examples/config.yaml
# bne.edge.example.com
```

## Configuration

### Routing

The router reads one YAML file, `config.yaml`, and the databases it names. It has these keys:

- `databases`: a name for each MMDB file, and its path relative to the config.
- `tokens`: values read from a database for the client IP. Each has a `db`, a `path` to the field and a `default`.
- `peers`: labels for sets of a token's values, such as `partner` for two ASNs. Each has a `token`, its `labels` and a `default`.
- `allowed_suffixes`: the domains a redirect may point under.
- `disabled`: targets to blackhole.
- `hosts`: a list of rules for each hostname. A rule has `when`, the values it matches, and `targets`, each with a weight.

```yaml
databases:
  asn: asn.mmdb
  netmap: netmap.mmdb

tokens:
  asn:      { db: asn,    path: [asn],      default: none }
  network:  { db: netmap, path: [network],  default: offnet }
  location: { db: netmap, path: [location], default: syd }

peers:
  peer:
    token: asn
    default: other
    labels:
      partner: [AS64500, AS64501]

allowed_suffixes: [.example.com, .example.net]
disabled: []

hosts:
  video.example.com:
    - when: { network: onnet }
      targets:
        "{location}.edge.example.com": 100
    - when: { peer: partner }
      targets:
        cdn-b.example.net: 100
    - targets:
        cdn-a.example.net: 70
        cdn-b.example.net: 30
```

The full example, with comments, is in [examples/config.yaml](examples/config.yaml).

A request is routed in five steps:

1. **Tokens.** The client IP is looked up once per database, and each token reads one field from the record. Values are lowercased. A missing field, or a value that is still not a DNS label, gives the token's default, so bad database data cannot shape a redirect.
2. **Peers.** A client whose token value is in one of a peer's sets gets that set's label. Any other client gets the peer's default. From here on, peers work like tokens.
3. **Rules.** The hostname picks its rules, which are checked in order. A rule matches when each token or peer in `when` has one of the listed values, and the first match decides.
4. **Targets.** The rule picks one of its targets by weight. Targets can use tokens and peers as placeholders, so `{location}.edge.example.com` sends a client to its own PoP. Each rendered hostname is checked against `allowed_suffixes` before it is used. Quote a target that starts with a placeholder, or YAML reads the braces as a mapping.
5. **Blackholes.** A client whose target is in `disabled` gets `401`. An entry can be written as in the rule or as rendered, so `bne.edge.example.com` blackholes one PoP and `"{location}.edge.example.com"` blackholes all of them. To move viewers off a CDN instead, take it out of the rule's targets.

The config is checked in full before it goes live. Unknown YAML fields, unknown tokens, broken templates, bad weights, missing databases and match values that could never match are all errors, and every problem is reported at once. `go run ./cmd/validate config.yaml` runs the same checks locally.

### Responses

| Status | Body | When |
|---|---|---|
| `302` | | A rule gave the client a target. `Location` is `https://<target><path>?<query>`, keeping the request's path and query. |
| `401` | `forbidden` | The client's target is in `disabled`. |
| `404` | `unknown host` | The hostname has no rules under `hosts`. Hostnames match without regard to case. |
| `503` | `no target available` | No rule gave the client a target. |
| `400` | `bad client address` | The front door gave no usable client address. |
| `405` | `method not allowed` | The method was not `GET` or `HEAD`. |
| `503` | `routes not loaded` | The container has not loaded a config yet. |

### Databases

`mkmmdb` turns a CSV into an MMDB. The first column is the prefix, and every other column becomes a field. Empty cells are left out of the record.

```
prefix,network,location
203.0.113.0/25,onnet,bne
2001:db8:100::/40,onnet,mel
```

```sh
go run ./cmd/mkmmdb examples/netmap.csv examples/netmap.mmdb
```

When prefixes overlap, the most specific one wins regardless of row order.

Any other MMDB works the same way. Point a token's `path` at the field, and check the field names with [mmdbinspect](https://github.com/maxmind/mmdbinspect). A number in a path indexes into an array, as in `[subdivisions, 0, iso_code]`. A numeric field becomes its digits, so an ASN stored as a number matches `"64500"`, not `AS64500`.

Two providers have free editions:

- [IPinfo Lite](https://ipinfo.io/developers/ipinfo-lite-database) has country and ASN in one file, under CC BY-SA 4.0.
- [DB-IP Lite](https://db-ip.com/db/lite.php) has separate country, city and ASN files, under CC BY 4.0.

Both licences require you to credit the provider. Upload the files to S3 beside your config, and do not commit them to this repo.

### Environment

Each stack sets these on the function. If you deploy it another way, set them yourself.

| Variable | Default | Meaning |
|---|---|---|
| `CONFIG_BUCKET` | required | Bucket holding the config and databases. |
| `CONFIG_KEY` | required | Key of the config, such as `routing/config.yaml`. Database paths in the config are relative to its folder. |
| `REFRESH_INTERVAL` | `10s` | How often each container rechecks S3, as a Go duration. |
| `CLIENT_IP` | `source-ip` | Where a Function URL request's client address comes from: `source-ip` or `cloudfront`. See [Client address and hostname](#client-address-and-hostname). |
| `DEBUG_KEY` | unset | Turns on the debug header for requests that send this key. See [Debugging a route](#debugging-a-route). |

A missing required variable or an invalid value stops the function during init, and the error is in its log. The function's role needs `s3:GetObject` on the config's folder. `s3:ListBucket` on that prefix is optional. With it, a missing object is reported as `NoSuchKey` instead of `AccessDenied`.

### Client address and hostname

Routing on the wrong address means routing on one the client chose, so the router reads the address only from where the front door puts it:

| Front door | Client address | Set with |
|---|---|---|
| Public Function URL | `requestContext.http.sourceIp`, the TCP peer. Client headers such as `X-Forwarded-For` are ignored. | `CLIENT_IP=source-ip` (the default) |
| CloudFront in front of a Function URL | `CloudFront-Viewer-Address`, added by an origin request policy. `sourceIp` is CloudFront's edge. | `CLIENT_IP=cloudfront` |
| ALB | The last entry of `X-Forwarded-For`, which the load balancer appends. Earlier entries came from the client and are ignored. Single- and multi-value header modes both work. | Automatic: ALB events are recognised by their shape |

Use `CLIENT_IP=cloudfront` only when the Function URL accepts requests from CloudFront alone, with IAM auth and Origin Access Control. On a public URL anyone could send the header.

An ALB must keep its default `X-Forwarded-For` handling, which appends the client's address without a port. With `preserve`, the last entry is whatever the client sent. With `remove`, or with the client port turned on, every request gets `400`.

Each stack in `deploy/` sets `CLIENT_IP` for its own front door, and none takes it as a parameter. Switching to `cloudfront` without locking down the URL would let clients choose their address. A test checks each template's `CLIENT_IP` against its Function URL.

The hostname that picks the rules comes from the front door too. On a Function URL, the Host header is always the URL's own domain, so the router uses `X-Forwarded-Host`, which a CDN or proxy in front would set. A client calling a public Function URL directly can pick which host's rules apply, but the redirect is still bounded by `allowed_suffixes`. Behind an ALB, the Host header is the viewer's own and is used as is.

## Building and deploying

### Building

```sh
make test               # gofmt, vet, and every test under the race detector
make lint               # go mod tidy check, staticcheck and govulncheck
make fuzz               # fuzz tests: no database value or config can produce an unsafe redirect
make bench              # one routing decision
make build/router.zip   # the linux/arm64 bootstrap for Lambda
```

### Deploying

The default stack, `deploy/function-url.yaml`, is plain CloudFormation. It has an IAM role that can read only the routing prefix, the function (`provided.al2023`, arm64), a public Function URL, a one-minute warm-up schedule, and a log group with 3-day retention.

```sh
make bucket BUCKET=my-switchyard    # once
make deploy BUCKET=my-switchyard    # prints the Function URL
make upload BUCKET=my-switchyard    # validates, then uploads examples/

curl -sI -H 'X-Forwarded-Host: video.example.com' https://<function-url>/live/master.m3u8
```

`upload` sends the databases before the config, so a new config never names a database that is not there yet. To upload your own routes instead of the examples, point `DATA` at a folder holding your `config.yaml` and its databases. To see the on-net rule from your own connection, add your address to `examples/netmap.csv`.

These variables change what `make` does:

| Variable | Default | Meaning |
|---|---|---|
| `BUCKET` | required | Bucket for the routing data and the function's code. |
| `STACK` | `switchyard` | CloudFormation stack name. |
| `REGION` | `ap-southeast-2` | AWS Region. |
| `PREFIX` | `routing` | Folder in the bucket for the config and databases. |
| `DATA` | `examples` | Local folder that `upload` sends. |
| `TEMPLATE` | `deploy/function-url.yaml` | Stack template to deploy. |
| `PARAMS` | empty | Parameters a template adds, such as `ListenerArn=...` for `alb.yaml`. |
| `RESERVED` | `0` | Reserved concurrency, to cap a load test or a request flood. `0` leaves it unset. |
| `DEBUG_KEY` | unset | Read from the shell. Turns on the debug header. |

Each container rechecks S3 every `RefreshInterval` (10 seconds by default), with one conditional GET per object. For the example's three objects that is about 26,000 a day per busy container. Lambda freezes a container between invocations, so the refresh only runs while the container is serving traffic. After an idle spell, the first request is answered from the previous routes while the refresh catches up.

An EventBridge schedule also invokes the function every minute with `{"warm": true}`. That keeps a container warm, and the invocation waits for a refresh, so the container it reaches never sits on routes more than a minute old. It is the only invocation that waits on S3. It reaches one container, not every warm one, so the background refresh stays. The schedule costs about 43,000 invocations a month, and a failed warm-up shows in the function's error metrics.

### Behind an existing ALB

`alb.yaml` registers the function in a new target group, and adds a listener rule that sends the named hosts to it. The load balancer must be in the same account and Region as the stack. The target group is named after the stack, so the stack name can be at most 32 characters.

```sh
make deploy BUCKET=my-switchyard STACK=switchyard-alb TEMPLATE=deploy/alb.yaml \
  PARAMS="ListenerArn=arn:aws:elasticloadbalancing:... Priority=100 HostHeaders=video.example.com"
```

`Priority` must be unused on the listener, and one rule takes at most five hosts. Give each stack its own `STACK` name. Deploying a different template under an existing name replaces that stack's front door.

### Behind CloudFront, with no cache

`cloudfront.yaml` puts a distribution in front of a Function URL that only the distribution can call. A CloudFront Function copies the viewer's Host into `X-Forwarded-Host`, over anything the viewer sent. An origin request policy passes on that header, `CloudFront-Viewer-Address`, `X-Switchyard-Debug` and the query string, and nothing else. The managed CachingDisabled policy sends every request through to the router.

```sh
make deploy BUCKET=my-switchyard STACK=switchyard-cf TEMPLATE=deploy/cloudfront.yaml \
  PARAMS="Aliases=video.example.com CertificateArn=arn:aws:acm:us-east-1:..."
```

`make deploy` prints the distribution's URL. The certificate must be in `us-east-1` and cover every alias. Point each alias at the `DomainName` output with a CNAME or alias record. Without aliases, the distribution answers only on its `cloudfront.net` name, so that name needs rules under `hosts`. A new distribution takes several minutes to deploy, and the CloudFront Function is billed on every request.

### Which config answered

Every answer from the routes carries `X-Version`, with no key needed. It names each database and then the config, each with the first seven hex digits of its SHA-256 and how many seconds this container has held that version:

```
X-Version: asn 3b1f0c2 age 1755; netmap 9d24e7a age 1755; config 76e1998 age 12
```

To check whether a container has picked up an upload, compare the hashes with your files, such as `shasum -a 256 examples/config.yaml | cut -c1-7`. An object that has not changed keeps its age across refreshes, so `config 76e1998 age 12` means this container swapped in that config 12 seconds ago. Responses that never reach the routes, such as a `405` or a `503` before the first load, do not carry it.

### Debugging a route

Deploy with a debug key to see how the router chose a client's target:

```sh
export DEBUG_KEY=$(openssl rand -hex 16)
make deploy BUCKET=my-switchyard

curl -sI -H 'X-Forwarded-Host: video.example.com' -H "X-Switchyard-Debug: $DEBUG_KEY" \
  https://<function-url>/live/master.m3u8
# x-switchyard-debug: rule=2; target=cdn-a.example.net; candidates=cdn-a.example.net:70,cdn-b.example.net:30; asn=none; location=syd; network=offnet; peer=other
```

The header gives the deciding rule, counted from 0 in the host's list. Then come the target, the targets the rule picked between with their weights, and every token and peer for the client. It comes with redirects, with a `401`, where it names the disabled target, and with a `503` when no rule matched, as `rule=none` and the tokens. A request without the key, or with the wrong one, gets the normal response and no header. The key is compared in constant time.

The key is a `NoEcho` stack parameter, so CloudFormation hides it, but anyone who can read the function's configuration can see it. `make deploy` reads it from the shell and never prints it. The next deploy without `DEBUG_KEY` turns the header off. `cloudfront.yaml` already passes the header on. Behind a distribution of your own, add `X-Switchyard-Debug` to its origin request policy.

## Contributing

Every change is test-driven, and `make test` and `make lint` must pass before a pull request. CI runs the same targets. [AGENTS.md](AGENTS.md) has the layout and the invariants, [go-standards.md](go-standards.md) the code style and [testing-standards.md](testing-standards.md) the testing rules. Report security problems privately, as [SECURITY.md](SECURITY.md) describes.

## Licence

Copyright 2026 Elijah Glover. Licensed under the [Apache License 2.0](LICENSE).
