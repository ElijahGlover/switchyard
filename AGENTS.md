# AGENTS.md

Guidance for coding agents working in this repo. Read the README first for what the router does.

## Commands

```sh
make test     # gofmt check, go vet, go test -race ./...
make lint     # go mod tidy check, staticcheck and govulncheck, at pinned versions
make fuzz     # run every fuzz target (FUZZTIME=30s each by default)
make cover    # list functions below full coverage
make bench    # BenchmarkResolve and BenchmarkHandle
make check    # build examples/*.mmdb from CSV and validate examples/config.yaml
go run ./cmd/validate -host video.example.com -ip 203.0.113.7 examples/config.yaml
make build/router.zip   # linux/arm64 bootstrap for Lambda
```

Go 1.26 or later. `make test` must pass before you hand work back.

## Layout

| Path | Role |
|---|---|
| `internal/route` | The core: config parsing, validation, tokens, rules, weighted pick. No AWS imports, ever. |
| `internal/store` | Loads the Router from S3, refreshes it in the background by ETag, keeps the last good Router. |
| `internal/mmdbcsv` | CSV to MMDB builder, also used by tests to make fixtures. |
| `internal/clientip` | Reads the client address from what a proxy adds: `X-Forwarded-For`, `CloudFront-Viewer-Address`. |
| `cmd/router` | Lambda handler: routes Function URL and ALB requests and handles the scheduled warm-up. Keep `main.go` to wiring; logic goes in `handler.go`. |
| `cmd/validate`, `cmd/mkmmdb` | Local tools. Logic lives in a testable `run` function. |
| `deploy/` | CloudFormation stacks, one per front door: `function-url.yaml` (the default, a public Function URL), `alb.yaml` (an existing ALB) and `cloudfront.yaml` (CloudFront with no cache). They repeat the same function, warm-up schedule, roles and log group, so change those in all three. `make deploy TEMPLATE=...` picks one. |

## Testing

Every change is test-driven: no production code without a failing test that needs it. [testing-standards.md](testing-standards.md) has the red, green, refactor cycle, what the tests look like, the anti-patterns to avoid and the golden test. Read it before writing code; its rules bind like this file's.

Before handing work back, run `make test` and `make lint`, then `make cover` and account for anything new it lists.

## Invariants

Do not weaken these without being asked.

- **Tokens are lowercase DNS labels.** Anything else falls back to the token's default.
- **The client address comes only from the front door's trusted source.** `sourceIp` on a public Function URL, `CloudFront-Viewer-Address` behind CloudFront, the last `X-Forwarded-For` entry on an ALB. Never the first entry, and never a header on a public URL. `TestTemplatesTrustOnlyTheirFrontDoor` checks each template's `CLIENT_IP` against its Function URL.
- **Disabled targets are blackholed, not rerouted.** Their clients get 401; nobody else moves.
- **Redirect targets stay under `allowed_suffixes`.** This is checked at load and again at request time.
- **A config is fully validated before it replaces the running one.** A bad upload keeps the old Router serving.
- **Objects are committed only with a Router built from them.** This is what makes a failed refresh retry in full.
- **The hash is fixed (FNV-1a plus splitmix64).** Never use `hash/maphash` or anything seeded per process.
- **Picks do not depend on map iteration order.** Use `sortedKeys` wherever order matters.
- **Requests never wait on S3.** `Store.Current` is a lock-free read of an atomic pointer. Refreshes run in one background goroutine (`Store.Run`), one at a time, each with a timeout of one interval. Lambda freezes the process between invocations, so a fetch caught by a freeze must be abandoned and retried, never waited on. The scheduled warm-up (`{"warm": true}`) is the only invocation that waits on a refresh, and `TestTemplateSchedulesAWarmUp` keeps each template's payload and the handler in step.
- **The hot path logs only warnings and errors.** At scale, logging every decision costs more than the compute.

## Code style

Go code follows [go-standards.md](go-standards.md). Read it before writing code; its rules bind like this file's. It covers layout, names, comments, control flow, errors, dependencies, concurrency, logging, performance and new config fields.

## Examples and docs

- **Documentation ranges only** in examples, tests and docs: `192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`, `2001:db8::/32`, with ASNs `AS64496` to `AS64511` and hostnames under `example.com` or `example.net`.
- **No third-party databases in the repo.** Point to IPinfo Lite or DB-IP Lite and keep the files in S3.
- **Clean room.** Do not add code, config, names or figures from any employer's systems.
- **Plain prose in the README and comments.** Short sentences, no em dashes.

## CI

`.github/workflows/test.yml` runs on every push to `main` and every pull request:

| Job | What it runs |
|---|---|
| `test` | `make test`, `make check` and `make build/router.zip`, on the oldest Go that `go.mod` allows and on the latest release |
| `lint` | `make lint` |
| `fuzz` | `make fuzz FUZZTIME=60s`, keeping any failing input as an artifact |

Keep CI green, and keep it running the same `make` targets as local work, so the two never disagree. Actions are pinned to commit SHAs with the tag in a comment. Dependabot (`.github/dependabot.yml`) opens weekly pull requests for Go modules and actions. To update one by hand, resolve the new tag with `git ls-remote --tags https://github.com/<owner>/<action>` and change the SHA and comment together.

## AWS

Never run `make bucket`, `make deploy`, `make upload`, or any `aws` command that creates, changes or deletes resources, without an explicit go-ahead in the current conversation. These cost money and change a real account. Read-only checks are fine. Region defaults to `ap-southeast-2`.

Do not commit or push unless asked.
