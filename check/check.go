// Package check verifies that module versions follow the versioning rules
// of Go modules: semantic versioning, module paths that agree with the major
// version, and version bumps that are as large as the changes in the API
// require.
package check

import (
	"fmt"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/cespedes/go-semver/api"
	"github.com/cespedes/go-semver/diff"
)

// Kind classifies a violation.
type Kind string

const (
	// KindVersion is a version that is not a valid Go module version.
	KindVersion Kind = "version"
	// KindModulePath is a module path that does not agree with the version
	// or with the go.mod file.
	KindModulePath Kind = "module-path"
	// KindBump is a version bump too small for the changes in the API, or
	// otherwise inconsistent between two releases.
	KindBump Kind = "bump"
)

// Violation is a rule broken by a module version or by the step between two
// of them.
type Violation struct {
	Kind Kind

	// Version is the version of the module the violation is about.
	Version string

	// Message describes the problem.
	Message string

	// Details gives further information, such as the incompatible changes
	// that call for a bigger version bump.
	Details []string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s: %s: %s", v.Version, v.Kind, v.Message)
}

// Module checks the rules that concern a single version of a module: that
// the version is a valid Go module version, that the module path has the
// major version suffix required by it, and that the go.mod file declares the
// path the module was loaded with.
func Module(m *api.Module) []Violation {
	var vs []Violation
	add := func(kind Kind, format string, args ...any) {
		vs = append(vs, Violation{Kind: kind, Version: m.Version, Message: fmt.Sprintf(format, args...)})
	}

	if !validVersion(m.Version) {
		add(KindVersion, "%q is not a valid version: want vMAJOR.MINOR.PATCH[-PRERELEASE]", m.Version)
		return vs
	}

	if _, pathMajor, ok := module.SplitPathVersion(m.Path); !ok {
		add(KindModulePath, "invalid major version suffix in module path %s", m.Path)
	} else if err := module.CheckPathMajor(m.Version, pathMajor); err != nil {
		add(KindModulePath, "module path %s does not match the version: %v", m.Path, err)
	}

	if m.DeclaredPath == "" {
		// Modules without go.mod are legitimate (and get +incompatible
		// versions), so there is nothing to check.
	} else if m.DeclaredPath != m.Path {
		add(KindModulePath, "go.mod declares module %s, but the module path is %s", m.DeclaredPath, m.Path)
	}
	return vs
}

// Bump checks the step from r.Old to r.New, which must be two versions of
// the same module with r.Old being the earlier one:
//
//   - the module path may only change together with the major version;
//   - the version bump must be at least as big as r.RequiredLevel() says,
//     within a major version.
//
// Versions v0 and pre-releases carry no compatibility promise, so changes to
// the API are not checked when either version is one of them.
func Bump(r *diff.Report) []Violation {
	old, new := r.Old, r.New
	var vs []Violation
	add := func(format string, args ...any) *Violation {
		vs = append(vs, Violation{Kind: KindBump, Version: new.Version, Message: fmt.Sprintf(format, args...)})
		return &vs[len(vs)-1]
	}

	oldBase := strings.TrimSuffix(old.Version, "+incompatible")
	newBase := strings.TrimSuffix(new.Version, "+incompatible")
	if !validVersion(old.Version) || !validVersion(new.Version) {
		add("cannot compare %s with %s: invalid version", old.Version, new.Version)
		return vs
	}
	if semver.Compare(newBase, oldBase) <= 0 {
		add("%s is not greater than %s", new.Version, old.Version)
		return vs
	}

	sameMajor := semver.Major(newBase) == semver.Major(oldBase)
	if sameMajor && old.Path != new.Path {
		add("module path changed from %s to %s without a new major version", old.Path, new.Path)
	}
	if !sameMajor {
		return vs // breaking changes are allowed
	}
	if semver.Major(newBase) == "v0" || semver.Prerelease(oldBase) != "" || semver.Prerelease(newBase) != "" {
		return vs
	}

	required, actual := r.RequiredLevel(), actualLevel(oldBase, newBase)
	if actual >= required {
		return vs
	}
	switch required {
	case diff.Major:
		v := add("incompatible API changes between %s and %s require a new major version, but this is a %s release",
			old.Version, new.Version, actual)
		v.Details = r.Incompatible()
	case diff.Minor:
		v := add("new API between %s and %s requires at least a minor release, but this is a %s release",
			old.Version, new.Version, actual)
		v.Details = r.Compatible()
	}
	return vs
}

// validVersion reports whether v is a complete vMAJOR.MINOR.PATCH version,
// as required for the version tags of Go modules.
func validVersion(v string) bool {
	base := strings.TrimSuffix(v, "+incompatible")
	return semver.IsValid(base) && semver.Canonical(base) == base
}

// actualLevel returns the kind of bump from old to new, which must be valid,
// canonical versions with new greater than old.
func actualLevel(old, new string) diff.Level {
	switch {
	case semver.Major(old) != semver.Major(new):
		return diff.Major
	case semver.MajorMinor(old) != semver.MajorMinor(new):
		return diff.Minor
	}
	return diff.Patch
}
