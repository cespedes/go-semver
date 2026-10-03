package diff

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"reflect"
	"testing"

	"github.com/cespedes/go-semver/api"
)

// module builds an api.Module with one package per entry of sources, which
// maps a path relative to the module to the source of the package.
func module(t *testing.T, path, version string, sources map[string]string) *api.Module {
	t.Helper()
	m := &api.Module{Path: path, Version: version}
	for rel, src := range sources {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "x.go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		importPath := path + rel
		pkg, err := new(types.Config).Check(importPath, fset, []*ast.File{f}, nil)
		if err != nil {
			t.Fatal(err)
		}
		m.Packages = append(m.Packages, &api.Package{ImportPath: importPath, Name: f.Name.Name, Types: pkg})
	}
	return m
}

func TestCompare(t *testing.T) {
	const base = "package p\n\nfunc A() {}\n\ntype T struct{ X int }\n"
	tests := []struct {
		name         string
		old, new     map[string]string
		wantLevel    Level
		wantIncompat []string
		wantCompat   []string
	}{
		{
			name:      "no change",
			old:       map[string]string{"": base},
			new:       map[string]string{"": base + "\nfunc unexported() {}\n"},
			wantLevel: Patch,
		},
		{
			name:       "addition",
			old:        map[string]string{"": base},
			new:        map[string]string{"": base + "\nfunc B() {}\n"},
			wantLevel:  Minor,
			wantCompat: []string{"B: added"},
		},
		{
			name:         "removal and addition",
			old:          map[string]string{"": base},
			new:          map[string]string{"": "package p\n\ntype T struct{ X int }\n\nfunc B() {}\n"},
			wantLevel:    Major,
			wantIncompat: []string{"A: removed"},
			wantCompat:   []string{"B: added"},
		},
		{
			name:         "changed signature",
			old:          map[string]string{"": base},
			new:          map[string]string{"": "package p\n\nfunc A(x int) {}\n\ntype T struct{ X int }\n"},
			wantLevel:    Major,
			wantIncompat: []string{"A: changed from func() to func(int)"},
		},
		{
			name:         "package removed and added",
			old:          map[string]string{"": base, "/old": "package old\n"},
			new:          map[string]string{"": base, "/new": "package new\n"},
			wantLevel:    Major,
			wantIncompat: []string{"package example.com/m/old: removed"},
			wantCompat:   []string{"package example.com/m/new: added"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Compare(module(t, "example.com/m", "v1.0.0", tt.old), module(t, "example.com/m", "v1.1.0", tt.new))
			if got := r.RequiredLevel(); got != tt.wantLevel {
				t.Errorf("RequiredLevel = %v, want %v (changes: %+v)", got, tt.wantLevel, r.Changes)
			}
			if got := r.Incompatible(); !reflect.DeepEqual(got, tt.wantIncompat) {
				t.Errorf("Incompatible = %q, want %q", got, tt.wantIncompat)
			}
			if got := r.Compatible(); !reflect.DeepEqual(got, tt.wantCompat) {
				t.Errorf("Compatible = %q, want %q", got, tt.wantCompat)
			}
		})
	}
}

func TestCompareMajorVersionPath(t *testing.T) {
	const src = "package p\n\nfunc A() {}\n"
	old := module(t, "example.com/m", "v1.0.0", map[string]string{"": src, "/sub": "package sub\n"})
	new := module(t, "example.com/m/v2", "v2.0.0", map[string]string{"": src, "/sub": "package sub\n"})
	if r := Compare(old, new); r.RequiredLevel() != Patch {
		t.Errorf("changes = %+v, want none", r.Changes)
	}
}

func TestCompareOrderAndWarnings(t *testing.T) {
	old := module(t, "example.com/m", "v1.0.0", map[string]string{"": "package p\n\nfunc Z() {}\n\nfunc Y() {}\n"})
	new := module(t, "example.com/m", "v2.0.0", map[string]string{"": "package p\n"})
	new.Packages[0].Errors = []error{types.Error{Msg: "boom"}}
	r := Compare(old, new)
	if got, want := r.Incompatible(), []string{"Y: removed", "Z: removed"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Incompatible = %q, want %q", got, want)
	}
	if len(r.Warnings) != 1 {
		t.Errorf("Warnings = %q, want one", r.Warnings)
	}
}

func TestDiscardStdout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	discardStdout(func() { os.Stdout.WriteString("noise\n") })
	if os.Stdout != w {
		t.Fatal("os.Stdout was not restored")
	}
	os.Stdout.WriteString("after\n")
	w.Close()
	if data, _ := io.ReadAll(r); string(data) != "after\n" {
		t.Errorf("stdout = %q, want only the text written outside discardStdout", data)
	}
}
