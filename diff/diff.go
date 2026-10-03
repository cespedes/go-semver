// Package diff compares the exported API of two versions of a Go module.
//
// The comparison of packages is done by golang.org/x/exp/apidiff, which
// implements the rules of the Go compatibility promise; this package adapts it
// to the modules loaded by package api and derives from the result the
// smallest semantic version bump that the changes require.
package diff

import (
	"fmt"
	"go/types"
	"os"
	"slices"
	"strings"
	"sync"

	"golang.org/x/exp/apidiff"

	"github.com/cespedes/go-semver/api"
)

// Level is a kind of semantic version bump.
type Level int

const (
	// Patch is required when the API did not change.
	Patch Level = iota
	// Minor is required when the API only gained new features.
	Minor
	// Major is required when the API changed in a backward-incompatible way.
	Major
)

func (l Level) String() string {
	switch l {
	case Patch:
		return "patch"
	case Minor:
		return "minor"
	case Major:
		return "major"
	}
	return fmt.Sprintf("Level(%d)", int(l))
}

// Change is a single difference between two versions of a module API.
type Change struct {
	// Message describes the change, e.g. "Foo: removed".
	Message string
	// Compatible is false if code using the old API may not compile against
	// the new one.
	Compatible bool
}

// Report is the result of comparing two versions of a module.
type Report struct {
	Old, New *api.Module

	// Changes lists the incompatible changes first, then the compatible
	// ones, each group sorted by message.
	Changes []Change

	// Warnings lists the problems that may make the report incomplete,
	// such as packages that could not be fully type-checked.
	Warnings []string
}

// Compare reports on the differences between the APIs of two versions of a
// module. They may have different module paths (for example, when the major
// version changes from v1 to v2), in which case the packages are matched by
// their path relative to the module.
func Compare(old, new *api.Module) *Report {
	r := &Report{Old: old, New: new}
	r.Warnings = append(warnings(old), warnings(new)...)

	oldMod := &apidiff.Module{Path: old.Path, Packages: typesOf(old)}
	newMod := &apidiff.Module{Path: new.Path, Packages: typesOf(new)}
	var changes []apidiff.Change
	discardStdout(func() { changes = apidiff.ModuleChanges(oldMod, newMod).Changes })
	for _, c := range changes {
		r.Changes = append(r.Changes, Change{Message: c.Message, Compatible: c.Compatible})
	}
	slices.SortFunc(r.Changes, func(a, b Change) int {
		if a.Compatible != b.Compatible {
			if !a.Compatible {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Message, b.Message)
	})
	return r
}

var stdoutMu sync.Mutex

// discardStdout calls f with os.Stdout redirected to the null device.
//
// apidiff writes diagnostics straight to the standard output when it finds
// inconsistencies, which happen when the packages being compared have invalid
// types (see Report.Warnings). They would corrupt the output of programs
// using this package, and carry no information that is not already in the
// report.
func discardStdout(f func()) {
	stdoutMu.Lock()
	defer stdoutMu.Unlock()
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		f()
		return
	}
	defer null.Close()
	orig := os.Stdout
	os.Stdout = null
	defer func() { os.Stdout = orig }()
	f()
}

func typesOf(m *api.Module) []*types.Package {
	pkgs := make([]*types.Package, len(m.Packages))
	for i, p := range m.Packages {
		pkgs[i] = p.Types
	}
	return pkgs
}

func warnings(m *api.Module) []string {
	var w []string
	for _, p := range m.Packages {
		if len(p.Errors) > 0 {
			w = append(w, fmt.Sprintf("%s@%s: package %s has %d errors (first: %v)",
				m.Path, m.Version, p.ImportPath, len(p.Errors), p.Errors[0]))
		}
	}
	return w
}

// Incompatible returns the messages of the incompatible changes.
func (r *Report) Incompatible() []string { return r.messages(false) }

// Compatible returns the messages of the compatible changes.
func (r *Report) Compatible() []string { return r.messages(true) }

func (r *Report) messages(compatible bool) []string {
	var msgs []string
	for _, c := range r.Changes {
		if c.Compatible == compatible {
			msgs = append(msgs, c.Message)
		}
	}
	return msgs
}

// RequiredLevel returns the smallest version bump that the changes justify:
// Major if there are incompatible changes, Minor if there are only compatible
// ones, and Patch if the API did not change.
func (r *Report) RequiredLevel() Level {
	switch {
	case len(r.Incompatible()) > 0:
		return Major
	case len(r.Compatible()) > 0:
		return Minor
	}
	return Patch
}
