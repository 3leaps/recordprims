package shape

import (
	"testing"
)

// Every embedded schema compiles under the admitted subset (init panics
// otherwise, so reaching this test proves admission).
func TestAllSchemasAdmitted(t *testing.T) {
	if got := len(Schemas()); got != 13 {
		t.Fatalf("%d schemas compiled, want 13 (v0: 4, v1: 9)", got)
	}
}
