// Command apiscan fails when a sibling prim's type is reachable from
// recordprims' exported Go API (repository-classes P7, rule 2).
//
// It type-checks every non-internal package from source and walks the types
// reachable from each exported object: signatures, exported and embedded
// struct fields, interface methods, exported methods, type arguments, and
// the declared constraints of generic types, aliases and functions.
// Alias chains are walked one step at a time, so an alias declared in a
// sibling is reported and an import alias or a re-export through an internal
// package is still seen. Standard library only.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const module = "github.com/3leaps/recordprims"

var sibling = regexp.MustCompile(`^github\.com/3leaps/[a-z0-9-]*prims(/|$)`)

// listed is one package from `go list -json`.
type listed struct {
	ImportPath string
	Dir        string
	GoFiles    []string
}

func main() {
	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "apiscan: go list:", err)
		os.Exit(2)
	}
	pkgs, err := decodeList(bytes.NewReader(out))
	if err != nil {
		fmt.Fprintln(os.Stderr, "apiscan:", err)
		os.Exit(2)
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "source", nil)
	failed := false
	for _, lp := range pkgs {
		if strings.Contains(lp.ImportPath+"/", "/internal/") {
			continue
		}
		var files []*ast.File
		for _, name := range lp.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(lp.Dir, name), nil, 0)
			if err != nil {
				fmt.Fprintln(os.Stderr, "apiscan:", err)
				os.Exit(2)
			}
			files = append(files, f)
		}
		pkg, err := (&types.Config{Importer: imp}).Check(lp.ImportPath, fset, files, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "apiscan:", err)
			os.Exit(2)
		}
		for _, hit := range scan(pkg) {
			fmt.Fprintf(os.Stderr, "api-scan: Go package %s exposes %s\n", lp.ImportPath, hit)
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}

// decodeList reads the concatenated JSON objects `go list -json` prints. It
// has no record size limit, and any decode error, a record without an import
// path or directory, or an empty listing is an error, never a skip.
func decodeList(r io.Reader) ([]listed, error) {
	dec := json.NewDecoder(r)
	var pkgs []listed
	for {
		var lp listed
		err := dec.Decode(&lp)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("go list output: %w", err)
		}
		if lp.ImportPath == "" || lp.Dir == "" {
			return nil, fmt.Errorf("go list output: record without import path or directory")
		}
		pkgs = append(pkgs, lp)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("go list output: no packages")
	}
	return pkgs, nil
}

// scan returns one line per exported object that reaches a sibling type.
func scan(pkg *types.Package) []string {
	var hits []string
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		w := walker{seen: map[types.Type]bool{}}
		w.typ(obj.Type())
		if w.hit != "" {
			hits = append(hits, fmt.Sprintf("sibling type %s via %s", w.hit, name))
		}
	}
	return hits
}

type walker struct {
	seen map[types.Type]bool
	hit  string
}

func (w *walker) typ(t types.Type) {
	if t == nil || w.hit != "" {
		return
	}
	// Walk an alias chain one step at a time: an alias declared in a sibling
	// package is that sibling's API even when it names a basic type.
	for {
		a, ok := t.(*types.Alias)
		if !ok {
			break
		}
		if p := a.Obj().Pkg(); p != nil && isSibling(p.Path()) {
			w.hit = a.String()
			return
		}
		for i := 0; i < a.TypeArgs().Len(); i++ {
			w.typ(a.TypeArgs().At(i))
		}
		w.constraints(a.Origin().TypeParams())
		if w.hit != "" {
			return
		}
		t = a.Rhs()
	}
	if w.seen[t] {
		return
	}
	w.seen[t] = true
	switch t := t.(type) {
	case *types.Named:
		for i := 0; i < t.TypeArgs().Len(); i++ {
			w.typ(t.TypeArgs().At(i))
		}
		path := ""
		if p := t.Obj().Pkg(); p != nil {
			path = p.Path()
		}
		if isSibling(path) {
			w.hit = t.String()
			return
		}
		if path != module && !strings.HasPrefix(path, module+"/") {
			return // standard library or another third party
		}
		// A declared constraint is API even when its parameter is unused or
		// only reaches a private field.
		w.constraints(t.Origin().TypeParams())
		w.typ(t.Underlying())
		for _, recv := range []types.Type{t, types.NewPointer(t)} {
			ms := types.NewMethodSet(recv)
			for i := 0; i < ms.Len(); i++ {
				if ms.At(i).Obj().Exported() {
					w.typ(ms.At(i).Type())
				}
			}
		}
	case *types.Pointer:
		w.typ(t.Elem())
	case *types.Slice:
		w.typ(t.Elem())
	case *types.Array:
		w.typ(t.Elem())
	case *types.Map:
		w.typ(t.Key())
		w.typ(t.Elem())
	case *types.Chan:
		w.typ(t.Elem())
	case *types.Signature:
		w.tuple(t.Params())
		w.tuple(t.Results())
		w.constraints(t.TypeParams())
	case *types.Struct:
		for i := 0; i < t.NumFields(); i++ {
			f := t.Field(i)
			if !f.Exported() && !f.Embedded() {
				continue
			}
			// A struct copied from a sibling (type D sibling.T) keeps the
			// sibling's field objects.
			if w.owned(f, t) {
				return
			}
			w.typ(f.Type())
		}
	case *types.Interface:
		for i := 0; i < t.NumEmbeddeds(); i++ {
			w.typ(t.EmbeddedType(i))
		}
		for i := 0; i < t.NumMethods(); i++ {
			m := t.Method(i)
			if !m.Exported() {
				continue
			}
			if w.owned(m, t) {
				return
			}
			w.typ(m.Type())
		}
	case *types.Union:
		for i := 0; i < t.Len(); i++ {
			w.typ(t.Term(i).Type())
		}
	case *types.TypeParam:
		w.typ(t.Constraint())
	}
}

// owned records a hit when obj, a field or method of t, was declared in a
// sibling package.
func (w *walker) owned(obj types.Object, t types.Type) bool {
	if p := obj.Pkg(); p != nil && isSibling(p.Path()) {
		w.hit = fmt.Sprintf("%s (member %s of %s)", p.Path(), obj.Name(), t)
		return true
	}
	return false
}

func isSibling(path string) bool {
	return sibling.MatchString(path) && path != module && !strings.HasPrefix(path, module+"/")
}

func (w *walker) constraints(l *types.TypeParamList) {
	for i := 0; i < l.Len(); i++ {
		w.typ(l.At(i).Constraint())
	}
}

func (w *walker) tuple(t *types.Tuple) {
	for i := 0; i < t.Len(); i++ {
		w.typ(t.At(i).Type())
	}
}
