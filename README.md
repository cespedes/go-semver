# go-semver

A tool to check whether a Go package or module versions its releases
correctly.

Go modules are expected to follow [Semantic Versioning](https://semver.org/),
and the Go toolchain builds on that expectation: the
[Go 1 compatibility promise](https://go.dev/doc/go1compat) and the
[module versioning rules](https://go.dev/ref/mod#versions) say that, within a
major version, a new release must not break code that compiled and worked
against an earlier one. `go-semver` verifies that a module actually keeps
that promise.

> **Status:** early stage. This document describes what the project aims to
> be; not everything below is implemented yet.

## What it checks

### 1. Correct use of SemVer

- Version tags are valid semantic versions with the `v` prefix Go requires
  (`v1.2.3`, `v2.0.0-rc.1`, ...).
- The module path matches the major version: `v2` and above must end in
  `/v2`, `/v3`, etc. (or use the equivalent `gopkg.in` convention), and
  `v0`/`v1` must not.
- The `module` line in `go.mod` agrees with the tag being published.
- Pre-release (`-rc.1`) and `v0.x` versions are treated according to their
  weaker stability guarantees.
- The bump between two consecutive releases is at least as large as the
  changes require (see below): breaking changes need a new major version, new
  API needs a new minor version, and anything else may be a patch.

### 2. API compatibility within a major version

Between any two releases that share a major version, the exported API of the
later one must be a backward-compatible superset of the earlier one. Following
the rules of the Go compatibility promise, `go-semver` looks for changes such
as:

- Removed or renamed exported identifiers (functions, methods, types, fields,
  constants, variables).
- Changed function or method signatures (parameters, results, variadics,
  receivers).
- Changed types of exported fields, variables or constants.
- Removed methods from a type, or new methods added to an interface that
  external code may implement (which breaks existing implementers).
- Changes that break comparability, assignability, or embedding.
- Newly required type parameters or tightened constraints on generic
  declarations.
- Removed or moved packages, including those under `internal/`-adjacent
  re-exports.
- Raised minimum Go version in `go.mod`, or newly required dependency
  versions, where this affects consumers.

Additions that the Go compatibility promise explicitly allows (new exported
identifiers, new methods on concrete types, new fields in structs that are
documented as not safe to use unkeyed, etc.) are not reported as breaking, but
are used to decide the minimum required version bump.

## Intended usage

The exact command-line interface is not final. The goal is something along
these lines:

```sh
# Compare two versions of a module
go-semver diff example.com/mod@v1.4.0 example.com/mod@v1.5.0

# Check every consecutive pair of released versions of a module
go-semver check example.com/mod

# Check the working tree against the latest release (e.g. in CI)
go-semver check .
```

The tool should exit with a non-zero status when it finds a violation, so it
can be used as a CI gate before tagging a release.

## Related work

- [`golang.org/x/exp/apidiff`](https://pkg.go.dev/golang.org/x/exp/apidiff)
  and [`gorelease`](https://pkg.go.dev/golang.org/x/exp/cmd/gorelease) compare
  the API of two versions and suggest a version bump.
- [`golang.org/x/mod/semver`](https://pkg.go.dev/golang.org/x/mod/semver)
  implements Go's flavour of semantic version parsing and comparison.

`go-semver` aims to combine this kind of API comparison with checks on the
whole release history of a module and its versioning conventions.

## License

MIT; see [LICENSE](LICENSE).
