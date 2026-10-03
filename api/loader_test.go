package api

import (
	"archive/zip"
	"bytes"
	"context"
	"go/types"
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
	time                 string // RFC 3339; defaults to defaultTime
}

const defaultTime = "2020-01-01T00:00:00Z"

func newFakeProxy(t *testing.T, mods ...fakeModule) *proxy.Client {
	t.Helper()
	zips := make(map[string][]byte)
	gomods := make(map[string]string)
	infos := make(map[string]string)
	lists := make(map[string][]string)
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
		lists[m.path] = append(lists[m.path], m.version)
		when := m.time
		if when == "" {
			when = defaultTime
		}
		infos[key] = `{"Version":"` + m.version + `","Time":"` + when + `"}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		if p, ok := strings.CutSuffix(key, "/@v/list"); ok && lists[p] != nil {
			w.Write([]byte(strings.Join(lists[p], "\n")))
		} else if k, ok := strings.CutSuffix(key, ".info"); ok && infos[k] != "" {
			w.Write([]byte(infos[k]))
		} else if k, ok := strings.CutSuffix(key, ".zip"); ok && zips[k] != nil {
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

func TestLoadUnrequiredDependency(t *testing.T) {
	lib := func(version, when, typ string) fakeModule {
		return fakeModule{
			path: "example.com/lib2", version: version, time: when,
			gomod: "module example.com/lib2\n",
			files: map[string]string{"sub/sub.go": "package sub\n\ntype Val " + typ + "\n"},
		}
	}
	// The module has no go.mod requirements, but imports a package of lib2
	// as it was when the module was published: between v1.0.0 and v1.1.0.
	app := fakeModule{
		path: "example.com/app2", version: "v1.0.0", time: "2021-01-01T00:00:00Z",
		gomod: "module example.com/app2\n",
		files: map[string]string{"app.go": `package app

import (
	"example.com/lib2/sub"
	"example.com/missing/pkg"
)

func Get() sub.Val { return 0 }

func Missing() pkg.T { return nil }
`},
	}
	l := &Loader{Proxy: newFakeProxy(t, app,
		lib("v1.0.0", "2020-01-01T00:00:00Z", "int"),
		lib("v1.1.0", "2022-01-01T00:00:00Z", "string"),
	)}
	m, err := l.Load(context.Background(), "example.com/app2", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	p := m.Packages[0]
	if got, want := p.Types.Scope().Lookup("Get").Type().String(), "func() example.com/lib2/sub.Val"; got != want {
		t.Errorf("Get = %s, want %s", got, want)
	}
	val := p.Types.Scope().Lookup("Get").Type().(*types.Signature).Results().At(0).Type()
	if got := val.Underlying().String(); got != "int" {
		t.Errorf("sub.Val is %s, want int (the version of lib2 available at the time)", got)
	}
	// Only the import that cannot be found is an error.
	if len(p.Errors) != 1 || !strings.Contains(p.Errors[0].Error(), "example.com/missing/pkg") {
		t.Errorf("errors = %v, want one about example.com/missing/pkg", p.Errors)
	}
}
