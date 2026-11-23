package recordprims

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// producerVectors is testdata/producer-instant.json, shared with the Rust tests.
type producerVectors struct {
	Spellings []struct {
		Name      string `json:"name"`
		Input     string `json:"input"`
		Canonical string `json:"canonical"`
		Refused   bool   `json:"refused"`
	} `json:"spellings"`
	Native []struct {
		Name        string `json:"name"`
		UnixSeconds int64  `json:"unix_seconds"`
		Nanos       int64  `json:"nanos"`
		Canonical   string `json:"canonical"`
		Refused     bool   `json:"refused"`
	} `json:"native"`
	RecordDigests []struct {
		Inputs       []string                   `json:"inputs"`
		Canonical    string                     `json:"canonical"`
		Record       map[string]json.RawMessage `json:"record"`
		RecordDigest string                     `json:"record_digest"`
	} `json:"record_digests"`
}

func loadProducerVectors(t *testing.T) producerVectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/producer-instant.json")
	if err != nil {
		t.Fatal(err)
	}
	var v producerVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Spellings) == 0 || len(v.Native) == 0 || len(v.RecordDigests) == 0 {
		t.Fatal("producer-instant vectors are empty")
	}
	return v
}

func TestProducerInstantVectors(t *testing.T) {
	v := loadProducerVectors(t)
	for _, c := range v.Spellings {
		got, err := NormalizeProducerInstant(c.Input)
		if c.Refused {
			if err == nil || !errors.Is(err, ErrSchema) || got != "" {
				t.Errorf("%s: %q = %q, %v; want an ErrSchema refusal", c.Name, c.Input, got, err)
			}
			continue
		}
		if err != nil || got != c.Canonical {
			t.Errorf("%s: %q = %q, %v; want %q", c.Name, c.Input, got, err, c.Canonical)
			continue
		}
		// The output is canonical: the strict parser accepts it and the
		// helper returns it unchanged.
		if _, err := ParseInstant(got); err != nil {
			t.Errorf("%s: output %q is not canonical: %v", c.Name, got, err)
		}
		if again, err := NormalizeProducerInstant(got); err != nil || again != got {
			t.Errorf("%s: normalizing %q again = %q, %v", c.Name, got, again, err)
		}
	}
	for _, c := range v.Native {
		in := time.Unix(c.UnixSeconds, c.Nanos)
		for _, loc := range []*time.Location{time.UTC, time.FixedZone("plus-five", 5*3600)} {
			got, err := ProducerInstant(in.In(loc))
			if c.Refused {
				if err == nil || !errors.Is(err, ErrSchema) || got != "" {
					t.Errorf("%s (%s): = %q, %v; want an ErrSchema refusal", c.Name, loc, got, err)
				}
				continue
			}
			if err != nil || got != c.Canonical {
				t.Errorf("%s (%s): = %q, %v; want %q", c.Name, loc, got, err, c.Canonical)
			}
		}
	}
}

// The canonical spelling never reaches the strict parser in a lenient form:
// every spelling the helper accepts other than the canonical one is refused by
// ParseInstant.
func TestProducerInstantLenientSpellingsStayOffTheWire(t *testing.T) {
	v := loadProducerVectors(t)
	for _, c := range v.Spellings {
		_, err := ParseInstant(c.Input)
		if c.Refused || c.Input != c.Canonical {
			if err == nil {
				t.Errorf("%s: ParseInstant accepted %q", c.Name, c.Input)
			}
		} else if err != nil {
			t.Errorf("%s: ParseInstant refused canonical %q: %v", c.Name, c.Input, err)
		}
	}
}

// A record built from any accepted spelling of the same instant has the same
// digest, and appending it again under the same key is an idempotent replay
// that returns the original receipt.
func TestProducerInstantStableRecordDigest(t *testing.T) {
	v := loadProducerVectors(t)
	for _, c := range v.RecordDigests {
		s, _, clk := openBodyStore(t)
		st, err := s.OpenStream("obs.producer", ModeRevisioned)
		if err != nil {
			t.Fatal(err)
		}
		const key = "k-producer"
		var first Receipt
		for j, input := range c.Inputs {
			at, err := NormalizeProducerInstant(input)
			if err != nil || at != c.Canonical {
				t.Fatalf("%q = %q, %v; want %q", input, at, err, c.Canonical)
			}
			raw := producerRecord(t, c.Record, at)
			clk.t = clk.t.Add(time.Second)
			rc, err := st.Append(ctx, Write{Record: raw, IdempotencyKey: key})
			if err != nil || rc.Pin == nil {
				t.Fatalf("append from %q: %+v, %v", input, rc, err)
			}
			if rc.Pin.Digest != c.RecordDigest {
				t.Fatalf("record digest from %q = %s, want %s", input, rc.Pin.Digest, c.RecordDigest)
			}
			if j == 0 {
				if rc.Outcome != OutcomeAppended {
					t.Fatalf("first append from %q: %+v", input, rc)
				}
				first = rc
				continue
			}
			if rc.Outcome != OutcomeAppended || rc.Seq != first.Seq || *rc.Pin != *first.Pin {
				t.Fatalf("append from %q = %+v, want the original receipt %+v", input, rc, first)
			}
		}
	}
}

// producerRecord renders the vector's record template with observed_at set to at.
func producerRecord(t *testing.T, tmpl map[string]json.RawMessage, at string) []byte {
	t.Helper()
	rec := make(map[string]json.RawMessage, len(tmpl))
	for k, v := range tmpl {
		rec[k] = v
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(tmpl["payload"], &payload); err != nil {
		t.Fatal(err)
	}
	payload["observed_at"], _ = json.Marshal(at)
	rec["payload"], _ = json.Marshal(payload)
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestProducerInstantAnalyticsModel prints the comparison for a
// millisecond-only analytics column. The column is a model (truncation to
// milliseconds), not a measured analytics system. Run with -v to see the table.
func TestProducerInstantAnalyticsModel(t *testing.T) {
	rows := []struct{ input, canonical, column string }{
		{"2026-11-23T12:00:00Z", "2026-11-23T12:00:00.000000Z", "2026-11-23T12:00:00.000Z"},
		{"2026-11-23T12:00:00.123Z", "2026-11-23T12:00:00.123000Z", "2026-11-23T12:00:00.123Z"},
		{"2026-11-23T12:00:00.123456Z", "2026-11-23T12:00:00.123456Z", "2026-11-23T12:00:00.123Z"},
	}
	var b strings.Builder
	b.WriteString("| producer input | canonical bytes | millisecond column (model) | column alone recovers input | recovered from canonical bytes |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, r := range rows {
		canonical, err := NormalizeProducerInstant(r.input)
		if err != nil || canonical != r.canonical {
			t.Fatalf("%q = %q, %v", r.input, canonical, err)
		}
		parsed, err := ParseInstant(canonical)
		if err != nil {
			t.Fatal(err)
		}
		column := parsed.Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z")
		if column != r.column {
			t.Fatalf("column(%q) = %q, want %q", canonical, column, r.column)
		}
		fromColumn, err := NormalizeProducerInstant(column)
		if err != nil {
			t.Fatal(err)
		}
		lost := parsed.Sub(parsed.Truncate(time.Millisecond))
		recovered := FormatInstant(parsed)
		if recovered != canonical {
			t.Fatalf("recovery from canonical bytes = %q, want %q", recovered, canonical)
		}
		verdict := "yes"
		if fromColumn != canonical {
			verdict = fmt.Sprintf("no, loses %d microseconds", lost.Microseconds())
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %s | `%s` |\n", r.input, canonical, column, verdict, recovered)
	}
	if !strings.Contains(b.String(), "no, loses 456 microseconds") {
		t.Fatal("the model must show the microsecond loss")
	}
	t.Log("\n" + b.String())
}
