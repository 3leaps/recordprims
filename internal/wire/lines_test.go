package wire

import (
	"bytes"
	"strings"
	"testing"

	"github.com/3leaps/recordprims/internal/canon"
)

func TestIngestLinesFraming(t *testing.T) {
	line := `{"seq":1}`
	for _, c := range []struct {
		name, in string
		want     string // reason, "" for accepted
		n        int
	}{
		{"empty input is an empty journal", "", "", 0},
		{"one line", line + "\n", "", 1},
		{"two lines", line + "\n" + line + "\n", "", 2},
		{"final line missing only its LF", line, ReasonIngest, 0},
		{"final line cut mid-object", line + "\n" + `{"se`, ReasonIngest, 0},
		{"CRLF", line + "\r\n", ReasonIngest, 0},
		{"CR inside a line", `{"seq":1,` + "\r" + `"a":2}` + "\n", ReasonIngest, 0},
		{"CR as the last byte", line + "\n\r", ReasonIngest, 0},
		{"empty line", "\n", ReasonIngest, 0},
		{"blank line between entries", line + "\n\n" + line + "\n", ReasonIngest, 0},
		{"whitespace-only line", line + "\n \t\n", ReasonIngest, 0},
		{"trailing blank line", line + "\n\n", ReasonIngest, 0},
		{"malformed JSON line", line + "\n{\n", ReasonIngest, 0},
		{"numeric domain on a line", `{"seq":9007199254740992}` + "\n", ReasonNumericDomain, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := IngestLines([]byte(c.in), KindJournalEntry)
			if r := reason(err); r != c.want {
				t.Fatalf("reason %q, want %q (%v)", r, c.want, err)
			}
			if err == nil && len(got) != c.n {
				t.Fatalf("%d entries, want %d", len(got), c.n)
			}
		})
	}
}

func TestIngestLinesBoundsLineLength(t *testing.T) {
	long := `{"a":"` + strings.Repeat("x", MaxLineBytes-7) + `"}` + "\n"
	if _, err := IngestLines([]byte(long), KindJournalEntry); reason(err) != ReasonLocalLimit {
		t.Fatalf("%v, want local_limit", err)
	}
	ok := `{"a":"` + strings.Repeat("x", MaxLineBytes-8) + `"}` + "\n"
	if _, err := IngestLines([]byte(ok), KindJournalEntry); err != nil {
		t.Fatalf("a line of exactly %d bytes: %v", len(ok)-1, err)
	}
}

// Encode gives back an equal tree and the same canonical bytes as the text it
// was parsed from.
func TestEncodeRoundTrip(t *testing.T) {
	for _, in := range []string{
		`{}`, `[]`, `null`, `true`, `"x"`,
		`{"b":1,"a":[1,2.5,-0.0,1e3,1E-7,{"c":null}],"s":"q\"b\\s\/"}`,
		`{"ctl":"\u0000\u0001\u001f\b\f\n\r\t","u":"\u00e9\u00a0\u2028\ud83d\ude00","del":"\u007f"}`,
		`{"n":9007199254740991,"m":-123.456e-10,"z":0}`,
		`[[[[{"deep":[{"x":"y"}]}]]]]`,
	} {
		n, err := ParseJSON([]byte(in))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		enc := Encode(n)
		back, err := ParseJSON(enc)
		if err != nil {
			t.Fatalf("%s: Encode gave unparseable %s: %v", in, enc, err)
		}
		if !bytes.Equal(Encode(back), enc) {
			t.Fatalf("%s: not a fixed point: %s vs %s", in, enc, Encode(back))
		}
		want, err1 := canon.Canonicalize([]byte(in))
		got, err2 := canon.Canonicalize(enc)
		if (err1 == nil) != (err2 == nil) || !bytes.Equal(want, got) {
			t.Fatalf("%s: canonical form differs: %s (%v) vs %s (%v)", in, want, err1, got, err2)
		}
	}
}
