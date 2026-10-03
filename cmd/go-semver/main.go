// Command go-semver checks whether Go modules use semantic versioning
// correctly and keep the Go compatibility promise within a major version.
//
// Usage:
//
//	go-semver diff [flags] module@old module@new
//	go-semver check [flags] module
//
// The diff command compares the API of two versions of a module and checks
// the versioning rules for the step between them. The second argument may be
// just a version (v1.5.0), meaning the same module path as the first.
//
// The check command downloads all the released versions of a module (and of
// its other major versions) and checks each version and each pair of
// consecutive releases.
//
// Modules are downloaded from the module proxy given by the -proxy flag, or
// else by the first http(s) URL in the GOPROXY environment variable, or else
// from https://proxy.golang.org.
//
// The exit status is 0 if no violation was found, 1 if there were violations,
// and 2 on usage errors or if a module could not be analyzed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cespedes/go-semver/api"
	"github.com/cespedes/go-semver/check"
	"github.com/cespedes/go-semver/diff"
	"github.com/cespedes/go-semver/history"
	"github.com/cespedes/go-semver/proxy"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage:
	go-semver diff [flags] module@old module@new
	go-semver check [flags] module
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	var violations int
	switch args[0] {
	case "diff":
		violations, err = runDiff(ctx, args[1:], stdout, stderr)
	case "check":
		violations, err = runCheck(ctx, args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "go-semver: unknown command %q\n%s", args[0], usage)
		return 2
	}
	switch {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		fmt.Fprintln(stderr, "go-semver:", err)
		return 2
	case violations > 0:
		return 1
	}
	return 0
}

// flags creates the flag set of a command, with the flags common to all.
func flags(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("go-semver "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	proxyURL := fs.String("proxy", proxyFromEnv(), "module proxy `URL` (default https://proxy.golang.org)")
	return fs, proxyURL
}

// proxyFromEnv returns the first http(s) URL of the GOPROXY environment
// variable, or "" if there is none.
func proxyFromEnv() string {
	list := strings.FieldsFunc(os.Getenv("GOPROXY"), func(r rune) bool { return r == ',' || r == '|' })
	for _, p := range list {
		if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
			return p
		}
	}
	return ""
}

// parseModuleVersion splits "path@version". If arg is only a version and
// defaultPath is not empty, defaultPath is used as the path.
func parseModuleVersion(arg, defaultPath string) (path, version string, err error) {
	path, version, ok := strings.Cut(arg, "@")
	switch {
	case !ok && defaultPath != "" && strings.HasPrefix(arg, "v") && !strings.Contains(arg, "/"):
		return defaultPath, arg, nil
	case !ok || path == "" || version == "":
		return "", "", fmt.Errorf("%q is not of the form module@version", arg)
	}
	return path, version, nil
}

func runDiff(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	fs, proxyURL := flags("diff", stderr)
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if fs.NArg() != 2 {
		return 0, errors.New("diff needs two arguments: module@old module@new")
	}
	oldPath, oldVersion, err := parseModuleVersion(fs.Arg(0), "")
	if err != nil {
		return 0, err
	}
	newPath, newVersion, err := parseModuleVersion(fs.Arg(1), oldPath)
	if err != nil {
		return 0, err
	}

	l := &api.Loader{Proxy: &proxy.Client{BaseURL: *proxyURL}}
	oldMod, err := l.Load(ctx, oldPath, oldVersion)
	if err != nil {
		return 0, err
	}
	newMod, err := l.Load(ctx, newPath, newVersion)
	if err != nil {
		return 0, err
	}
	r := diff.Compare(oldMod, newMod)

	fmt.Fprintf(stdout, "old: %s@%s\nnew: %s@%s\n", oldPath, oldVersion, newPath, newVersion)
	printList(stdout, "incompatible changes:", r.Incompatible())
	printList(stdout, "compatible changes:", r.Compatible())
	printList(stdout, "warnings:", r.Warnings)
	fmt.Fprintf(stdout, "required version bump: %s\n", r.RequiredLevel())

	var vs []check.Violation
	vs = append(vs, check.Module(oldMod)...)
	vs = append(vs, check.Module(newMod)...)
	vs = append(vs, check.Bump(r)...)
	printViolations(stdout, vs)
	return len(vs), nil
}

func runCheck(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	fs, proxyURL := flags("check", stderr)
	prereleases := fs.Bool("prereleases", false, "include pre-release versions")
	verbose := fs.Bool("v", false, "show progress and the API changes of every release")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if fs.NArg() != 1 {
		return 0, errors.New("check needs one argument: the module path")
	}
	modulePath := fs.Arg(0)

	a := &history.Analyzer{
		Proxy:       &proxy.Client{BaseURL: *proxyURL},
		Prereleases: *prereleases,
	}
	if *verbose {
		a.Progress = func(path, version string) { fmt.Fprintf(stderr, "loading %s@%s\n", path, version) }
	}
	res, err := a.Analyze(ctx, modulePath)
	if err != nil {
		return 0, err
	}

	retracted, failed := 0, 0
	for _, v := range res.Versions {
		switch {
		case v.Retracted:
			retracted++
		case v.Err != nil:
			failed++
			fmt.Fprintf(stdout, "warning: %s@%s could not be analyzed: %v\n", v.Path, v.Version, v.Err)
		default:
			printTypeErrors(stdout, v)
		}
	}
	fmt.Fprintf(stdout, "%s: %d versions (%d retracted, %d not analyzed), %d steps checked\n",
		modulePath, len(res.Versions), retracted, failed, len(res.Steps))

	if *verbose {
		for _, s := range res.Steps {
			fmt.Fprintf(stdout, "\n%s -> %s (needs %s)\n", s.Old.Version, s.New.Version, s.Report.RequiredLevel())
			for _, c := range s.Report.Changes {
				fmt.Fprintf(stdout, "    %s\n", changeLine(c))
			}
		}
		fmt.Fprintln(stdout)
	}
	vs := res.Violations()
	printViolations(stdout, vs)
	return len(vs), nil
}

// printTypeErrors warns about the packages of v that could not be fully
// type-checked, since the changes reported for them may not be accurate.
func printTypeErrors(w io.Writer, v *history.Version) {
	for _, p := range v.Module.Packages {
		if len(p.Errors) > 0 {
			fmt.Fprintf(w, "warning: %s@%s: package %s has %d type errors, so changes involving it may be inaccurate (first: %v)\n",
				v.Path, v.Version, p.ImportPath, len(p.Errors), p.Errors[0])
		}
	}
}

func changeLine(c diff.Change) string {
	if c.Compatible {
		return "compatible:   " + c.Message
	}
	return "incompatible: " + c.Message
}

func printList(w io.Writer, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintln(w, title)
	for _, it := range items {
		fmt.Fprintf(w, "    %s\n", it)
	}
}

func printViolations(w io.Writer, vs []check.Violation) {
	for _, v := range vs {
		fmt.Fprintln(w, "violation:", v)
		for _, d := range v.Details {
			fmt.Fprintf(w, "    %s\n", d)
		}
	}
	if len(vs) == 0 {
		fmt.Fprintln(w, "no violations found")
	} else {
		fmt.Fprintf(w, "%d violations found\n", len(vs))
	}
}
