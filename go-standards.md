# Go standards

How Go code in this repo is written. The rules come from [Effective Go](https://go.dev/doc/effective_go) and [20 Golang practices for modern engineers](https://medium.com/@code.chandrashekhar/20-golang-practices-for-modern-engineers-fd9a4a21d043), adapted to this codebase. They bind like [AGENTS.md](AGENTS.md), and where the two disagree, AGENTS.md wins. Tests follow [testing-standards.md](testing-standards.md).

## Tools

- `make test` runs `gofmt`, `go vet` and the tests under the race detector.
- `make lint` checks that `go.mod` and `go.sum` are tidy, then runs `staticcheck` and `govulncheck` at the versions pinned in the Makefile.

## Packages and files

- Everything lives under `internal/`, so nothing outside the module can import it. Dependencies flow one way: `cmd` uses the `internal` packages, `store` uses `route`, and `route` imports no other package of ours and nothing from AWS.
- Split a package into files by job. `internal/route` has `router.go` for deciding, `build.go` for validating a config, `template.go` for targets and `hash.go` for the fixed hash.
- Order a file from most to least important: exported types, then the constructor, then exported methods, then private types and helpers. `internal/store/store.go` is the model.
- In `cmd`, `main.go` only wires things together. Logic goes in `handler.go` or a testable `run`.

## Names

- The wider the scope, the longer the name. Loop variables and receivers are a letter or two (`r *Router`, `b *builder`). Exported names and wide-scope variables say what they hold.
- Use MixedCaps, never underscores. Package names are one lowercase word.
- No `Get` on getters: a field `owner` has a method `Owner`. `Getter.Get` is fine, because it fetches from S3 rather than returning a field.
- A one-method interface is named for its method plus -er, like `Getter`.

## Comments

- Every exported name has a doc comment that starts with the name: "// Resolve returns ...", not "// This function ...". Every package has a package comment.
- Comments explain why, not what. Match the existing density.
- Write them in the plain prose that AGENTS.md asks for.

## Control flow

- Handle errors and edge cases first, then return. The main path stays at the lowest indent.
- No `else` after a branch that ends in `return`, `break` or `continue`.
- Keep functions short and shallow. When one grows past a screen or nests more than three deep, split it. `route.New` was 189 lines nested five deep; it now hands each config section to a small `builder` method.
- Write shared logic once. In the handler, `plain` sets the headers every response needs; in `route`, `safe` is the one check of a rendered target.

## Errors

- Wrap errors with context and `%w`, never `%v`: `fmt.Errorf("get %s: %w", key, err)`. Callers can then use `errors.Is` and `errors.As`.
- Use sentinel errors where callers branch on them: `route.ErrUnknownHost`, `route.ErrNoTarget`, `route.ErrBlackholed`, `store.ErrNotModified`. Their text starts with the package name, as in `route: unknown host`.
- Error strings are lowercase with no closing punctuation. They may start with a proper name, such as a header: `X-Forwarded-For: last entry: ...`.
- Validation reports every problem at once with `errors.Join`, each prefixed by its config path, e.g. `hosts.video.example.com[0].targets["..."]`.

## Interfaces and dependencies

- Accept interfaces, return concrete types. Declare an interface where it is used and keep it small. `store.Getter` and the handler's `routes` are the models.
- Pass dependencies in through constructors or struct fields: `store.New(getter, key)` and `handler{routes: ...}`. No mutable package-level state.
- Use the standard library first, including `slices` and `maps` before writing a generic helper. The current dependencies are `maxminddb-golang/v2`, `mmdbwriter`, `go.yaml.in/yaml/v3`, `aws-lambda-go` and `aws-sdk-go-v2`. Ask before adding another, and only add ones under licences compatible with Apache-2.0 (Apache, MIT, BSD, ISC).

## Context and concurrency

- A function that does I/O or can block takes `ctx context.Context` as its first parameter. Never store a context in a struct.
- Exported functions do not start goroutines. The caller decides: `main.go` runs `go routes.Run(ctx, interval)`.
- Every goroutine exits when its context ends. `TestRunStopsWhenItsContextEnds` checks this for `Run`.
- Choose the primitive by what the request path can afford. Data that requests read goes behind an atomic pointer (`Store.Current`), so a read never waits. Work done one at a time takes a mutex (`Store.Refresh`).

## Logging

- Log with `log/slog` as key-value pairs, and pass the context: `slog.WarnContext(ctx, "no target", "host", host, "err", err)`.
- The request path logs only warnings and errors.

## Performance

- Benchmark anything on the request path with `make bench`, before and after a change.
- Do work only when a request needs it. `Explain` runs only for a request with the debug key; others use the cheaper `Resolve`.
- Build a string that every response carries in one buffer sized up front, as `versionHeader` does.

## Config fields

A new config field needs a YAML tag, validation in `route.New`, a test, and an entry in `examples/config.yaml`. `KnownFields` makes unknown keys an error.

## Deliberately not used

These come up in Go style guides, and were weighed and turned down here:

- **A goroutine that owns state and talks over channels.** Requests would wait on that goroutine, and `Store.Current` must never wait.
- **goleak, wire or fx.** They are new dependencies. `synctest` already fails a test whose goroutines never exit, and constructors wire this small a program by hand. Mocking libraries and `t.Parallel` are covered in [testing-standards.md](testing-standards.md).
- **`sync.Pool`.** Lambda sends a container one request at a time, so a pool would add code and save nothing. Use one only if a benchmark shows the need.
- **`tool` directives in `go.mod`.** They would pull the linters' dependencies into `go.mod`. The Makefile already pins the linters' versions.
