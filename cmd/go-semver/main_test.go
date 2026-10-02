package main

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeModule struct{ path, version, src string }

// newFakeProxy serves the given modules, each with a single package, and
// returns its URL.
func newFakeProxy(t *testing.T, mods ...fakeModule) string {
	t.Helper()
	zips := make(map[string][]byte)
	gomods := make(map[string]string)
	lists := make(map[string][]string)
	for _, m := range mods {
		key := m.path + "/@v/" + m.version
		lists[m.path] = append(lists[m.path], m.version)
		gomods[key] = "module " + m.path + "\n\ngo 1.21\n"
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
	return srv.URL
}

const (
	srcA  = "package m\n\nfunc A() {}\n"
	srcAB = "package m\n\nfunc A() {}\n\nfunc B() {}\n"
	srcB  = "package m\n\nfunc B() {}\n"
)

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestDiff(t *testing.T) {
	url := newFakeProxy(t,
		fakeModule{"example.com/m", "v1.0.0", srcA},
		fakeModule{"example.com/m", "v1.1.0", srcAB},
		fakeModule{"example.com/m", "v1.0.1", srcB},
	)
	tests := []struct {
		name     string
		args     []string
		wantCode int
		want     []string
	}{
		{"compatible", []string{"diff", "-proxy", url, "example.com/m@v1.0.0", "v1.1.0"}, 0,
			[]string{"compatible changes:", "B: added", "required version bump: minor", "no violations found"}},
		{"violation", []string{"diff", "-proxy", url, "example.com/m@v1.0.0", "example.com/m@v1.0.1"}, 1,
			[]string{"incompatible changes:", "A: removed", "required version bump: major", "violation: v1.0.1: bump:", "1 violations found"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out, errOut := runCLI(t, tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, tt.wantCode, out, errOut)
			}
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("output does not contain %q:\n%s", w, out)
				}
			}
		})
	}
}

func TestCheck(t *testing.T) {
	good := newFakeProxy(t,
		fakeModule{"example.com/m", "v1.0.0", srcA},
		fakeModule{"example.com/m", "v1.1.0", srcAB},
	)
	code, out, errOut := runCLI(t, "check", "-proxy", good, "example.com/m")
	if code != 0 || !strings.Contains(out, "2 versions") || !strings.Contains(out, "no violations found") {
		t.Errorf("good: exit code = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}

	bad := newFakeProxy(t,
		fakeModule{"example.com/m", "v1.0.0", srcA},
		fakeModule{"example.com/m", "v1.0.1", srcB},
	)
	code, out, errOut = runCLI(t, "check", "-v", "-proxy", bad, "example.com/m")
	if code != 1 || !strings.Contains(out, "violation: v1.0.1") || !strings.Contains(out, "incompatible: A: removed") {
		t.Errorf("bad: exit code = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(errOut, "loading example.com/m@v1.0.0") {
		t.Errorf("-v: stderr = %q, want progress", errOut)
	}
}

func TestUsageErrors(t *testing.T) {
	url := newFakeProxy(t)
	tests := [][]string{
		{},
		{"bogus"},
		{"diff"},
		{"diff", "example.com/m@v1.0.0"},
		{"diff", "example.com/m", "example.com/m@v1.0.0"},
		{"diff", "example.com/m@v1.0.0", "v1.1"}, // the module does not exist
		{"check"},
		{"check", "-proxy", url, "example.com/none"},
	}
	for _, args := range tests {
		if code, out, errOut := runCLI(t, args...); code != 2 {
			t.Errorf("run(%q) exit code = %d, want 2\nstdout: %s\nstderr: %s", args, code, out, errOut)
		}
	}
	if code, out, _ := runCLI(t, "help"); code != 0 || !strings.Contains(out, "usage:") {
		t.Errorf("help: exit code = %d, stdout = %q", code, out)
	}
}
