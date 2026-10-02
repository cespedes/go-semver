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

> **Status:** early stage. Modules published on a module proxy can already
> be checked (see [Usage](#usage)); checking a local working tree and some of
> the checks listed below are not implemented yet.

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

## Usage

```sh
go install github.com/cespedes/go-semver/cmd/go-semver@latest

# Compare two versions of a module and check the version bump between them
go-semver diff example.com/mod@v1.4.0 example.com/mod@v1.5.0

# Check every released version of a module (and of its other major versions)
go-semver check example.com/mod
```

Modules are downloaded from `https://proxy.golang.org`, or from the proxy
given with `-proxy` or in the `GOPROXY` environment variable. Run
`go-semver check -v` to see the API changes of every release, and
`-prereleases` to include pre-release versions.

The exit status is 0 if no violation was found, 1 if there were violations,
and 2 on usage errors or if a module could not be analyzed, so the tool can be
used as a CI gate.

Checking the working tree against the latest release (`go-semver check .`) is
planned.

### Limitations

- Only the platform of the machine running the tool is analyzed, with cgo
  disabled: files importing `"C"` are ignored, so APIs that depend on them may
  be reported as changed.
- `replace` and `exclude` directives are ignored.
- Pre-releases and `v0` versions are exempt from the API compatibility rules.
- Versions that cannot be downloaded or type-checked are reported and skipped.

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
