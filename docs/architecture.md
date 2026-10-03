# Architecture

This document explains how `go-semver` works internally: which packages it is
made of, what each one does and how, and how they call each other. For what
the tool is for and how to use it, see the [README](../README.md).

## Overview

`go-semver` checks released versions of Go modules. Everything it knows about a
module comes from a Go module proxy, and everything is done in memory: module
archives are never written to disk.

The work is a pipeline in which each stage is a package:

```
 proxy  ->  api  ->  diff  ->  check  ->  history  ->  cmd/go-semver
 fetch      load     compare   judge      walk the      command line
 files      & type-  two API   the        whole         and output
            check    versions  changes    history
```

1. **`proxy`** downloads the list of versions, metadata, `go.mod` file and zip
   archive of a module version.
2. **`api`** turns a module version into type-checked Go packages
   (`go/types`), downloading its dependencies as needed.
3. **`diff`** compares the packages of two versions and classifies each
   difference as compatible or incompatible.
4. **`check`** decides whether the version numbers are right given those
   differences, and whether each version is well formed.
5. **`history`** applies the previous stages to every pair of consecutive
   releases of a module.
6. **`cmd/go-semver`** is the command line front end.

### Package dependencies

Each package only imports the ones to its left in the pipeline above, plus
`golang.org/x/mod` and `golang.org/x/exp/apidiff`:

```
cmd/go-semver ---> history ---> check ---> diff ---> api ---> proxy
      |               |           |                   ^
      |               |           +-------------------+   (check also reads api.Module)
      |               +---> diff, api, proxy
      +---> check, diff, api, proxy

examples/listapi ---> api, proxy
```

External dependencies:

- `golang.org/x/mod` (`module`, `modfile`, `semver`, `zip`): path and version
  escaping, parsing of `go.mod`, version ordering and the limits of module zip
  files.
- `golang.org/x/exp/apidiff`: the actual comparison of two package APIs
  according to the Go compatibility rules.
- The standard library's `go/parser`, `go/types`, `go/build`, `go/importer`.

## Package `proxy`

A client for the [GOPROXY protocol](https://go.dev/ref/mod#goproxy-protocol).

`Client` has a base URL (default `https://proxy.golang.org`), an optional
`http.Client`, and a maximum zip size. Its zero value is usable. Methods:

| Method | Request | Returns |
| --- | --- | --- |
| `Versions` | `<module>/@v/list` | tagged versions, sorted with `semver.Sort` |
| `Info` | `<module>/@v/<version>.info` | `Info{Version, Time}` |
| `Latest` | `<module>/@latest` | `Info` of the latest version |
| `GoMod` | `<module>/@v/<version>.mod` | contents of `go.mod` |
| `Zip` | `<module>/@v/<version>.zip` | a `*zip.Reader` |

How it works:

- Every request goes through a single private function, `get`. It escapes the
  module path and the version with `module.EscapePath` / `module.EscapeVersion`
  (upper-case letters become `!x`), builds the URL, and performs the request
  with the caller's `context.Context`.
- An HTTP 404 or 410 becomes an error wrapping `ErrNotFound`, so callers can
  use `errors.Is`. Other statuses give an error that includes the first KiB of
  the response body.
- `Zip` has to return a `*zip.Reader`, which needs the whole archive in memory
  (the zip central directory is at the end of the file, so it cannot be
  streamed). It reads at most `MaxZipSize` bytes (default 500 MiB, the limit Go
  imposes on module zips) and fails if the proxy sends more.
- `Versions` does not return pseudo-versions: the proxy only lists tags. A
  module without tags yields an empty list, not an error.

## Package `api`

Turns a module version into type-checked packages. This is the largest
package. Its entry point is `Loader.Load(ctx, modulePath, version)`, which
returns an `*api.Module`:

```go
type Module struct {
	Path, Version string
	GoVersion     string     // "go" directive of go.mod
	DeclaredPath  string     // "module" directive of go.mod
	Packages      []*Package // importable packages, sorted by import path
}

type Package struct {
	ImportPath, Name string
	Types            *types.Package // the type-checked API
	Errors           []error        // parse and type errors; Types is still usable
}
```

### The `Loader`

A `Loader` owns all the state of the analysis and caches it, so loading
several versions of a module (or modules that share dependencies) does not
repeat downloads or type-checking:

- one `token.FileSet` and one `types.Context` shared by all packages;
- `sources`: downloaded module versions, keyed by `module.Version`;
- `pkgs`: type-checked packages, keyed by (module version, directory);
- the caches used to look up unrequired dependencies (see below);
- the standard library importer.

It is not safe for concurrent use. Because `go/types` importers have no
`context.Context` parameter, `Load` stores its context in the `Loader` for the
nested downloads triggered by imports.

### Loading a module version

`Load` does this:

1. **`Loader.source`** downloads the version, unless it is cached:
   `Proxy.Zip` for the code and `Proxy.GoMod` for the `go.mod` file. The
   proxy's `.mod` file is used instead of the one in the zip because it is
   authoritative and is synthesized for modules that have none. It is parsed
   with `modfile.ParseLax`, keeping the `module` path, the `go` version and the
   `require` list. The zip is handed to `newSourceFS`.
2. **`sourceFS`** (`sourcefs.go`) is an in-memory view of the module's source
   files. It indexes the archive and keeps only the non-test `.go` files that
   could be imported (not under `testdata`, `vendor`, or directories starting
   with `.` or `_`). Those entries are copied still compressed, with
   `zip.File.OpenRaw` / `zip.Writer.CreateRaw`, into a new small archive, so
   that the original (often mostly testdata and binaries) can be garbage
   collected; files are decompressed when read. It also records which
   directories hold a nested module (a `go.mod` below the root), whose
   packages are not part of this module.
3. For every candidate directory (`sourceFS.packageDirs`), **`loadPackage`**
   parses and type-checks the package. Directories where no file survives the
   build constraints are skipped. `main` packages and `internal` packages are
   type-checked (others may import `internal` ones) but are not included in
   `Module.Packages`, since they are not API. Any other failure, such as files
   of two different packages in one directory, makes `Load` fail.

### Type-checking a package

`Loader.check` does the work for one directory:

- **Choosing files.** A copy of `build.Default` is used, with cgo disabled and
  `OpenFile` pointing at the `sourceFS`. `build.Context.MatchFile` applies
  file name suffixes (`_linux.go`, `_amd64.go`) and `//go:build` lines for the
  platform where the tool runs. Files are parsed with comments, positions
  named `<module>@<version>/<file>`. Files that import `"C"`, and files of a
  `documentation` package, are dropped.
- **Checking.** `types.Config` is set up with: the shared `types.Context`; the
  language version from the module's `go` directive (or `go1.16` if it has
  none; left unrestricted if it is newer than the running toolchain);
  `IgnoreFuncBodies`, because only declarations matter, which makes it much
  faster; an `Error` callback that collects every error instead of stopping;
  and a `moduleImporter` bound to the module being checked.
- **Result.** The package is returned even if it has errors. They are stored in
  `Package.Errors`, and `Types` holds whatever could be built (unresolved types
  appear as `invalid type`).
- **Caching and cycles.** `loadPackage` caches by (module version, directory),
  so a package imported by many others is checked once and always yields the
  same `*types.Package`. An `inProgress` flag detects import cycles.

### Resolving imports

`moduleImporter` implements `types.ImporterFrom`. For an import path:

1. `unsafe` is `types.Unsafe`.
2. If the first path element has no dot, it is the standard library, loaded by
   `go/importer` in `source` mode from `GOROOT` of the running toolchain
   (one shared importer, so all modules see the same standard library types).
3. Otherwise `Loader.resolve` picks the module providing the package among
   the importing module itself and the modules in its `require` list: the one
   with the longest path that is a prefix of the import path. (Including the
   module itself and using the longest match is what makes `/v2` paths
   resolve correctly.) The package is then loaded from that module, whose
   own `go.mod` is used to resolve its imports in turn. Dependencies are
   therefore downloaded lazily, only when one of their packages is actually
   imported.
4. If no required module provides it, `resolveUnrequired` looks in the proxy.
   This happens for modules without `go.mod` and for modules older than Go 1.17
   that do not list all their dependencies. `findProvider` tries each prefix of
   the import path, longest first, as a module path; for each one that exists
   and contains the package directory it uses `versionAt`, which picks the
   latest release published before the version being analyzed (found by binary
   search over the release times from `Proxy.Info`; the earliest release if all
   are newer; `Proxy.Latest` for modules without tags). This approximates what
   a build at that time would have used. Without it, every type from such a
   dependency would be `invalid type`, and comparing versions would report
   bogus changes. Results, version lists and release times are cached.

### Known limitations of `api`

Only the platform of the running machine is analyzed and cgo is off, so APIs
that exist only with cgo or on other platforms are not seen (a type defined
only in a cgo file shows up as `invalid type`). `replace` and `exclude` are
ignored, and the lookup of unrequired dependencies is an approximation.

## Package `diff`

Compares two `*api.Module` values: `diff.Compare(old, new)` returns a
`*diff.Report`.

The comparison itself is done by
[`golang.org/x/exp/apidiff`](https://pkg.go.dev/golang.org/x/exp/apidiff),
which implements the rules of the Go compatibility promise. `Compare` wraps
it:

- It builds `apidiff.Module` values from the `types.Package`s of each side and
  calls `apidiff.ModuleChanges`, which matches packages by their path
  *relative to the module path* (so `example.com/m` and `example.com/m/v2` can
  be compared) and reports added and removed packages as well.
- It turns the result into `Change{Message, Compatible}` values and sorts them:
  incompatible changes first, then compatible ones, each group by message, so
  the output is deterministic (apidiff's order is not).
- It collects `Warnings` for packages that had type errors on either side,
  since the report may then be inaccurate.
- It runs `apidiff` with `os.Stdout` redirected to the null device
  (`discardStdout`). When apidiff meets invalid types it prints diagnostics
  straight to standard output, which would corrupt the output of this tool.
  This is a workaround for a problem in the library, not something to rely on.

`Report.RequiredLevel()` summarizes the changes as the smallest version bump
they justify: `Major` if there are incompatible changes, `Minor` if there are
only compatible ones (new API), `Patch` if there are none. `Level` is
`Patch < Minor < Major`.

## Package `check`

Applies the versioning rules. It has two entry points, both returning a list
of `Violation{Kind, Version, Message, Details}`:

**`check.Module(m *api.Module)`** checks one version in isolation:

- The version is a complete `vMAJOR.MINOR.PATCH[-PRERELEASE]` (with optional
  `+incompatible`); `semver.IsValid` alone accepts shorthands like `v1.2`, so
  the version must also equal its `semver.Canonical` form.
- The module path has the major version suffix the version requires
  (`module.SplitPathVersion` + `module.CheckPathMajor`, which also understands
  `+incompatible` and `gopkg.in` paths).
- The `module` directive of `go.mod` equals the path the module was loaded
  with (skipped if there is no directive).

**`check.Bump(r *diff.Report)`** checks the step from `r.Old` to `r.New`. In
this order, stopping at the first rule that decides the case:

1. Both versions must be valid, and the new one greater than the old one.
2. Within the same major version the module path may not change.
3. If the major version changed, anything goes.
4. If the major version is 0, or either version is a pre-release, there is no
   compatibility promise, so the API is not checked.
5. Otherwise the actual bump (major/minor/patch, computed by `actualLevel`) must
   be at least `r.RequiredLevel()`. A violation lists the offending changes in
   `Details`.

`check` does no I/O: it only looks at the `api.Module` and `diff.Report` values
it is given, which makes its rules easy to test with hand-made inputs.

## Package `history`

Runs the whole pipeline over a module's release history. `Analyzer.Analyze(ctx,
modulePath)` returns a `Result` with every `Version` found and a `Step` for
each pair of consecutive releases.

1. **Finding versions** (`listVersions`). The versions of the path given are
   listed with `Proxy.Versions`. Other major versions live under other paths,
   so `/v2`, `/v3`, ... are probed too (`pathForMajor`; `gopkg.in` paths use
   `.vN`). Probing stops after `maxMajorGap` (3) consecutive majors without
   versions, but never before the major of the path given, because modules may
   skip majors (`go-jose` has `/v3` and `/v4` but no `/v2`). Pre-releases are
   dropped unless `Prereleases` is set. All versions are sorted by semver.
2. **Retractions.** For each path, the `retract` directives of the `go.mod` of
   its highest release (`retractions`) mark versions as `Retracted`; those are
   neither downloaded nor compared.
3. **Walking.** For each remaining version, in order: `Loader.Load` (an error
   is stored in `Version.Err` and the version is skipped), `check.Module`, and,
   if there is a previous version that loaded successfully,
   `diff.Compare(previous, current)` followed by `check.Bump`. The previous
   version is the last *loaded* one, so a version that fails to load does not
   break the chain.
4. **Result.** `Result.Violations()` flattens everything in version order: the
   violations of each version, then those of the step that led to it.

One `api.Loader` is kept for the whole analysis, so dependencies shared by
many versions are downloaded and type-checked once. The `Progress` callback,
if set, is called before each version is loaded.

## Package `cmd/go-semver`

The command line. `main` calls `run(ctx, args, stdout, stderr) int` (separate
from `main` so it can be tested), which dispatches on the first argument:

- **`diff [-proxy URL] module@old module@new`**: `api.Loader.Load` for both,
  `diff.Compare`, then prints the changes, the required bump and the violations
  from `check.Module` (both versions) and `check.Bump`. The second argument may
  be just a version, using the path of the first.
- **`check [-proxy URL] [-v] [-prereleases] module`**: `history.Analyzer.Analyze`
  and prints a summary, warnings about versions that could not be analyzed or
  have type errors, and the violations. With `-v` it also prints progress
  (stderr) and the changes of every step.

The proxy URL comes from `-proxy`, else from the first `http(s)` entry of the
`GOPROXY` environment variable, else the default. Exit status: 0 if no
violations, 1 if there are some, 2 for usage errors or when a module cannot be
analyzed at all. Versions that fail to load do not change the exit status;
they are reported as warnings.

## Package `examples/listapi`

A small program that prints the exported identifiers of a module: it uses
`proxy.Client.Latest` (if no version is given) and `api.Loader.Load`, then walks
each package scope with `go/types` (`ObjectString`, the fields of structs and
the method sets of named types). It is a demonstration of how to use `api` on
its own.

## Call flow

`go-semver diff example.com/m@v1.0.0 v1.1.0`:

```
run
 └─ runDiff
     ├─ api.Loader.Load(example.com/m, v1.0.0)   ┐ each: Proxy.Zip, Proxy.GoMod,
     ├─ api.Loader.Load(example.com/m, v1.1.0)   ┘ newSourceFS, check each package
     │                                              (imports -> moduleImporter ->
     │                                               resolve -> Loader.source ...)
     ├─ diff.Compare(old, new)  ── apidiff.ModuleChanges
     ├─ check.Module(old), check.Module(new)
     └─ check.Bump(report)
```

`go-semver check example.com/m`:

```
run
 └─ runCheck
     └─ history.Analyzer.Analyze
         ├─ listVersions ── Proxy.Versions (per major), retractions ── Proxy.GoMod
         └─ for each version:
             ├─ api.Loader.Load          (shared Loader: cached sources and packages)
             ├─ check.Module
             └─ diff.Compare(previous, current) -> check.Bump
```

## Testing

No test uses the network. The tests of `proxy`, `api`, `history` and
`cmd/go-semver` start an `httptest` server that behaves like a module proxy,
serving zips built in memory, and point a `proxy.Client` at it (the helper is
repeated in each test file, as they are small). `diff` and `check` build
`api.Module` and `diff.Report` values by hand, parsing and type-checking small
snippets directly, since they do not depend on the proxy.

## Possible extensions

- **Checking a working tree** (`go-semver check .`): `api.Loader` only knows how
  to read a module from the proxy; it would need another source of module files
  in place of the zip behind `sourceFS` (for example a directory on disk).
- **Other platforms**: the build context in `Loader.init` is fixed to the
  running platform; loading several `GOOS`/`GOARCH` combinations and merging
  the APIs would remove the cgo and platform-specific false positives, at the
  cost of the standard library importer, which is also tied to the host.
- **Releasing memory**: a `Loader` keeps everything it loads; for very long
  histories it could drop versions that are no longer needed.
