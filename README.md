# softprobe-go

Go SDK for the **Softprobe Hybrid** platform. It talks HTTP to
`softprobe-runtime` and gives test authors an ergonomic `Softprobe` /
`SoftprobeSession` pair that mirrors the TypeScript, Python, and Java SDKs.

## Status

This module is **not yet released** at a stable public import path. The
`go.mod` module name is `softprobe-go` (used via `replace` directives inside
this monorepo), and no CI workflow publishes a versioned module yet. The
`go get github.com/softprobe/softprobe-go@v0.5.0` command on the docs site
refers to a **planned** release and is not wired to this repository today.

Consume this module from source, typically via a `replace` directive:

```go
// e2e/go.mod (example)
require softprobe-go v0.0.0
replace softprobe-go => ../softprobe-go
```

The in-repo harnesses under [`e2e/go/`](../e2e/go/) already do this.

## Build and test

```bash
cd softprobe-go
go test ./...
```

## Minimal replay example

```go
package main

import (
    "softprobe-go/softprobe"
)

func main() {
    sp := softprobe.New(softprobe.Options{BaseURL: "http://127.0.0.1:8080"})
    session, err := sp.StartSession("replay")
    if err != nil {
        panic(err)
    }
    defer session.Close()

    if err := session.LoadCaseFromFile(
        "spec/examples/cases/fragment-happy-path.case.json",
    ); err != nil {
        panic(err)
    }

    hit, err := session.FindInCase(softprobe.CaseSpanPredicate{
        Direction: "outbound",
        Method:    "GET",
        Path:      "/fragment",
    })
    if err != nil {
        panic(err)
    }

    if err := session.MockOutbound(softprobe.MockRuleSpec{
        Direction: "outbound",
        Method:    "GET",
        Path:      "/fragment",
        Response:  hit.Response,
    }); err != nil {
        panic(err)
    }

    // Drive the SUT through the proxy with x-softprobe-session-id = session.ID().
}
```

## Public surface

Mirrors the TypeScript SDK:

- `Softprobe` — entry point (`StartSession`, `Attach`)
- `SoftprobeSession`:
  - `LoadCaseFromFile(path)` / `LoadCase(caseJSON)`
  - `FindInCase(predicate)` / `FindAllInCase(predicate)`
  - `MockOutbound(spec)` / `ClearRules()`
  - `SetPolicy(policyJSON)` / `SetAuthFixtures(fixturesJSON)`
  - `Close()`

Typed errors (recoverable via `errors.As`):

- `*RuntimeError` — non-2xx response from the runtime
- `*UnreachableError` — transport-layer failure
- `*UnknownSessionError` — stable `unknown_session` envelope (also matches `*RuntimeError`)
- `*CaseLoadError` — file read / parse / runtime load failure
- `*CaseLookupAmbiguityError` — more than one `FindInCase` match

## Canonical CLI

The `softprobe` command lives in [`softprobe-runtime/`](../softprobe-runtime/),
not in this package. This SDK only speaks the JSON control API over HTTP.

## License

Apache-2.0. See [`LICENSE`](./LICENSE) and the monorepo [`LICENSING.md`](../LICENSING.md) for the full dual-license map (server components are under the Softprobe Source License 1.0).
