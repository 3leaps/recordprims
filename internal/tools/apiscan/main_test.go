package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

// fakes are type-checked in order; each may import the earlier ones.
var fakes = [][2]string{
	{"github.com/3leaps/fakeprims/x", `package x
type T struct{ N int }
type I interface{ M() }
type ID = string
type Fn = func(int) error
type Ints = []int
type G[P any] = []P
type C interface{ ~int }
func New() T { return T{} }`},
	{module + "/internal/w", `package w
import (
	"github.com/3leaps/fakeprims/x"
	cp "github.com/3leaps/fakeprims/x"
)
type Alias = cp.T
type IDAlias = cp.ID
type Wrap struct{ Inner x.T }
type Hidden struct{ inner x.T }
type Embeds struct{ x.T }
func Use() int { return x.New().N }`},
}

type mapImporter map[string]*types.Package

func (m mapImporter) Import(path string) (*types.Package, error) { return m[path], nil }

func check(t *testing.T, imp mapImporter, fset *token.FileSet, path, src string) *types.Package {
	t.Helper()
	f, err := parser.ParseFile(fset, path+".go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := (&types.Config{Importer: imp}).Check(path, fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return pkg
}

func scanRoot(t *testing.T, body string) []string {
	t.Helper()
	fset := token.NewFileSet()
	imp := mapImporter{}
	for _, f := range fakes {
		imp[f[0]] = check(t, imp, fset, f[0], f[1])
	}
	src := `package recordprims
import (
	cp "github.com/3leaps/fakeprims/x"
	. "github.com/3leaps/fakeprims/x"
	"` + module + `/internal/w"
)
var _ = cp.New
var _ = New
var _ = w.Use
` + body
	return scan(check(t, imp, fset, module, src))
}

func TestLeaks(t *testing.T) {
	for name, body := range map[string]string{
		"aliased import":           `func F() cp.T { return cp.T{} }`,
		"dot import":               `func F(T) {}`,
		"internal alias":           `func F() w.Alias { return w.Alias{} }`,
		"internal wrapper":         `func F() w.Wrap { return w.Wrap{} }`,
		"embedded field":           `func F() w.Embeds { return w.Embeds{} }`,
		"exported field":           `type S struct{ X *cp.T }`,
		"unexported type":          `type s struct{ X []cp.T }; func F() s { return s{} }`,
		"method result":            `type S struct{}; func (S) M() map[string]cp.T { return nil }`,
		"pointer method":           `type S struct{}; func (*S) M(chan cp.I) {}`,
		"interface method":         `type J interface{ M() cp.I }`,
		"type alias":               `type A = cp.T`,
		"defined type":             `type D cp.T`,
		"variable":                 `var V cp.T`,
		"generic constraint":       `func F[P interface{ ~int | cp.T }]() {}`,
		"sibling alias to basic":   `func F() cp.ID { return "" }`,
		"local alias of alias":     `type A = cp.ID`,
		"sibling alias to func":    `func F(cp.Fn) {}`,
		"sibling alias to slice":   `var V cp.Ints`,
		"generic sibling alias":    `func F() cp.G[int] { return nil }`,
		"internal alias of alias":  `func F() w.IDAlias { return "" }`,
		"unused type param":        `type Box[P cp.C] struct{}`,
		"private field param":      `type Box[P cp.C] struct{ v P }`,
		"alias type param":         `type A[P cp.C] = []int`,
		"sibling alias constraint": `type Box[P cp.ID] struct{}`,
		"instantiated generic":     `type Box[P cp.C] struct{}; func F() Box[int] { return Box[int]{} }`,
		"nested constraint":        `type Box[P interface{ ~int; M() cp.T }] struct{}`,
		"func type parameter":      `type H func(cp.T)`,
	} {
		if hits := scanRoot(t, body); len(hits) == 0 {
			t.Errorf("%s: leak not reported", name)
		} else if !strings.Contains(hits[0], "fakeprims") {
			t.Errorf("%s: wrong report %q", name, hits[0])
		}
	}
}

func TestInternalUseIsClean(t *testing.T) {
	for name, body := range map[string]string{
		"unexported func":     `func f() cp.T { return cp.T{} }`,
		"unexported field":    `type S struct{ x cp.T }`,
		"hidden wrapper":      `func F() w.Hidden { return w.Hidden{} }`,
		"unexported method":   `type S struct{}; func (S) m() cp.T { return cp.T{} }`,
		"value through int":   `func F() int { return w.Use() }`,
		"unexported alias":    `type a = cp.ID; func f() a { return "" }`,
		"clean generic type":  `type Box[P any] struct{ v P }; func F() Box[int] { return Box[int]{} }`,
		"clean generic alias": `type A[P comparable] = []P`,
		"private generic":     `type box[P cp.C] struct{ v P }`,
		"basic through alias": `func F() string { var x cp.ID; return x }`,
	} {
		if hits := scanRoot(t, body); len(hits) != 0 {
			t.Errorf("%s: false report %v", name, hits)
		}
	}
}

func TestDecodeListLongRecord(t *testing.T) {
	// One record larger than bufio.Scanner's 64 KiB default token.
	files := make([]string, 4000)
	for i := range files {
		files[i] = fmt.Sprintf("file_with_a_long_name_%05d.go", i)
	}
	big, _ := json.Marshal(listed{ImportPath: module, Dir: "/r", GoFiles: files})
	if len(big) <= 64<<10 {
		t.Fatalf("record is %d bytes, want over 64 KiB", len(big))
	}
	small, _ := json.Marshal(listed{ImportPath: module + "/x", Dir: "/r/x", GoFiles: []string{"x.go"}})
	pkgs, err := decodeList(bytes.NewReader(append(append(big, '\n'), small...)))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 || len(pkgs[0].GoFiles) != 4000 || pkgs[1].ImportPath != module+"/x" {
		t.Fatalf("got %d packages", len(pkgs))
	}
}

func TestDecodeListRefuses(t *testing.T) {
	for name, in := range map[string]string{
		"empty":          ``,
		"truncated":      `{"ImportPath":"a","Dir":"/a","GoFiles":["a.go"]`,
		"garbage":        `{"ImportPath":"a","Dir":"/a"} not json`,
		"no import path": `{"Dir":"/a","GoFiles":["a.go"]}`,
		"no directory":   `{"ImportPath":"a","GoFiles":["a.go"]}`,
		"line format":    "a\t/a\ta.go",
	} {
		if _, err := decodeList(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
