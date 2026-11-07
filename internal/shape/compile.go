// Package shape is the durable-record contract's third evaluation stage: it
// validates a document that passed ingest and numeric_domain against the
// pinned JSON Schemas.
//
// It is a bounded validator for exactly the keyword subset the pinned schemas
// use (draft 2020-12 semantics), not a general JSON Schema engine. Every
// schema is compiled when the package loads, including unselected branches
// and $defs. A keyword or construct outside the subset makes the package
// refuse to load, so a contract pin that needs more fails the build instead
// of being validated loosely. The schemas are embedded byte-identical copies
// of the vendored contract (checked by the conformance suite).
package shape

import (
	"embed"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/3leaps/recordprims/internal/wire"
)

//go:embed schemas/v0/*.schema.json schemas/v1/*.schema.json
var schemaFS embed.FS

const draft202012 = "https://json-schema.org/draft/2020-12/schema"

// node is one compiled schema position.
type node struct {
	path string // the schema path, used in diagnostics

	ref     string // "#/$defs/<name>", resolved through the document
	typ     string // "", object, array, string, integer, boolean, null
	consts  []*wire.Node
	hasEnum bool // consts came from enum (any member matches) rather than const
	isConst bool

	required        []string
	properties      map[string]*node
	additionalFalse bool

	pattern              *regexp.Regexp
	dateTime             bool // format: date-time, asserted (see format.go)
	minLength, maxLength int  // -1 when absent
	minimum, maximum     *decimal

	items, contains    *node
	minItems, maxItems int // -1 when absent
	uniqueItems        bool

	allOf, anyOf, oneOf    []*node
	not, ifN, thenN, elseN *node
}

// doc is one compiled schema document.
type doc struct {
	name string // v1/journal-entry.schema.json
	root *node
	defs map[string]*node
}

// Keywords the subset admits, and the metadata recognised by name.
var (
	assertionKeywords = map[string]bool{
		"$ref": true, "type": true, "const": true, "enum": true,
		"required": true, "properties": true, "additionalProperties": true,
		"pattern": true, "minLength": true, "maxLength": true,
		"minimum": true, "maximum": true,
		"items": true, "contains": true, "minItems": true, "maxItems": true, "uniqueItems": true,
		"allOf": true, "anyOf": true, "oneOf": true, "not": true,
		"if": true, "then": true, "else": true,
	}
	metadataKeywords = map[string]bool{
		"$schema": true, "$id": true, "$defs": true, "title": true, "description": true,
		"format": true,
	}
	// refSiblings are the only keywords admitted beside $ref; the root-only
	// $schema, $id and $defs are admitted there too, at the root.
	refSiblings   = map[string]bool{"title": true, "description": true}
	admittedTypes = map[string]bool{
		"object": true, "array": true, "string": true, "integer": true, "boolean": true, "null": true,
	}
)

// compileError is a schema admission failure: a build or pin qualification
// failure, never an instance refusal.
type compileError struct{ msg string }

func (e *compileError) Error() string { return "shape: schema admission: " + e.msg }

type compiler struct {
	name string
	defs map[string]*wire.Node
	err  error
}

func (c *compiler) fail(path, format string, a ...any) {
	if c.err == nil {
		c.err = &compileError{msg: c.name + " " + path + ": " + fmt.Sprintf(format, a...)}
	}
}

func compileDoc(name string, raw []byte) (*doc, error) {
	root, err := wire.ParseJSON(raw)
	if err != nil {
		return nil, &compileError{msg: name + ": " + err.Error()}
	}
	if root.Kind != wire.Object {
		return nil, &compileError{msg: name + ": schema document is not an object"}
	}
	if s := root.Get("$schema"); s == nil || s.Kind != wire.String || s.Str != draft202012 {
		return nil, &compileError{msg: name + ": $schema must be " + draft202012}
	}
	c := &compiler{name: name, defs: map[string]*wire.Node{}}
	if d := root.Get("$defs"); d != nil {
		if d.Kind != wire.Object {
			return nil, &compileError{msg: name + ": $defs is not an object"}
		}
		for _, m := range d.Fields {
			c.defs[m.Name] = m.Value
		}
	}
	out := &doc{name: name, defs: map[string]*node{}}
	// Compile every definition, used or not, then the root.
	names := make([]string, 0, len(c.defs))
	for n := range c.defs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out.defs[n] = c.compile(c.defs[n], "#/$defs/"+n, false)
	}
	out.root = c.compile(root, "#", true)
	if c.err != nil {
		return nil, c.err
	}
	if err := checkRefCycles(out); err != nil {
		return nil, err
	}
	return out, nil
}

func nonNegInt(n *wire.Node) (int, bool) {
	if n == nil || n.Kind != wire.Number {
		return 0, false
	}
	v, err := wire.SafeInt(n.Token)
	if err != nil || v < 0 {
		return 0, false
	}
	return int(v), true
}

func (c *compiler) sub(n *wire.Node, path string) *node {
	if n == nil {
		return nil
	}
	if n.Kind != wire.Object {
		c.fail(path, "schema position is not an object (boolean schemas are outside the subset)")
		return nil
	}
	return c.compile(n, path, false)
}

func (c *compiler) list(n *wire.Node, path string) []*node {
	if n.Kind != wire.Array || len(n.Elems) == 0 {
		c.fail(path, "must be a non-empty array of schemas")
		return nil
	}
	out := make([]*node, len(n.Elems))
	for i, e := range n.Elems {
		out[i] = c.sub(e, fmt.Sprintf("%s/%d", path, i))
	}
	return out
}

// compile admits one schema object. isRoot allows $schema and $defs there.
func (c *compiler) compile(s *wire.Node, path string, isRoot bool) *node {
	nd := &node{path: path, minLength: -1, maxLength: -1, minItems: -1, maxItems: -1}
	for _, m := range s.Fields {
		k, v := m.Name, m.Value
		switch {
		case assertionKeywords[k]:
		case metadataKeywords[k]:
			switch k {
			case "$schema", "$id", "$defs":
				if !isRoot {
					c.fail(path, "%s is admitted only at the document root", k)
				}
			case "format":
				if v.Kind != wire.String || v.Str != "date-time" {
					c.fail(path, "format other than date-time is outside the subset")
				}
				// Asserted, with the reference validator's exact semantics.
				nd.dateTime = true
			default:
				if v.Kind != wire.String {
					c.fail(path, "%s must be a string", k)
				}
			}
			continue
		default:
			c.fail(path, "keyword %q is outside the admitted subset", k)
			continue
		}
		kp := path + "/" + k
		switch k {
		case "$ref":
			if v.Kind != wire.String || !strings.HasPrefix(v.Str, "#/$defs/") {
				c.fail(kp, "only local #/$defs references are admitted")
				continue
			}
			if _, ok := c.defs[strings.TrimPrefix(v.Str, "#/$defs/")]; !ok {
				c.fail(kp, "unresolved reference %q", v.Str)
			}
			nd.ref = v.Str
		case "type":
			if v.Kind != wire.String || !admittedTypes[v.Str] {
				c.fail(kp, "type must be one of object, array, string, integer, boolean, null")
				continue
			}
			nd.typ = v.Str
		case "const":
			nd.consts, nd.isConst = []*wire.Node{v}, true
		case "enum":
			if v.Kind != wire.Array || len(v.Elems) == 0 {
				c.fail(kp, "enum must be a non-empty array")
				continue
			}
			nd.consts, nd.hasEnum = v.Elems, true
		case "required":
			if v.Kind != wire.Array {
				c.fail(kp, "required must be an array of strings")
				continue
			}
			for _, e := range v.Elems {
				if e.Kind != wire.String {
					c.fail(kp, "required must be an array of strings")
					continue
				}
				nd.required = append(nd.required, e.Str)
			}
		case "properties":
			if v.Kind != wire.Object {
				c.fail(kp, "properties must be an object")
				continue
			}
			nd.properties = map[string]*node{}
			for _, pm := range v.Fields {
				nd.properties[pm.Name] = c.sub(pm.Value, kp+"/"+pm.Name)
			}
		case "additionalProperties":
			if v.Kind != wire.Bool {
				c.fail(kp, "only boolean additionalProperties is admitted")
				continue
			}
			nd.additionalFalse = !v.Bool
		case "pattern":
			if v.Kind != wire.String {
				c.fail(kp, "pattern must be a string")
				continue
			}
			if !admittedPatterns[v.Str] {
				c.fail(kp, "pattern %q is not in the evaluated set (see patterns.go)", v.Str)
				continue
			}
			re, err := regexp.Compile(v.Str)
			if err != nil {
				c.fail(kp, "pattern does not compile: %v", err)
				continue
			}
			nd.pattern = re
		case "minLength", "maxLength", "minItems", "maxItems":
			x, ok := nonNegInt(v)
			if !ok {
				c.fail(kp, "%s must be a non-negative integer", k)
				continue
			}
			switch k {
			case "minLength":
				nd.minLength = x
			case "maxLength":
				nd.maxLength = x
			case "minItems":
				nd.minItems = x
			case "maxItems":
				nd.maxItems = x
			}
		case "minimum", "maximum":
			if v.Kind != wire.Number {
				c.fail(kp, "%s must be a number", k)
				continue
			}
			r, ok := parseDecimal(v.Token)
			if !ok {
				c.fail(kp, "%s is not an exact number", k)
				continue
			}
			if k == "minimum" {
				nd.minimum = &r
			} else {
				nd.maximum = &r
			}
		case "uniqueItems":
			if v.Kind != wire.Bool {
				c.fail(kp, "uniqueItems must be a boolean")
				continue
			}
			nd.uniqueItems = v.Bool
		case "items":
			nd.items = c.sub(v, kp)
		case "contains":
			nd.contains = c.sub(v, kp)
		case "allOf":
			nd.allOf = c.list(v, kp)
		case "anyOf":
			nd.anyOf = c.list(v, kp)
		case "oneOf":
			nd.oneOf = c.list(v, kp)
		case "not":
			nd.not = c.sub(v, kp)
		case "if":
			nd.ifN = c.sub(v, kp)
		case "then":
			nd.thenN = c.sub(v, kp)
		case "else":
			nd.elseN = c.sub(v, kp)
		}
	}
	// Draft 2020-12 applies keywords beside a $ref; the reference validator
	// (draft 7 semantics) ignores them. The contract admits only annotations
	// beside a $ref (and the root-only keywords at the root): constraints
	// belong on the $defs target or inside allOf.
	if nd.ref != "" {
		for _, m := range s.Fields {
			if m.Name == "$ref" || refSiblings[m.Name] {
				continue
			}
			if isRoot && (m.Name == "$schema" || m.Name == "$id" || m.Name == "$defs") {
				continue
			}
			c.fail(path, "$ref with sibling %q: only title and description may accompany a $ref", m.Name)
			break
		}
	}
	return nd
}

// checkRefCycles refuses a reference cycle that consumes no instance depth
// (a $ref chain through applicators only), which would never terminate.
func checkRefCycles(d *doc) error {
	var visit func(n *node, stack map[string]bool) error
	visit = func(n *node, stack map[string]bool) error {
		if n == nil {
			return nil
		}
		if n.ref != "" {
			name := strings.TrimPrefix(n.ref, "#/$defs/")
			if stack[name] {
				return &compileError{msg: d.name + " " + n.path + ": reference cycle through " + n.ref}
			}
			stack[name] = true
			if err := visit(d.defs[name], stack); err != nil {
				return err
			}
			delete(stack, name)
		}
		// Applicators that stay at the same instance location.
		for _, group := range [][]*node{n.allOf, n.anyOf, n.oneOf} {
			for _, s := range group {
				if err := visit(s, stack); err != nil {
					return err
				}
			}
		}
		for _, s := range []*node{n.not, n.ifN, n.thenN, n.elseN} {
			if err := visit(s, stack); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(d.root, map[string]bool{}); err != nil {
		return err
	}
	for _, n := range d.defs {
		if err := visit(n, map[string]bool{}); err != nil {
			return err
		}
	}
	return nil
}
