package proxy

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

var testZip = func() []byte {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, _ := w.Create("example.com/!mod@v1.2.0/mod.go")
	io.WriteString(f, "package mod\n")
	w.Close()
	return buf.Bytes()
}()

func newTestClient(t *testing.T) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/example.com/!mod/@v/list", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "v1.10.0\nv1.2.0\nv2.0.0-rc.1\nv1.9.3\n")
	})
	mux.HandleFunc("/example.com/empty/@v/list", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/example.com/!mod/@v/v1.2.0.info", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"Version":"v1.2.0","Time":"2024-05-06T07:08:09Z"}`)
	})
	mux.HandleFunc("/example.com/!mod/@latest", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"Version":"v1.10.0","Time":"2025-01-02T03:04:05Z"}`)
	})
	mux.HandleFunc("/example.com/!mod/@v/v1.2.0.mod", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "module example.com/Mod\n")
	})
	mux.HandleFunc("/example.com/!mod/@v/v1.2.0.zip", func(w http.ResponseWriter, r *http.Request) {
		w.Write(testZip)
	})
	mux.HandleFunc("/example.com/!mod/@v/v1.3.0.zip", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "not a zip")
	})
	mux.HandleFunc("/example.com/gone/@v/list", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	})
	mux.HandleFunc("/example.com/broken/@v/list", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL + "/"}
}

func TestVersions(t *testing.T) {
	c := newTestClient(t)
	got, err := c.Versions(context.Background(), "example.com/Mod")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"v1.2.0", "v1.9.3", "v1.10.0", "v2.0.0-rc.1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Versions = %v, want %v", got, want)
	}

	got, err = c.Versions(context.Background(), "example.com/empty")
	if err != nil || len(got) != 0 {
		t.Errorf("Versions(empty) = %v, %v; want empty, nil", got, err)
	}
}

func TestInfoAndLatest(t *testing.T) {
	c := newTestClient(t)
	info, err := c.Info(context.Background(), "example.com/Mod", "v1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC); info.Version != "v1.2.0" || !info.Time.Equal(want) {
		t.Errorf("Info = %+v", info)
	}
	latest, err := c.Latest(context.Background(), "example.com/Mod")
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != "v1.10.0" {
		t.Errorf("Latest = %+v", latest)
	}
}

func TestGoModAndZip(t *testing.T) {
	c := newTestClient(t)
	mod, err := c.GoMod(context.Background(), "example.com/Mod", "v1.2.0")
	if err != nil || string(mod) != "module example.com/Mod\n" {
		t.Errorf("GoMod = %q, %v", mod, err)
	}
	z, err := c.Zip(context.Background(), "example.com/Mod", "v1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(z.File) != 1 || z.File[0].Name != "example.com/!mod@v1.2.0/mod.go" {
		t.Errorf("Zip files = %v", z.File)
	}
	rc, err := z.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if data, _ := io.ReadAll(rc); string(data) != "package mod\n" {
		t.Errorf("mod.go = %q", data)
	}
}

func TestZipErrors(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	if _, err := c.Zip(ctx, "example.com/Mod", "v1.3.0"); err == nil {
		t.Error("invalid zip: want error")
	}
	c.MaxZipSize = int64(len(testZip)) - 1
	if _, err := c.Zip(ctx, "example.com/Mod", "v1.2.0"); err == nil {
		t.Error("zip over MaxZipSize: want error")
	}
	c.MaxZipSize = int64(len(testZip))
	if _, err := c.Zip(ctx, "example.com/Mod", "v1.2.0"); err != nil {
		t.Errorf("zip exactly MaxZipSize: %v", err)
	}
}

func TestErrors(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	if _, err := c.Versions(ctx, "example.com/unknown"); !errors.Is(err, ErrNotFound) {
		t.Errorf("404: err = %v, want ErrNotFound", err)
	}
	if _, err := c.Versions(ctx, "example.com/gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("410: err = %v, want ErrNotFound", err)
	}
	if _, err := c.Versions(ctx, "example.com/broken"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("500: err = %v, want non-NotFound error", err)
	}
	if _, err := c.Versions(ctx, "not a module path"); err == nil {
		t.Error("invalid module path: want error")
	}
	if _, err := c.Info(ctx, "example.com/Mod", "bad/version"); err == nil {
		t.Error("invalid version: want error")
	}
}

func TestContextCanceled(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Versions(ctx, "example.com/Mod"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
