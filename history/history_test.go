package history

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cespedes/go-semver/check"
	"github.com/cespedes/go-semver/proxy"
)

// fakeModule is a module version served by the fake proxy. Its zip holds a
// single file with the package source.
type fakeModule struct {
	path, version, gomod, src string
}

func newFakeProxy(t *testing.T, mods ...fakeModule) *proxy.Client {
	t.Helper()
	zips := make(map[string][]byte)
	gomods := make(map[string]string)
	lists := make(map[string][]string)
	for _, m := range mods {
		key := m.path + "/@v/" + m.version
		lists[m.path] = append(lists[m.path], m.version)
		gomods[key] = m.gomod
		if m.src == "" {
			continue // listed, but not downloadable
		}
		var buf bytes.Buffer
		w := zip.NewWriter(&buf)
		f, err := w.Create(m.path + "@" + m.version + "/p.go")
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(m.src))
		w.Close()
		zips[key] = buf.Bytes()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		if p, ok := strings.CutSuffix(key, "/@v/list"); ok && lists[p] != nil {
			w.Write([]byte(strings.Join(lists[p], "\n")))
		} else if k, ok := strings.CutSuffix(key, ".zip"); ok && zips[k] != nil {
			w.Write(zips[k])
		} else if k, ok := strings.CutSuffix(key, ".mod"); ok && gomods[k] != "" {
			w.Write([]byte(gomods[k]))
		} else {
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &proxy.Client{BaseURL: srv.URL}
}

const (
	srcA  = "package m\n\nfunc A() {}\n"
	srcAB = "package m\n\nfunc A() {}\n\nfunc B() {}\n"
	srcB  = "package m\n\nfunc B() {}\n"
)

func gomod(path string, extra ...string) string {
	return "module " + path + "\n\ngo 1.21\n" + strings.Join(extra, "\n")
}

func versionList(r *Result) string {
	var s []string
	for _, v := range r.Versions {
		s = append(s, v.Version)
	}
	return strings.Join(s, " ")
}

func TestAnalyze(t *testing.T) {
	const m = "example.com/m"
	a := &Analyzer{Proxy: newFakeProxy(t,
		fakeModule{m, "v1.0.0", gomod(m), srcA},
		fakeModule{m, "v1.1.0", gomod(m), srcAB},
		fakeModule{m, "v1.1.1", gomod(m), srcB}, // breaking change in a patch
		fakeModule{m, "v1.2.0-rc.1", gomod(m), srcB},
		fakeModule{m + "/v2", "v2.0.0", gomod(m + "/v2"), srcB},
	)}
	// Starting from a later major gives the same result.
	for _, path := range []string{m, m + "/v2"} {
		res, err := a.Analyze(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := versionList(res), "v1.0.0 v1.1.0 v1.1.1 v2.0.0"; got != want {
			t.Fatalf("versions = %q, want %q", got, want)
		}
		if len(res.Steps) != 3 {
			t.Fatalf("steps = %d, want 3", len(res.Steps))
		}
		vs := res.Violations()
		if len(vs) != 1 || vs[0].Kind != check.KindBump || vs[0].Version != "v1.1.1" {
			t.Fatalf("violations = %v, want one bump violation for v1.1.1", vs)
		}
	}
}

func TestAnalyzeMajorGap(t *testing.T) {
	const m = "example.com/m"
	a := &Analyzer{Proxy: newFakeProxy(t,
		fakeModule{m, "v1.0.0", gomod(m), srcA},
		// There is no /v2.
		fakeModule{m + "/v3", "v3.0.0", gomod(m + "/v3"), srcB},
		fakeModule{m + "/v4", "v4.0.0", gomod(m + "/v4"), srcB},
	)}
	for _, path := range []string{m, m + "/v3", m + "/v4"} {
		res, err := a.Analyze(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := versionList(res), "v1.0.0 v3.0.0 v4.0.0"; got != want {
			t.Errorf("Analyze(%s): versions = %q, want %q", path, got, want)
		}
	}
}

func TestMajorOf(t *testing.T) {
	tests := map[string]int{
		"example.com/m": 1, "example.com/m/v2": 2, "example.com/m/v12": 12,
		"gopkg.in/yaml.v3": 3,
	}
	for path, want := range tests {
		if got, err := majorOf(path); err != nil || got != want {
			t.Errorf("majorOf(%q) = %d, %v; want %d", path, got, err, want)
		}
	}
}

func TestAnalyzePrereleases(t *testing.T) {
	const m = "example.com/m"
	a := &Analyzer{
		Proxy: newFakeProxy(t,
			fakeModule{m, "v1.0.0", gomod(m), srcA},
			fakeModule{m, "v1.1.0-rc.1", gomod(m), srcAB},
			fakeModule{m, "v1.1.0", gomod(m), srcAB},
		),
		Prereleases: true,
	}
	res, err := a.Analyze(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := versionList(res), "v1.0.0 v1.1.0-rc.1 v1.1.0"; got != want {
		t.Errorf("versions = %q, want %q", got, want)
	}
}

func TestAnalyzeRetracted(t *testing.T) {
	const m = "example.com/m"
	a := &Analyzer{Proxy: newFakeProxy(t,
		fakeModule{m, "v1.0.0", gomod(m), srcA},
		fakeModule{m, "v1.0.1", gomod(m), srcB}, // broken release
		fakeModule{m, "v1.0.2", gomod(m, "retract v1.0.1"), srcA},
	)}
	res, err := a.Analyze(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Versions[1].Retracted || res.Versions[0].Retracted || res.Versions[2].Retracted {
		t.Errorf("retracted = %v %v %v, want only v1.0.1", res.Versions[0].Retracted, res.Versions[1].Retracted, res.Versions[2].Retracted)
	}
	if len(res.Steps) != 1 || res.Steps[0].Old.Version != "v1.0.0" || res.Steps[0].New.Version != "v1.0.2" {
		t.Errorf("steps = %+v, want only v1.0.0 -> v1.0.2", res.Steps)
	}
	if vs := res.Violations(); len(vs) != 0 {
		t.Errorf("violations = %v, want none", vs)
	}
}

func TestAnalyzeLoadError(t *testing.T) {
	const m = "example.com/m"
	a := &Analyzer{Proxy: newFakeProxy(t,
		fakeModule{m, "v1.0.0", gomod(m), srcA},
		fakeModule{m, "v1.1.0", gomod(m), ""}, // cannot be downloaded
		fakeModule{m, "v1.2.0", gomod(m), srcAB},
	)}
	res, err := a.Analyze(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Versions[1].Err == nil || res.Versions[0].Err != nil || res.Versions[2].Err != nil {
		t.Errorf("errors = %v %v %v, want only v1.1.0", res.Versions[0].Err, res.Versions[1].Err, res.Versions[2].Err)
	}
	if len(res.Steps) != 1 || res.Steps[0].Old.Version != "v1.0.0" || res.Steps[0].New.Version != "v1.2.0" {
		t.Errorf("steps = %+v, want only v1.0.0 -> v1.2.0", res.Steps)
	}
}

func TestAnalyzeNotFound(t *testing.T) {
	a := &Analyzer{Proxy: newFakeProxy(t)}
	if _, err := a.Analyze(context.Background(), "example.com/none"); !errors.Is(err, proxy.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPathForMajor(t *testing.T) {
	tests := []struct {
		path  string
		major int
		want  string
	}{
		{"example.com/m", 1, "example.com/m"},
		{"example.com/m", 2, "example.com/m/v2"},
		{"example.com/m/v3", 1, "example.com/m"},
		{"example.com/m/v3", 4, "example.com/m/v4"},
		{"gopkg.in/yaml.v3", 1, "gopkg.in/yaml.v1"},
		{"gopkg.in/yaml.v3", 2, "gopkg.in/yaml.v2"},
	}
	for _, tt := range tests {
		if got, err := pathForMajor(tt.path, tt.major); err != nil || got != tt.want {
			t.Errorf("pathForMajor(%q, %d) = %q, %v; want %q", tt.path, tt.major, got, err, tt.want)
		}
	}
}
