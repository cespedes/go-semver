package check

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cespedes/go-semver/api"
	"github.com/cespedes/go-semver/diff"
)

func TestModule(t *testing.T) {
	tests := []struct {
		name     string
		m        api.Module
		wantKind Kind // "" means no violation
	}{
		{"ok v1", api.Module{Path: "example.com/m", Version: "v1.2.3", DeclaredPath: "example.com/m"}, ""},
		{"ok v2", api.Module{Path: "example.com/m/v2", Version: "v2.0.0", DeclaredPath: "example.com/m/v2"}, ""},
		{"ok prerelease", api.Module{Path: "example.com/m/v3", Version: "v3.0.0-rc.1"}, ""},
		{"ok incompatible", api.Module{Path: "example.com/m", Version: "v2.1.0+incompatible"}, ""},
		{"ok gopkg.in", api.Module{Path: "gopkg.in/yaml.v3", Version: "v3.0.1"}, ""},
		{"shorthand", api.Module{Path: "example.com/m", Version: "v1.2"}, KindVersion},
		{"no v prefix", api.Module{Path: "example.com/m", Version: "1.2.3"}, KindVersion},
		{"v2 without suffix", api.Module{Path: "example.com/m", Version: "v2.0.0"}, KindModulePath},
		{"v1 with suffix", api.Module{Path: "example.com/m/v2", Version: "v1.0.0"}, KindModulePath},
		{"v0 with suffix", api.Module{Path: "example.com/m/v2", Version: "v0.1.0"}, KindModulePath},
		{"bad suffix", api.Module{Path: "example.com/m/v1", Version: "v1.0.0"}, KindModulePath},
		{"go.mod mismatch", api.Module{Path: "example.com/m", Version: "v1.0.0", DeclaredPath: "example.com/other"}, KindModulePath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vs := Module(&tt.m)
			switch {
			case tt.wantKind == "" && len(vs) > 0:
				t.Errorf("unexpected violations: %v", vs)
			case tt.wantKind != "" && (len(vs) != 1 || vs[0].Kind != tt.wantKind):
				t.Errorf("violations = %v, want one of kind %s", vs, tt.wantKind)
			}
		})
	}
}

func report(oldPath, oldVersion, newPath, newVersion string, changes ...diff.Change) *diff.Report {
	return &diff.Report{
		Old:     &api.Module{Path: oldPath, Version: oldVersion},
		New:     &api.Module{Path: newPath, Version: newVersion},
		Changes: changes,
	}
}

func TestBump(t *testing.T) {
	const p = "example.com/m"
	breaking := diff.Change{Message: "A: removed"}
	feature := diff.Change{Message: "B: added", Compatible: true}
	tests := []struct {
		name string
		r    *diff.Report
		want string // substring of the only violation's message; "" for none
	}{
		{"patch, no changes", report(p, "v1.0.0", p, "v1.0.1"), ""},
		{"minor, no changes", report(p, "v1.0.0", p, "v1.1.0"), ""},
		{"minor with feature", report(p, "v1.0.0", p, "v1.1.0", feature), ""},
		{"patch with feature", report(p, "v1.0.0", p, "v1.0.1", feature), "requires at least a minor"},
		{"minor with breaking", report(p, "v1.0.0", p, "v1.1.0", breaking, feature), "require a new major"},
		{"patch with breaking", report(p, "v1.3.0", p, "v1.3.1", breaking), "require a new major"},
		{"major with breaking", report(p, "v1.0.0", p+"/v2", "v2.0.0", breaking), ""},
		{"v0 breaking", report(p, "v0.1.0", p, "v0.1.1", breaking), ""},
		{"v0 to v1", report(p, "v0.9.0", p, "v1.0.0", breaking), ""},
		{"new prerelease", report(p, "v1.0.0", p, "v1.1.0-rc.1", breaking), ""},
		{"from prerelease", report(p, "v1.1.0-rc.1", p, "v1.1.0", breaking), ""},
		{"prerelease to prerelease", report(p+"/v2", "v2.0.0-rc.1", p+"/v2", "v2.0.0-rc.2", breaking), ""},
		{"downgrade", report(p, "v1.1.0", p, "v1.0.0"), "not greater"},
		{"same version", report(p, "v1.1.0", p, "v1.1.0"), "not greater"},
		{"invalid version", report(p, "v1.1", p, "v1.2.0"), "invalid version"},
		{"path changed in same major", report(p, "v1.0.0", p+"2", "v1.1.0"), "module path changed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vs := Bump(tt.r)
			if tt.want == "" {
				if len(vs) > 0 {
					t.Errorf("unexpected violations: %v", vs)
				}
				return
			}
			if len(vs) != 1 || vs[0].Kind != KindBump || !strings.Contains(vs[0].Message, tt.want) {
				t.Errorf("violations = %v, want one containing %q", vs, tt.want)
			}
		})
	}
}

func TestBumpDetails(t *testing.T) {
	const p = "example.com/m"
	r := report(p, "v1.0.0", p, "v1.1.0",
		diff.Change{Message: "A: removed"}, diff.Change{Message: "B: added", Compatible: true})
	vs := Bump(r)
	if len(vs) != 1 || !reflect.DeepEqual(vs[0].Details, []string{"A: removed"}) || vs[0].Version != "v1.1.0" {
		t.Errorf("violations = %+v", vs)
	}
}
