package api

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cespedes/go-semver/proxy"
)

// fakeModule is a module version served by the fake proxy.
type fakeModule struct {
	path, version, gomod string
	files                map[string]string
}

func newFakeProxy(t *testing.T, mods ...fakeModule) *proxy.Client {
	t.Helper()
	zips := make(map[string][]byte)
	gomods := make(map[string]string)
	for _, m := range mods {
		var buf bytes.Buffer
		w := zip.NewWriter(&buf)
		for name, content := range m.files {
			f, err := w.Create(m.path + "@" + m.version + "/" + name)
			if err != nil {
				t.Fatal(err)
			}
			f.Write([]byte(content))
		}
		w.Close()
		key := m.path + "/@v/" + m.version
		zips[key] = buf.Bytes()
		gomods[key] = m.gomod
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		if k, ok := strings.CutSuffix(key, ".zip"); ok && zips[k] != nil {
			w.Write(zips[k])
		} else if k, ok := strings.CutSuffix(key, ".mod"); ok && zips[k] != nil {
			w.Write([]byte(gomods[k]))
		} else {
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &proxy.Client{BaseURL: srv.URL}
}

var libModule = fakeModule{
	path: "example.com/lib", version: "v1.0.0",
	gomod: "module example.com/lib\n\ngo 1.21\n",
	files: map[string]string{
		"lib.go": "package lib\n\ntype Thing struct{ N int }\n\nfunc New() *Thing { return &Thing{} }\n",
	},
}

var appModule = fakeModule{
	path: "example.com/app", version: "v1.2.0",
	gomod: "module example.com/app\n\ngo 1.21\n\nrequire example.com/lib v1.0.0\n",
	files: map[string]string{
		"go.mod": "module example.com/app\n",
		"app.go": `package app

import (
	"io"

	"example.com/lib"
)

type Handler struct {
	T *lib.Thing
	W io.Writer
}

func Make() *lib.Thing { return lib.New() }
`,
		"app_windows.go":           "package app\n\nfunc WinOnly() {}\n",
		"other.go":                 "//go:build !linux\n\npackage app\n\nfunc NotLinux() {}\n",
		"cgo.go":                   "package app\n\nimport \"C\"\n\nfunc Cgo() {}\n",
		"app_test.go":              "package app\n\nfunc InTest() {}\n",
		"internal/x/x.go":          "package x\n",
		"cmd/tool/main.go":         "package main\n\nfunc main() {}\n",
		"testdata/foo/foo.go":      "package foo\n",
		"sub/go.mod":               "module example.com/app/sub\n",
		"sub/sub.go":               "package sub\n",
		"sub2/sub2.go":             "package sub2\n\nconst C = 1\n",
		"onlywindows/w_windows.go": "package onlywindows\n",
	},
}

func TestLoad(t *testing.T) {
	l := &Loader{Proxy: newFakeProxy(t, appModule, libModule)}
	m, err := l.Load(context.Background(), "example.com/app", "v1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if m.GoVersion != "1.21" {
		t.Errorf("GoVersion = %q", m.GoVersion)
	}
	if m.DeclaredPath != "example.com/app" {
		t.Errorf("DeclaredPath = %q", m.DeclaredPath)
	}
	var paths []string
	for _, p := range m.Packages {
		paths = append(paths, p.ImportPath)
		for _, err := range p.Errors {
			t.Errorf("%s: unexpected error: %v", p.ImportPath, err)
		}
	}
	if got, want := strings.Join(paths, " "), "example.com/app example.com/app/sub2"; got != want {
		t.Fatalf("packages = %q, want %q", got, want)
	}

	scope := m.Packages[0].Types.Scope()
	for _, name := range []string{"WinOnly", "NotLinux", "Cgo", "InTest"} {
		if scope.Lookup(name) != nil {
			t.Errorf("%s should not be part of the package", name)
		}
	}
	h := scope.Lookup("Handler")
	if h == nil {
		t.Fatal("Handler not found")
	}
	if got, want := h.Type().Underlying().String(), "struct{T *example.com/lib.Thing; W io.Writer}"; got != want {
		t.Errorf("Handler = %s, want %s", got, want)
	}
	if got, want := scope.Lookup("Make").Type().String(), "func() *example.com/lib.Thing"; got != want {
		t.Errorf("Make = %s, want %s", got, want)
	}
}

func TestLoadMissingDependency(t *testing.T) {
	app := appModule
	app.gomod = "module example.com/app\n\ngo 1.21\n"
	l := &Loader{Proxy: newFakeProxy(t, app)}
	m, err := l.Load(context.Background(), "example.com/app", "v1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Packages[0].Errors) == 0 {
		t.Error("want type errors for the unresolved import")
	}
}

func TestLoadNotFound(t *testing.T) {
	l := &Loader{Proxy: newFakeProxy(t)}
	if _, err := l.Load(context.Background(), "example.com/app", "v1.2.0"); err == nil {
		t.Error("want error")
	}
}
