// Package proxy is a client for the GOPROXY protocol, used to download
// information about Go modules from a module proxy such as
// https://proxy.golang.org.
//
// See https://go.dev/ref/mod#goproxy-protocol for the protocol definition.
package proxy

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	modzip "golang.org/x/mod/zip"
)

// DefaultURL is the module proxy used when Client.BaseURL is empty.
const DefaultURL = "https://proxy.golang.org"

// ErrNotFound is returned (wrapped) when the proxy reports that a module or
// version does not exist (HTTP 404 or 410).
var ErrNotFound = errors.New("proxy: not found")

// Client talks to a module proxy. The zero value is ready to use and
// queries DefaultURL with http.DefaultClient.
type Client struct {
	// BaseURL is the proxy URL, without a trailing slash.
	// If empty, DefaultURL is used.
	BaseURL string

	// HTTPClient is used to perform requests.
	// If nil, http.DefaultClient is used.
	HTTPClient *http.Client

	// MaxZipSize is the maximum size in bytes of a module zip archive
	// accepted by Zip. If zero, it defaults to the limit imposed by Go
	// on module zip files (500 MiB).
	MaxZipSize int64
}

// Info is the metadata of a single module version.
type Info struct {
	Version string    // canonical version, e.g. "v1.2.3"
	Time    time.Time // commit time of the version
}

// Versions returns the tagged versions of the module known to the proxy,
// sorted in ascending semver order. Pseudo-versions are not included.
// A module with no tagged versions yields an empty list and no error.
func (c *Client) Versions(ctx context.Context, modulePath string) ([]string, error) {
	body, err := c.get(ctx, modulePath, "@v/list")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("proxy: reading version list of %s: %w", modulePath, err)
	}
	versions := strings.Fields(string(data))
	semver.Sort(versions)
	return versions, nil
}

// Info returns the metadata of the given module version.
func (c *Client) Info(ctx context.Context, modulePath, version string) (*Info, error) {
	elem, err := versionElem(version, "info")
	if err != nil {
		return nil, err
	}
	return c.info(ctx, modulePath, elem)
}

// Latest returns the metadata of the latest version of the module,
// as chosen by the proxy.
func (c *Client) Latest(ctx context.Context, modulePath string) (*Info, error) {
	return c.info(ctx, modulePath, "@latest")
}

// GoMod returns the contents of the go.mod file of the given module version.
func (c *Client) GoMod(ctx context.Context, modulePath, version string) ([]byte, error) {
	elem, err := versionElem(version, "mod")
	if err != nil {
		return nil, err
	}
	body, err := c.get(ctx, modulePath, elem)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("proxy: reading go.mod of %s@%s: %w", modulePath, version, err)
	}
	return data, nil
}

// Zip downloads the module zip archive of the given module version.
//
// The whole archive is held in memory, since the zip format cannot be read
// as a stream. An error is returned if it is larger than Client.MaxZipSize.
// File names inside the archive are prefixed with "<module>@<version>/".
func (c *Client) Zip(ctx context.Context, modulePath, version string) (*zip.Reader, error) {
	elem, err := versionElem(version, "zip")
	if err != nil {
		return nil, err
	}
	body, err := c.get(ctx, modulePath, elem)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	limit := c.MaxZipSize
	if limit == 0 {
		limit = modzip.MaxZipFile
	}
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("proxy: reading zip of %s@%s: %w", modulePath, version, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("proxy: zip of %s@%s is larger than %d bytes", modulePath, version, limit)
	}
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("proxy: invalid zip of %s@%s: %w", modulePath, version, err)
	}
	return r, nil
}

func (c *Client) info(ctx context.Context, modulePath, elem string) (*Info, error) {
	body, err := c.get(ctx, modulePath, elem)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var info Info
	if err := json.NewDecoder(body).Decode(&info); err != nil {
		return nil, fmt.Errorf("proxy: decoding info of %s: %w", modulePath, err)
	}
	return &info, nil
}

// versionElem returns the "@v/<version>.<ext>" request path element.
func versionElem(version, ext string) (string, error) {
	escaped, err := module.EscapeVersion(version)
	if err != nil {
		return "", fmt.Errorf("proxy: %w", err)
	}
	return "@v/" + escaped + "." + ext, nil
}

// get performs a GET request for <base>/<escaped module path>/<elem>.
// On success the caller must close the returned body.
func (c *Client) get(ctx context.Context, modulePath, elem string) (io.ReadCloser, error) {
	escaped, err := module.EscapePath(modulePath)
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultURL
	}
	url := strings.TrimSuffix(base, "/") + "/" + escaped + "/" + elem

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		return resp.Body, nil
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	detail := strings.TrimSpace(string(msg))
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return nil, fmt.Errorf("%w: GET %s: %s: %s", ErrNotFound, url, resp.Status, detail)
	}
	return nil, fmt.Errorf("proxy: GET %s: %s: %s", url, resp.Status, detail)
}
