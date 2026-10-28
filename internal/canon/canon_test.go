package canon

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestCanonicalize(t *testing.T) {
	// Member order, whitespace, number and escape spelling all normalise.
	got, err := Canonicalize([]byte(`{ "b": 1.0E1, "a": "A\/", "c": [true, null] }`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a":"A/","b":10,"c":[true,null]}`; string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestDigestHashesCanonicalBytes(t *testing.T) {
	got, err := Digest([]byte(`{"b":1,"a":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(`{"a":"x","b":1}`))
	if want := "sha256:" + hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestRefusesOutsideDomain(t *testing.T) {
	for _, in := range []string{
		`{"a":1,"a":2}`,    // duplicate member
		`{"a":"\ud800"}`,   // lone surrogate
		`9007199254740993`, // integer outside binary64
		`{"a":1`,           // invalid JSON
	} {
		if _, err := Digest([]byte(in)); err == nil {
			t.Errorf("%s: want refusal", in)
		}
		if _, err := Canonicalize([]byte(in)); err == nil {
			t.Errorf("%s: want refusal", in)
		}
	}
}
