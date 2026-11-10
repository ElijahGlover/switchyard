# Testing standards

How tests are written and how changes are made test-first. These rules bind like [AGENTS.md](AGENTS.md), and where the two disagree, AGENTS.md wins. Code style is in [go-standards.md](go-standards.md).

## TDD is required

Every change is test-driven, following [Learn Go with Tests](https://github.com/quii/learn-go-with-tests). No production code without a failing test that needs it.

1. **Red.** Write the test for the next small behaviour and run it. Read the failure: the message must say what went wrong without opening the test. A compile failure for a missing function counts as red; write the stub, then watch the test fail on its assertion.
2. **Green.** Write the least code that passes. No extra branches, options or error cases that no test asks for.
3. **Refactor.** Improve the code and the tests with everything green. Do not change behaviour in this step.

Commit-sized steps: one behaviour per cycle. A bug fix starts with a test that reproduces the bug.

**Tests for code that already exists.** A test that has never failed proves nothing. After writing it, break the code it covers by hand, confirm the right test fails, then put the code back. If the break survives, the test is missing something, or the code is unnecessary and should go. (The Host header fallback in `requestHost` was removed this way.)

**Before handing work back,** run `make test` and `make lint`, then `make cover` and account for anything new it lists. Coverage finds untested behaviour; it is not a target, and a covered line can still be unchecked, as the runtime suffix guard was.

## What the tests look like

- **Table-driven** with named `t.Run` subtests. Helpers take `testing.TB` and call `t.Helper()`.
- **got and want.** Name results `got` and `want`, and failure messages say both.
- **Test behaviour through the public API.** Every `internal` package is tested black-box (`package route_test`). The only exception is the `cmd` packages, which test `package main` through their `run` or `handle` functions.
- **Time comes from `testing/synctest`,** not an injected clock. Inside `synctest.Test`, `time.Sleep(time.Minute)` returns at once with the fake clock moved on.
- **No mocking libraries.** Hand-written fakes only. A fake shares a contract test with the real implementation (`getterContract` in `internal/store`), so it cannot drift. The contract also runs against real S3 when `SWITCHYARD_TEST_BUCKET` is set.
- **Testable examples** (`ExampleXxx` with `// Output:`) for the functions a reader starts from: `route.New`, `Router.Resolve`, `Router.Explain`, `mmdbcsv.Build`. Add one when you add an entry point. They are documentation that cannot go stale.
- **Properties as fuzz tests.** Invariants that must hold for any input belong in a `FuzzXxx` with seeds for the known edge cases. Run `make fuzz` after touching tokens, templates or config parsing.
- **Concurrency under `-race`.** Shared state gets a test that hammers it from many goroutines.
- **Benchmarks** for anything on the request path. Report `make bench` before and after.
- **Fixtures are built inside the test** from CSV strings with `mmdbcsv.Build`, or with `mmdbwriter` for typed fields. Do not commit `.mmdb` files.
- **No network in tests**, apart from the opt-in real-S3 contract.
- **Error tests assert on a substring** of the message, so messages can improve without breaking tests.
- **No `t.Parallel`.** One store test swaps the global logger, `synctest` tests cannot run in parallel, and the suite takes about a second and a half anyway. The `tc := tc` copy is not needed since Go 1.22.

## Anti-patterns to avoid

From the [anti-patterns chapter](https://github.com/quii/learn-go-with-tests/blob/main/anti-patterns.md), with what each looks like here:

- **Evergreen tests.** Every new test is seen to fail, red-first or by breaking the code it covers.
- **Useless assertions.** A failure says what was got, what was wanted and which case. `got false, want true` is not a message.
- **Asserting on irrelevant detail, or spying on implementation.** Assert on outcomes: where a client is sent, or what the store costs in S3 requests and downloads. Never assert on call order, argument formats or internal state. The store's fake counts requests; it does not record them.
- **Several behaviours in one test.** One behaviour per test or subtest. If a test needs two phases, the earlier phase is setup and is not asserted.
- **Not listening to the tests.** A test that needs a seam in production code is a design problem. The store once had a clock field only tests used; it went when `synctest` made it unnecessary.
- **Excessive setup.** If a test needs many doubles or a long setup, simplify the code under test.
- **Big interfaces.** `store.Getter` has one method. Keep interfaces this small and declare them where they are used.
- **Exporting for tests.** Never export anything just so a test can reach it.
- **Complicated table tests.** A table whose loop needs flags or branches should be separate tests.

## The golden test

`TestResolveIsStable` is a golden test. It pins clients to targets because a hash change moves every viewer to a different CDN on deploy. Only update its table when that is the intended outcome, and say so.
