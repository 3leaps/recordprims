// Command schemacheck validates example payload schemas and their examples
// with the repository's bounded JSON Schema validator.
//
// Usage: schemacheck <dir>...
//
// Each <dir> holds schema.json, at least one valid-*.json that must conform,
// and at least one refused-*.json that must not. A schema keyword outside the
// validator's subset fails the check.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/3leaps/recordprims/internal/shape"
	"github.com/3leaps/recordprims/internal/wire"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: schemacheck <dir>...")
		os.Exit(2)
	}
	total := 0
	for _, dir := range os.Args[1:] {
		n, err := checkDir(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "schemacheck:", err)
			os.Exit(1)
		}
		total += n
	}
	fmt.Printf("[ok] schemacheck: %d schemas, %d examples\n", len(os.Args)-1, total)
}

func checkDir(dir string) (int, error) {
	schemaPath := filepath.Join(dir, "schema.json")
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	valid, refused := 0, 0
	for _, name := range names {
		want := strings.HasPrefix(name, "valid-")
		if !want && !strings.HasPrefix(name, "refused-") {
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		doc, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return 0, err
		}
		inst, err := wire.ParseJSON(doc)
		if err != nil {
			return 0, fmt.Errorf("%s/%s: %v", dir, name, err)
		}
		verr := shape.ValidateDocument(schemaPath, raw, inst)
		if _, compileFailed := verr.(*wire.Reject); verr != nil && !compileFailed {
			return 0, fmt.Errorf("%s: %v", schemaPath, verr)
		}
		switch {
		case want && verr != nil:
			return 0, fmt.Errorf("%s/%s should conform: %v", dir, name, verr)
		case !want && verr == nil:
			return 0, fmt.Errorf("%s/%s should be refused but conforms", dir, name)
		}
		if want {
			valid++
		} else {
			refused++
		}
	}
	if valid == 0 || refused == 0 {
		return 0, fmt.Errorf("%s needs at least one valid-*.json and one refused-*.json", dir)
	}
	return valid + refused, nil
}
