package recordprims

import (
	"fmt"
	"testing"
	"time"
)

// An explicit exact-pin selection applies to every purpose. A selected
// revision that is held is refused with protected_use for every purpose, a
// selected retired revision is refused for serve, and a refusal delivers no
// frame.
func TestReleaseExplicitSelectionAppliesToEveryPurpose(t *testing.T) {
	s, _, clk := openBodyStore(t)
	st, err := s.OpenStream("sel", ModeRevisioned)
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string]Pin{}
	for _, id := range []string{"plain", "other", "retired", "held"} {
		clk.t = clk.t.Add(time.Second)
		rc, err := st.Append(ctx, Write{Record: recordJSON("sel", id, 1, nil, `{"n":1}`, nil), IdempotencyKey: "k-" + id})
		if err != nil || rc.Pin == nil {
			t.Fatalf("append %s: %+v, %v", id, rc, err)
		}
		pins[id] = *rc.Pin
	}
	clk.t = clk.t.Add(time.Second)
	if _, err := st.Retire(ctx, "retired", RetireWithdrawn); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Second)
	held := pins["held"]
	hold := fmt.Sprintf(
		`{"v":1,"stream":"sel","idempotency_key":"k-hold","action":"tombstone","target":{"stream":"sel","record_id":"held","revision":1,"digest":%q},"scope":"revision","reason":"operator_hold","evidence":{"ref":"case-1","digest":%q},"observed_at":%q,"provenance":{"actor_kind":"process","actor":"test"}}`,
		held.Digest, digestOf([]byte("ev")), FormatInstant(clk.t))
	if rc, err := st.Protect(ctx, ProtectionRequest{Raw: []byte(hold)}); err != nil || rc.Outcome != "tombstoned" {
		t.Fatalf("hold: %+v, %v", rc, err)
	}

	for _, purpose := range []UsePurpose{PurposeServe, PurposeExport, PurposeReuse} {
		for _, closure := range []UseClosure{ClosureMetadataOnly, ClosureFullContent} {
			name := string(purpose) + "/" + string(closure)

			// One selected pin: exactly that record frame, the rest omitted.
			sink := &collectSink{}
			res, err := st.Release(ctx, UseRequest{Purpose: purpose, Selection: []Pin{pins["plain"]}, Closure: closure}, sink)
			if err != nil || res.Outcome != ReleaseReleased {
				t.Fatalf("%s plain: %+v, %v", name, res, err)
			}
			if len(sink.headers) != 1 || sink.headers[0].Pin != pins["plain"] || len(res.Selection) != 1 {
				t.Fatalf("%s plain: frames %+v, selection %+v", name, sink.headers, res.Selection)
			}
			if res.OmittedCount != 3 {
				t.Fatalf("%s plain: omitted %d, want 3", name, res.OmittedCount)
			}

			// A selected held revision is refused for every purpose, with
			// nothing delivered, even beside an eligible pin.
			sink = &collectSink{}
			res, err = st.Release(ctx, UseRequest{Purpose: purpose, Selection: []Pin{pins["plain"], held}, Closure: closure}, sink)
			if err != nil || res.Outcome != ReleaseRefused || res.RefusalReason != "protected_use" || len(sink.headers) != 0 {
				t.Fatalf("%s held: %+v, %v, frames %d", name, res, err, len(sink.headers))
			}

			// A selected retired revision is refused for serve only.
			sink = &collectSink{}
			res, err = st.Release(ctx, UseRequest{Purpose: purpose, Selection: []Pin{pins["retired"]}, Closure: closure}, sink)
			if err != nil {
				t.Fatalf("%s retired: %v", name, err)
			}
			if purpose == PurposeServe {
				if res.Outcome != ReleaseRefused || res.RefusalReason != "protected_use" || len(sink.headers) != 0 {
					t.Fatalf("%s retired: %+v, frames %d", name, res, len(sink.headers))
				}
			} else if res.Outcome != ReleaseReleased || len(sink.headers) != 1 {
				t.Fatalf("%s retired: %+v, frames %d", name, res, len(sink.headers))
			}
		}
	}

	// With an empty selection, serve omits the held and the retired revision;
	// export and reuse omit only the held one.
	for _, c := range []struct {
		purpose UsePurpose
		want    int
	}{{PurposeServe, 2}, {PurposeExport, 3}, {PurposeReuse, 3}} {
		sink := &collectSink{}
		res, err := st.Release(ctx, UseRequest{Purpose: c.purpose, Closure: ClosureMetadataOnly}, sink)
		if err != nil || res.Outcome != ReleaseReleased || len(sink.headers) != c.want || res.OmittedCount != 4-c.want {
			t.Fatalf("%s empty selection: %+v, %v, frames %d", c.purpose, res, err, len(sink.headers))
		}
	}
}
