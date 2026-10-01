// Package api loads the exported API of Go modules downloaded from a module
// proxy.
//
// Everything is kept in memory: module zip archives are read straight from
// the proxy, parsed and type-checked with go/types without being written to
// disk. The dependencies of a module are downloaded, lazily and only when one
// of their packages is imported, from the same proxy, using the versions
// listed in the requirements of the go.mod file of the module that imports
// them. The standard library is read from GOROOT of the running toolchain.
//
// Limitations:
//   - Only the platform of the running program is considered (build
//     constraints are evaluated for it), and cgo is disabled: files that
//     import "C" are ignored.
//   - replace and exclude directives, and vendoring, are ignored.
//   - Dependencies that are not listed in go.mod (modules declaring a go
//     version older than 1.17 do not have to list all of them) cannot be
//     resolved, which is reported as a type error of the affected package.
package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"go/version"
	"io"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	"github.com/cespedes/go-semver/proxy"
)

// Module is a module version and the packages that make up its API.
type Module struct {
	Path    string
	Version string

	// GoVersion is the "go" directive of the go.mod file, e.g. "1.21".
	// It is empty if the go.mod file does not have one.
	GoVersion string

	// Packages are the packages of the module that can be imported by other
	// modules (that is, excluding internal and main packages), sorted by
	// import path.
	Packages []*Package
}

// Package is a type-checked package of a Module.
type Package struct {
	ImportPath string
	Name       string
	Types      *types.Package

	// Errors are the problems found while parsing and type-checking the
	// package. Types is still usable, but may be incomplete.
	Errors []error
}

// Loader loads modules. The zero value is ready to use. A Loader caches
// downloaded modules and type-checked packages, so reusing it to load several
// versions of a module (or of modules with common dependencies) avoids
// repeating work. It is not safe for concurrent use.
type Loader struct {
	// Proxy is the client used to download modules.
	// If nil, a zero proxy.Client is used.
	Proxy *proxy.Client

	ctx     context.Context
	fset    *token.FileSet
	std     types.ImporterFrom
	types   *types.Context
	build   build.Context
	sources map[module.Version]*source
	pkgs    map[pkgKey]*pkgEntry
}

// source is a downloaded module version.
type source struct {
	mod       module.Version
	fs        *sourceFS
	goVersion string           // as in the go directive
	requires  []module.Version // as listed in go.mod
}

type pkgKey struct {
	mod module.Version
	dir string // relative to the module root
}

type pkgEntry struct {
	pkg        *Package
	err        error
	inProgress bool
}

// errNoGoFiles is returned for directories without any buildable file.
var errNoGoFiles = errors.New("no buildable Go files")

func (l *Loader) init() {
	if l.fset != nil {
		return
	}
	l.fset = token.NewFileSet()
	l.std = importer.ForCompiler(l.fset, "source", nil).(types.ImporterFrom)
	l.types = types.NewContext()
	l.build = build.Default
	l.build.CgoEnabled = false
	l.build.GOROOT = ""
	l.build.GOPATH = ""
	l.build.JoinPath = path.Join
	l.build.IsAbsPath = path.IsAbs
	l.sources = make(map[module.Version]*source)
	l.pkgs = make(map[pkgKey]*pkgEntry)
	if l.Proxy == nil {
		l.Proxy = new(proxy.Client)
	}
}

// Load downloads the given module version and type-checks its packages.
func (l *Loader) Load(ctx context.Context, modulePath, version string) (*Module, error) {
	l.init()
	l.ctx = ctx
	src, err := l.source(module.Version{Path: modulePath, Version: version})
	if err != nil {
		return nil, err
	}
	m := &Module{Path: modulePath, Version: version, GoVersion: src.goVersion}
	for _, dir := range src.fs.packageDirs() {
		e, err := l.loadPackage(src, dir)
		if errors.Is(err, errNoGoFiles) {
			continue
		}
		if err != nil {
			return nil, err
		}
		p := e.pkg
		if p.Name == "main" || hasInternal(p.ImportPath) {
			continue
		}
		m.Packages = append(m.Packages, p)
	}
	return m, nil
}

func hasInternal(importPath string) bool {
	return slices.Contains(strings.Split(importPath, "/"), "internal")
}

// source downloads (or returns from the cache) a module version.
func (l *Loader) source(mv module.Version) (*source, error) {
	if s, ok := l.sources[mv]; ok {
		return s, nil
	}
	zr, err := l.Proxy.Zip(l.ctx, mv.Path, mv.Version)
	if err != nil {
		return nil, err
	}
	// The go.mod file served by the proxy is the authoritative one: it
	// is synthesized for modules that do not have it.
	data, err := l.Proxy.GoMod(l.ctx, mv.Path, mv.Version)
	if err != nil {
		return nil, err
	}
	mf, err := modfile.ParseLax("go.mod", data, nil)
	if err != nil {
		return nil, fmt.Errorf("api: %s@%s: %w", mv.Path, mv.Version, err)
	}
	fsys, err := newSourceFS(zr, mv.Path+"@"+mv.Version+"/")
	if err != nil {
		return nil, fmt.Errorf("api: %s@%s: %w", mv.Path, mv.Version, err)
	}
	s := &source{mod: mv, fs: fsys}
	if mf.Go != nil {
		s.goVersion = mf.Go.Version
	}
	for _, r := range mf.Require {
		s.requires = append(s.requires, r.Mod)
	}
	l.sources[mv] = s
	return s, nil
}

// resolve finds the module providing importPath, as seen from src, and
// the directory of the package inside it.
func (l *Loader) resolve(src *source, importPath string) (*source, string, error) {
	best := module.Version{}
	for _, mv := range append([]module.Version{src.mod}, src.requires...) {
		if (importPath == mv.Path || strings.HasPrefix(importPath, mv.Path+"/")) && len(mv.Path) > len(best.Path) {
			best = mv
		}
	}
	if best.Path == "" {
		return nil, "", fmt.Errorf("no module providing package %s is required by %s", importPath, src.mod.Path)
	}
	dep := src
	if best != src.mod {
		var err error
		if dep, err = l.source(best); err != nil {
			return nil, "", err
		}
	}
	return dep, strings.TrimPrefix(strings.TrimPrefix(importPath, best.Path), "/"), nil
}

// moduleImporter resolves the imports of the packages of a module.
type moduleImporter struct {
	l   *Loader
	src *source
}

func (m moduleImporter) Import(importPath string) (*types.Package, error) {
	return m.ImportFrom(importPath, "", 0)
}

func (m moduleImporter) ImportFrom(importPath, _ string, _ types.ImportMode) (*types.Package, error) {
	if importPath == "unsafe" {
		return types.Unsafe, nil
	}
	if first, _, _ := strings.Cut(importPath, "/"); !strings.Contains(first, ".") {
		return m.l.std.ImportFrom(importPath, "", 0)
	}
	src, dir, err := m.l.resolve(m.src, importPath)
	if err != nil {
		return nil, err
	}
	e, err := m.l.loadPackage(src, dir)
	if err != nil {
		return nil, err
	}
	return e.pkg.Types, nil
}

// loadPackage parses and type-checks the package in directory dir (relative
// to the module root) of src.
func (l *Loader) loadPackage(src *source, dir string) (*pkgEntry, error) {
	key := pkgKey{src.mod, dir}
	if e, ok := l.pkgs[key]; ok {
		if e.inProgress {
			return nil, fmt.Errorf("import cycle involving %s", importPathOf(src, dir))
		}
		return e, e.err
	}
	e := &pkgEntry{inProgress: true}
	l.pkgs[key] = e
	e.pkg, e.err = l.check(src, dir)
	e.inProgress = false
	return e, e.err
}

func importPathOf(src *source, dir string) string {
	if dir == "" {
		return src.mod.Path
	}
	return src.mod.Path + "/" + dir
}

func (l *Loader) check(src *source, dir string) (*Package, error) {
	importPath := importPathOf(src, dir)
	if src.fs.inNestedModule(dir) {
		return nil, fmt.Errorf("package %s is in a nested module", importPath)
	}
	p := &Package{ImportPath: importPath}
	ctxt := l.build
	ctxt.OpenFile = func(name string) (io.ReadCloser, error) {
		data, err := src.fs.readFile(strings.TrimPrefix(path.Clean(name), "/"))
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	var files []*ast.File
	for _, name := range src.fs.goFiles(dir) {
		ok, err := ctxt.MatchFile("/"+dir, name)
		if err != nil || !ok {
			continue
		}
		rel := path.Join(dir, name)
		data, err := src.fs.readFile(rel)
		if err != nil {
			return nil, fmt.Errorf("package %s: %w", importPath, err)
		}
		f, err := parser.ParseFile(l.fset, src.mod.Path+"@"+src.mod.Version+"/"+rel, data, parser.ParseComments)
		if err != nil {
			p.Errors = append(p.Errors, err)
		}
		if f == nil || importsC(f) || f.Name.Name == "documentation" {
			continue
		}
		if p.Name == "" {
			p.Name = f.Name.Name
		} else if p.Name != f.Name.Name {
			return nil, fmt.Errorf("package %s: found packages %s and %s", importPath, p.Name, f.Name.Name)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("package %s: %w", importPath, errNoGoFiles)
	}

	conf := types.Config{
		Context:          l.types,
		GoVersion:        l.langVersion(src),
		IgnoreFuncBodies: true,
		Importer:         moduleImporter{l, src},
		Error:            func(err error) { p.Errors = append(p.Errors, err) },
	}
	p.Types, _ = conf.Check(importPath, l.fset, files, nil)
	return p, nil
}

// langVersion returns the language version to use for the packages of src,
// or "" if it cannot be restricted.
func (l *Loader) langVersion(src *source) string {
	if src.goVersion == "" {
		return "go1.16"
	}
	v := "go" + src.goVersion
	if !version.IsValid(v) || version.Compare(v, runtime.Version()) > 0 {
		return ""
	}
	return v
}

func importsC(f *ast.File) bool {
	for _, imp := range f.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err == nil && p == "C" {
			return true
		}
	}
	return false
}
