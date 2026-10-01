package api

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// maxSourceFile is the largest source file that will be read from a module
// zip archive.
const maxSourceFile = 16 << 20

// sourceFS is a read-only, in-memory view of the files of a module zip
// archive. File names are relative to the module root and use slashes.
type sourceFS struct {
	files  map[string]*zip.File
	dirs   map[string]map[string]bool // directory -> entry name -> is a directory
	nested map[string]bool            // directories holding a nested module
}

// newSourceFS indexes the files of zr, whose names must start with prefix.
func newSourceFS(zr *zip.Reader, prefix string) (*sourceFS, error) {
	s := &sourceFS{
		files:  make(map[string]*zip.File),
		dirs:   make(map[string]map[string]bool),
		nested: make(map[string]bool),
	}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") {
			continue
		}
		rel, ok := strings.CutPrefix(f.Name, prefix)
		if !ok {
			return nil, fmt.Errorf("zip file %q does not start with %q", f.Name, prefix)
		}
		s.files[rel] = f
		dir, name := path.Split(rel)
		dir = strings.TrimSuffix(dir, "/")
		if name == "go.mod" && dir != "" {
			s.nested[dir] = true
		}
		s.addEntry(dir, name, false)
		for dir != "" {
			parent, name := path.Split(dir)
			parent = strings.TrimSuffix(parent, "/")
			s.addEntry(parent, name, true)
			dir = parent
		}
	}
	return s, nil
}

func (s *sourceFS) addEntry(dir, name string, isDir bool) {
	if s.dirs[dir] == nil {
		s.dirs[dir] = make(map[string]bool)
	}
	s.dirs[dir][name] = isDir
}

// readFile returns the contents of the file with the given relative name.
func (s *sourceFS) readFile(rel string) ([]byte, error) {
	f, ok := s.files[rel]
	if !ok {
		return nil, fs.ErrNotExist
	}
	if f.UncompressedSize64 > maxSourceFile {
		return nil, fmt.Errorf("%s is too large (%d bytes)", rel, f.UncompressedSize64)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxSourceFile))
}

// goFiles returns the names of the non-test .go files in dir, sorted.
func (s *sourceFS) goFiles(dir string) []string {
	var names []string
	for name, isDir := range s.dirs[dir] {
		if !isDir && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// inNestedModule reports whether dir is inside a nested module, whose
// packages do not belong to this module.
func (s *sourceFS) inNestedModule(dir string) bool {
	for ; dir != "" && dir != "."; dir = path.Dir(dir) {
		if s.nested[dir] {
			return true
		}
	}
	return false
}

// packageDirs returns, sorted, the directories that may hold packages of
// the module: those with .go files that are not in a nested module nor under
// a testdata or vendor directory, or one whose name starts with "." or "_".
func (s *sourceFS) packageDirs() []string {
	var dirs []string
	for dir := range s.dirs {
		if len(s.goFiles(dir)) == 0 || s.inNestedModule(dir) || ignoredDir(dir) {
			continue
		}
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

func ignoredDir(dir string) bool {
	for _, elem := range strings.Split(dir, "/") {
		if elem == "testdata" || elem == "vendor" || strings.HasPrefix(elem, ".") || strings.HasPrefix(elem, "_") {
			return true
		}
	}
	return false
}
