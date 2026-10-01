// Command listapi prints the exported identifiers of the packages of a Go
// module, downloaded from a module proxy.
//
// Usage:
//
//	listapi module[@version] [package-suffix]
//
// If version is omitted, the latest version known to the proxy is used.
// If package-suffix is given, only the packages whose import path ends with
// it are shown.
package main

import (
	"context"
	"fmt"
	"go/types"
	"os"
	"strings"

	"github.com/cespedes/go-semver/api"
	"github.com/cespedes/go-semver/proxy"
)

func main() {
	if len(os.Args) < 2 || len(os.Args) > 3 {
		fmt.Fprintln(os.Stderr, "usage: listapi module[@version] [package-suffix]")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "listapi:", err)
		os.Exit(1)
	}
}

func run(arg string, filter []string) error {
	ctx := context.Background()
	modPath, version, _ := strings.Cut(arg, "@")
	client := new(proxy.Client)
	if version == "" {
		info, err := client.Latest(ctx, modPath)
		if err != nil {
			return err
		}
		version = info.Version
	}

	l := &api.Loader{Proxy: client}
	m, err := l.Load(ctx, modPath, version)
	if err != nil {
		return err
	}
	fmt.Printf("module %s %s\n", m.Path, m.Version)
	for _, p := range m.Packages {
		if len(filter) > 0 && !strings.HasSuffix(p.ImportPath, filter[0]) {
			continue
		}
		fmt.Printf("\npackage %s\n", p.ImportPath)
		for _, err := range p.Errors {
			fmt.Printf("  warning: %v\n", err)
		}
		printPackage(p.Types)
	}
	return nil
}

func printPackage(pkg *types.Package) {
	qual := types.RelativeTo(pkg)
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		fmt.Println(" ", types.ObjectString(obj, qual))
		tn, ok := obj.(*types.TypeName)
		if !ok || tn.IsAlias() {
			continue
		}
		if st, ok := tn.Type().Underlying().(*types.Struct); ok {
			for f := range st.Fields() {
				if f.Exported() {
					fmt.Printf("    field %s %s\n", f.Name(), types.TypeString(f.Type(), qual))
				}
			}
		}
		for _, t := range []types.Type{tn.Type(), types.NewPointer(tn.Type())} {
			ms := types.NewMethodSet(t)
			for sel := range ms.Methods() {
				if m := sel.Obj(); m.Exported() && isNew(t, sel) {
					fmt.Printf("    %s\n", types.SelectionString(sel, qual))
				}
			}
		}
	}
}

// isNew reports whether sel is a method that is in the method set of *T but
// not of T (or any method, when t is T), so each method is printed once.
func isNew(t types.Type, sel *types.Selection) bool {
	if _, ptr := t.(*types.Pointer); !ptr {
		return true
	}
	elem := t.(*types.Pointer).Elem()
	return types.NewMethodSet(elem).Lookup(sel.Obj().Pkg(), sel.Obj().Name()) == nil
}
