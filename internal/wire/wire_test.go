package wire

import (
	"errors"
	"strings"
	"testing"
)

func reason(err error) string {
	var r *Reject
	if errors.As(err, &r) {
		return r.Reason
	}
	if err != nil {
		return "other: " + err.Error()
	}
	return ""
}

// The safe-integer controls are the contract reference interpreter's own
// (validate-durable-record-fixtures.py, "Bounded raw-token numeric_domain").
func TestSafeIntAcceptsExactIntegers(t *testing.T) {
	for _, c := range []struct {
		tok  string
		want int64
	}{
		{"0", 0}, {"-0", 0}, {"0.0", 0}, {"-0.0", 0}, {"0e99999999", 0},
		{"12", 12}, {"12.0", 12}, {"1.2e1", 12}, {"1.200e+1", 12}, {"-1", -1},
		{"-9007199254740991", -MaxSafeInteger}, {"9007199254740990", MaxSafeInteger - 1},
		{"9007199254740991", MaxSafeInteger}, {"12000000000000000000000e-21", 12},
		{"1" + strings.Repeat("0", 100) + "e-100", 1},
		{"9007199254740991000000000000000000000e-21", MaxSafeInteger},
		{"0.000000000000000000009007199254740991e36", MaxSafeInteger},
		{"-9007199254740991000000000000000000000e-21", -MaxSafeInteger},
	} {
		got, err := SafeInt(c.tok)
		if err != nil || got != c.want {
			t.Errorf("SafeInt(%q) = %d, %v; want %d", c.tok, got, err, c.want)
		}
	}
}

func TestSafeIntRefusesOutOfDomain(t *testing.T) {
	for _, tok := range []string{
		"0.5", "12.0000000000000001", "1.0000000000000001", "1200000000000000000000e-21",
		"12000000000000000000001e-21", "1e-400", "1e-99999999", "9007199254740992",
		"9007199254740992000000000000000000000e-21", "-9007199254740992",
		"9223372036854775807", "18446744073709551615", "1e400", "1e99999999",
		"9007199254740991.5",
		"1e999999999999999999999999999999", // bounded: the exponent is never expanded
	} {
		if _, err := SafeInt(tok); reason(err) != ReasonNumericDomain {
			t.Errorf("SafeInt(%q): %v, want numeric_domain", tok, err)
		}
	}
}

func recordDoc(fields string) []byte {
	return []byte(`{"v":0,` + fields + `}`)
}

// Payload numbers are opaque to numeric_domain; protocol integers are not.
func TestPayloadIsOpaqueToNumericDomain(t *testing.T) {
	raw := []byte(`{"v":0,"stream":"s","record_id":"r1","revision":12.0,` +
		`"payload_contract":"contract: ex/v0",` +
		`"payload":{"length":12.5,"seq":9007199254740992,"nested":{"revision":0.25}},` +
		`"provenance":{"actor_kind":"human","actor":"op"},` +
		`"classification":{"retention":"short","sensitivity":"0-public"}}`)
	n, err := Ingest(raw, KindRecord)
	if err != nil {
		t.Fatal(err)
	}
	if r := n.Get("revision"); !r.IsInt || r.Int != 12 {
		t.Fatalf("revision = %+v, want int 12", r)
	}
	if p := n.Get("payload").Get("seq"); p.IsInt {
		t.Fatal("payload number marked as a protocol integer")
	}
}

// Non-numeric types and safe negative integers pass numeric_domain, so the
// shape stage decides them.
func TestNonNumbersPassNumericDomain(t *testing.T) {
	for _, v := range []string{"-1", "true", "null", `"12"`} {
		if _, err := Ingest(recordDoc(`"revision":`+v), KindRecord); err != nil {
			t.Errorf("revision %s: %v, want pass", v, err)
		}
	}
}

// Malformed JSON, duplicate members, non-JSON constants, payload numbers
// outside binary64 and combined stage 1 and stage 2 defects in either member
// order all fail at ingest (the reference interpreter's controls).
func TestIngestRefusals(t *testing.T) {
	bad := []string{
		"{",
		`{"v":0,"v":0}`,
		`{"v":NaN}`,
		`{"v":Infinity}`,
		`{"v":0,"payload":{"x":1e400}}`,
		`{"v":0,"payload":{"x":9007199254740993}}`,
		`{"v":0,"payload":{"n":` + strings.Repeat("1", 308) + `}}`,
		`{"v":0,"payload":{"n":` + strings.Repeat("1", 309) + `}}`,
		`{"v":0,"payload":{"n":` + strings.Repeat("9", 309) + `}}`,
		`{"v":0,"payload":{"n":` + strings.Repeat("1", 310) + `}}`,
		`{"v":0,"payload":{"n":` + strings.Repeat("1", 316) + `}}`,
		`{"v":0,"revision":1.5,"payload":{"n":1e400}}`,
		`{"v":0,"payload":{"n":1e400},"revision":1.5}`,
		`{"v":0,"revision":1.5,"payload":{"n":` + strings.Repeat("9", 309) + `}}`,
	}
	for _, raw := range bad {
		if _, err := Ingest([]byte(raw), KindRecord); reason(err) != ReasonIngest {
			t.Errorf("%.60s: %v, want ingest", raw, err)
		}
	}
}

// Parser cases the Go tokenizer must refuse that encoding/json would accept.
func TestStrictParserRefusals(t *testing.T) {
	for name, raw := range map[string]string{
		"duplicate after unescape": `{"a":1,"a":2}`,
		"lone high surrogate":      `{"a":"\ud83d"}`,
		"lone low surrogate":       `{"a":"\ude00"}`,
		"high then non-low":        `{"a":"\ud83dA"}`,
		"high then high":           `{"a":"\ud83d\ud83d"}`,
		"byte order mark":          "\ufeff{}",
		"raw control character":    "{\"a\":\"x\ty\"}",
		"invalid utf-8":            "{\"a\":\"\xff\"}",
		"encoded surrogate":        "{\"a\":\"\xed\xa0\x80\"}",
		"trailing comma":           `{"a":1,}`,
		"trailing data":            `{} {}`,
		"leading zero":             `{"a":01}`,
		"bare minus":               `{"a":-}`,
		"empty fraction":           `{"a":1.}`,
		"empty exponent":           `{"a":1e}`,
		"plus sign":                `{"a":+1}`,
		"single quotes":            `{'a':1}`,
		"bad escape":               `{"a":"\x"}`,
		"short unicode escape":     `{"a":"\u12"}`,
		"signed unicode escape":    `{"a":"\u+123"}`,
		"empty input":              ``,
	} {
		if _, err := ParseJSON([]byte(raw)); reason(err) != ReasonIngest {
			t.Errorf("%s: %v, want ingest", name, err)
		}
	}
}

// Nesting past the local bound is a local limit, not malformed input.
func TestDepthLimitIsLocal(t *testing.T) {
	deep := strings.Repeat("[", maxDepth+2) + strings.Repeat("]", maxDepth+2)
	if _, err := ParseJSON([]byte(deep)); reason(err) != ReasonLocalLimit {
		t.Fatalf("%v, want local_limit", err)
	}
	ok := strings.Repeat("[", maxDepth) + strings.Repeat("]", maxDepth)
	if _, err := ParseJSON([]byte(ok)); err != nil {
		t.Fatalf("depth %d refused: %v", maxDepth, err)
	}
}

// Every document kind runs both stages: a non-integral protocol integer is
// numeric_domain, and a non-finite opaque number is ingest, even when both
// defects are present.
func TestEveryKindRunsBothStages(t *testing.T) {
	for kind, intField := range map[string]string{
		KindJournalEntry:      `"seq":1.5`,
		KindRecord:            `"revision":1.5`,
		KindProtectionRequest: `"restores":1.5`,
		KindProtectionReceipt: `"seq":1.5`,
		KindWriteReceipt:      `"seq":1.5`,
		KindExportRequest:     `"current_cut":1.5`,
		KindExportResult:      `"protection_cut":1.5`,
		KindBodyStateRequest:  `"body":{"length":1.5}`,
		KindStratumProfile:    `"runtime":{"idempotency_window_ms":1.5}`,
	} {
		if _, err := Ingest([]byte(`{`+intField+`}`), kind); reason(err) != ReasonNumericDomain {
			t.Errorf("%s: %v, want numeric_domain", kind, err)
		}
		both := `{` + intField + `,"zz_opaque":1e400}`
		if _, err := Ingest([]byte(both), kind); reason(err) != ReasonIngest {
			t.Errorf("%s: %v, want ingest before numeric_domain", kind, err)
		}
	}
}

func TestStrictParserAccepts(t *testing.T) {
	n, err := ParseJSON([]byte(" {\"a\":\"\\ud83d\\ude00\\u00e9\\/\",\"b\":[1,-0.5e-3,true,false,null],\"c\":{}} \n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := n.Get("a").Str; got != "\U0001F600\u00e9/" {
		t.Fatalf("decoded %q", got)
	}
	if got := n.Get("b").Elems[1].Token; got != "-0.5e-3" {
		t.Fatalf("number token %q, want the raw spelling", got)
	}
}
