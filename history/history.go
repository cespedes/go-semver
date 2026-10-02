// Package history checks the whole release history of a Go module: it
// downloads every released version from a module proxy and applies the rules
// of package check to each version and to each pair of consecutive releases.
package history

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/cespedes/go-semver/api"
	"github.com/cespedes/go-semver/check"
	"github.com/cespedes/go-semver/diff"
	"github.com/cespedes/go-semver/proxy"
)

// Analyzer checks the history of modules. The zero value is ready to use.
// An Analyzer keeps downloaded modules in memory, so it should be reused
// only for modules that share dependencies, and is not safe for concurrent
// use.
type Analyzer struct {
	// Proxy is the client used to download modules.
	// If nil, a zero proxy.Client is used.
	Proxy *proxy.Client

	// Prereleases makes the analysis include pre-release versions
	// (v1.2.0-rc.1). By default they are ignored, since they carry no
	// compatibility promise.
	Prereleases bool

	// Progress, if not nil, is called before each version is downloaded.
	Progress func(modulePath, version string)

	loader *api.Loader
}

// Version is one released version of a module.
type Version struct {
	// Path is the module path of this version, which includes the major
	// version suffix (example.com/m/v2).
	Path    string
	Version string

	// Retracted is true if the module retracted this version. Retracted
	// versions are not downloaded nor compared with the others.
	Retracted bool

	// Module is the loaded version. It is nil if Retracted is true or if
	// Err is not nil.
	Module *api.Module

	// Err is the reason why the version could not be loaded. Versions that
	// failed to load are not compared with the others.
	Err error

	// Violations are the broken rules that concern this version alone.
	Violations []check.Violation
}

// Step is the change between two consecutive releases that were loaded.
type Step struct {
	Old, New   *Version
	Report     *diff.Report
	Violations []check.Violation
}

// Result is the outcome of analyzing the history of a module.
type Result struct {
	// Versions lists all the versions found, in ascending order, across
	// all the major versions of the module.
	Versions []*Version

	// Steps lists the comparisons between consecutive releases.
	Steps []*Step
}

// Violations returns all the violations found, version by version in
// ascending order: first those of each version, then those of the step to
// reach it.
func (r *Result) Violations() []check.Violation {
	steps := make(map[*Version]*Step)
	for _, s := range r.Steps {
		steps[s.New] = s
	}
	var vs []check.Violation
	for _, v := range r.Versions {
		vs = append(vs, v.Violations...)
		if s := steps[v]; s != nil {
			vs = append(vs, s.Violations...)
		}
	}
	return vs
}

// Analyze downloads all the released versions of the module, including those
// of its other major versions (found by probing /v2, /v3... until one has no
// versions), and checks them. modulePath may carry any
// major version suffix: the whole family of modules is analyzed.
//
// Versions that cannot be downloaded or type-checked are reported in
// Version.Err rather than failing the analysis.
func (a *Analyzer) Analyze(ctx context.Context, modulePath string) (*Result, error) {
	if a.Proxy == nil {
		a.Proxy = new(proxy.Client)
	}
	if a.loader == nil || a.loader.Proxy != a.Proxy {
		a.loader = &api.Loader{Proxy: a.Proxy}
	}

	versions, err := a.listVersions(ctx, modulePath)
	if err != nil {
		return nil, err
	}

	res := &Result{Versions: versions}
	var prev *Version // last version loaded successfully
	for _, v := range versions {
		if v.Retracted {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if a.Progress != nil {
			a.Progress(v.Path, v.Version)
		}
		v.Module, v.Err = a.loader.Load(ctx, v.Path, v.Version)
		if v.Err != nil {
			continue
		}
		v.Violations = check.Module(v.Module)
		if prev != nil {
			r := diff.Compare(prev.Module, v.Module)
			res.Steps = append(res.Steps, &Step{Old: prev, New: v, Report: r, Violations: check.Bump(r)})
		}
		prev = v
	}
	return res, nil
}

// listVersions lists the versions of all the major versions of the module in
// ascending order, with the retracted ones marked.
func (a *Analyzer) listVersions(ctx context.Context, modulePath string) ([]*Version, error) {
	var all []*Version
	for major := 1; ; major++ {
		path, err := pathForMajor(modulePath, major)
		if err != nil {
			return nil, err
		}
		list, err := a.Proxy.Versions(ctx, path)
		if err != nil && !errors.Is(err, proxy.ErrNotFound) {
			return nil, err
		}
		if len(list) == 0 {
			if major == 1 {
				// v0 and v1 share a path, but a module may start at v2.
				continue
			}
			break
		}
		retracted, err := a.retractions(ctx, path, list)
		if err != nil {
			return nil, err
		}
		for _, v := range list {
			if semver.Prerelease(v) != "" && !a.Prereleases {
				continue
			}
			all = append(all, &Version{Path: path, Version: v, Retracted: retracted(v)})
		}
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("history: no versions found for %s: %w", modulePath, proxy.ErrNotFound)
	}
	slices.SortStableFunc(all, func(x, y *Version) int {
		if c := semver.Compare(x.Version, y.Version); c != 0 {
			return c
		}
		return len(x.Path) - len(y.Path)
	})
	return all, nil
}

// retractions returns a function that tells whether a version of the module
// at path is retracted. Retractions are declared in the go.mod file of the
// highest release.
func (a *Analyzer) retractions(ctx context.Context, path string, list []string) (func(string) bool, error) {
	latest := ""
	for _, v := range list {
		if semver.Prerelease(v) == "" && (latest == "" || semver.Compare(v, latest) > 0) {
			latest = v
		}
	}
	if latest == "" {
		return func(string) bool { return false }, nil
	}
	data, err := a.Proxy.GoMod(ctx, path, latest)
	if err != nil {
		return nil, err
	}
	mf, err := modfile.ParseLax("go.mod", data, nil)
	if err != nil {
		return nil, fmt.Errorf("history: %s@%s: %w", path, latest, err)
	}
	return func(v string) bool {
		for _, r := range mf.Retract {
			if semver.Compare(v, r.Low) >= 0 && semver.Compare(v, r.High) <= 0 {
				return true
			}
		}
		return false
	}, nil
}

// pathForMajor returns the module path of the given major version of the
// module that modulePath belongs to. Major versions 0 and 1 have no suffix,
// except in gopkg.in paths, which always have one.
func pathForMajor(modulePath string, major int) (string, error) {
	prefix, _, ok := module.SplitPathVersion(modulePath)
	if !ok {
		return "", fmt.Errorf("history: invalid module path %q", modulePath)
	}
	switch {
	case strings.HasPrefix(modulePath, "gopkg.in/"):
		return fmt.Sprintf("%s.v%d", prefix, major), nil
	case major < 2:
		return prefix, nil
	}
	return fmt.Sprintf("%s/v%d", prefix, major), nil
}
