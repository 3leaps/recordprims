package shape

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/3leaps/recordprims/internal/wire"
)

// TestKeywordVerdictsMatchReference replays testdata/keyword-verdicts.json:
// synthetic schemas that each isolate one admitted keyword or composition,
// with the contract's reference shape validator's verdict per instance
// (regenerate with scripts/gen-shape-keyword-cases.py).
func TestKeywordVerdictsMatchReference(t *testing.T) {
	b, err := os.ReadFile("testdata/keyword-verdicts.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name     string          `json:"name"`
		Schema   json.RawMessage `json:"schema"`
		Instance string          `json:"instance"`
		Valid    bool            `json:"valid"`
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 100 {
		t.Fatalf("%d cases", len(cases))
	}
	for _, c := range cases {
		body := string(c.Schema[1 : len(c.Schema)-1])
		raw := `{"$schema":"` + draft202012 + `"`
		if body != "" {
			raw += "," + body
		}
		raw += "}"
		d, err := compileDoc(c.Name, []byte(raw))
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		n, err := wire.ParseJSON([]byte(c.Instance))
		if err != nil {
			t.Fatalf("%s %s: %v", c.Name, c.Instance, err)
		}
		st := &state{d: d}
		f := st.check(d.root, n)
		if st.limit {
			t.Fatalf("%s: local limit", c.Name)
		}
		if (f == nil) != c.Valid {
			t.Errorf("%s %s: recordprims valid=%v, reference valid=%v", c.Name, c.Instance, f == nil, c.Valid)
		}
	}
}
