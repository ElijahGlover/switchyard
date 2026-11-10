# Security policy

## Reporting a vulnerability

Report security problems privately, through GitHub's [private vulnerability reporting](https://github.com/ElijahGlover/switchyard/security/advisories/new). Please do not open a public issue or pull request for them.

A useful report has the config, with anything private removed, the request, what the router answered and what it should have answered. The `X-Version` header and the debug header, if you have the key, show which config and databases made the decision.

## Supported versions

Fixes land on `main`. Only the latest version is supported.

## In scope

The router makes a safe decision from data it does not fully trust, so these reports matter most:

- A redirect to a hostname outside `allowed_suffixes`.
- A database value that reaches a hostname without being a lowercase DNS label.
- A client choosing the address it is routed on, when `CLIENT_IP` is set correctly for the front door.
- A config that passes validation but fails or panics on a request, or a bad upload that replaces a good config.
- The debug header sent in reply to a request without the right key.

## Out of scope

These are documented in the README and work as intended:

- On a public Function URL, `X-Forwarded-Host` picks which host's rules apply. The redirect is still bounded by `allowed_suffixes`.
- `CLIENT_IP=cloudfront` on a Function URL that anyone can call lets clients choose their address. Use it only when the URL accepts requests from CloudFront alone.
- Anyone who can read the function's configuration can read `DEBUG_KEY`.
- Request floods against a public Function URL. Cap concurrency with `RESERVED` on `make deploy`.
- How accurate a third-party database is.
