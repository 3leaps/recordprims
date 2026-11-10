package recordprims

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type clockFunc func() time.Time

func (f clockFunc) Now() time.Time { return f() }

func TestStreamIndexesCreatedAndRebuiltOnWatermarkOrBackrefCorruption(t *testing.T) {
	ctx := context.Background()
	store, root, _ := openBodyStore(t)

	st, err := store.OpenStream("idx-stream", ModeRevisioned)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	rcpt, err := st.Append(ctx, Write{
		Record:         recordJSON("idx-stream", "rec-1", 1, nil, `{"v":"a"}`, nil),
		IdempotencyKey: "k-idx-1",
	})
	if err != nil || rcpt.Outcome != OutcomeAppended {
		t.Fatalf("Append: %+v, err=%v", rcpt, err)
	}

	sdir := filepath.Join(root, streamDir("idx-stream"))
	headsFile := filepath.Join(sdir, "heads.idx")
	keysFile := filepath.Join(sdir, "keys.idx")
	recsFile := filepath.Join(sdir, "records.idx")

	for _, f := range []string{headsFile, keysFile, recsFile} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("expected index file %s to exist: %v", f, err)
		}
	}

	origHeadsBytes, err := os.ReadFile(headsFile)
	if err != nil {
		t.Fatalf("read heads.idx: %v", err)
	}

	// 1. Corrupt watermark (CutSeq mismatch) in heads.idx and verify Head() rebuilds it.
	var doc indexDoc[headIndexEntry]
	if err := json.Unmarshal(origHeadsBytes, &doc); err != nil {
		t.Fatalf("unmarshal heads.idx: %v", err)
	}
	doc.Watermark.CutSeq = 999
	tampered, _ := canonicalJSON(doc)
	if err := os.WriteFile(headsFile, tampered, 0o600); err != nil {
		t.Fatalf("write tampered heads.idx: %v", err)
	}
	os.Chmod(headsFile, 0o600)

	hi, found, err := st.Head("rec-1")
	if err != nil || !found || hi.Pin.Revision != 1 {
		t.Fatalf("Head after watermark corruption: %+v, found=%v, err=%v", hi, found, err)
	}
	rebuiltBytes, err := os.ReadFile(headsFile)
	if err != nil {
		t.Fatalf("read rebuilt heads.idx: %v", err)
	}
	if string(rebuiltBytes) != string(origHeadsBytes) {
		t.Fatalf("rebuilt heads.idx does not match original after watermark corruption")
	}

	// 2. Corrupt back-reference offset in records.idx (while recomputing content_digest)
	// and verify Revisions() detects the back-reference mismatch and rebuilds records.idx.
	origRecsBytes, err := os.ReadFile(recsFile)
	if err != nil {
		t.Fatalf("read records.idx: %v", err)
	}
	var rdoc indexDoc[recordIndexEntry]
	if err := json.Unmarshal(origRecsBytes, &rdoc); err != nil {
		t.Fatalf("unmarshal records.idx: %v", err)
	}
	rdoc.Entries[0].Offset += 5
	eb, _ := canonicalJSON(rdoc.Entries)
	rdoc.ContentDigest = sha256HexPrefixed(eb[:len(eb)-1])
	tamperedRecs, _ := canonicalJSON(rdoc)
	if err := os.WriteFile(recsFile, tamperedRecs, 0o600); err != nil {
		t.Fatalf("write tampered records.idx: %v", err)
	}
	os.Chmod(recsFile, 0o600)

	revs, err := st.Revisions("rec-1")
	if err != nil || len(revs) != 1 {
		t.Fatalf("Revisions after back-reference corruption: %+v, err=%v", revs, err)
	}
	rebuiltRecsBytes, err := os.ReadFile(recsFile)
	if err != nil {
		t.Fatalf("read rebuilt records.idx: %v", err)
	}
	if string(rebuiltRecsBytes) != string(origRecsBytes) {
		t.Fatalf("rebuilt records.idx does not match original after back-reference corruption")
	}
}

func TestPostCommitIndexFailureIsIndeterminate(t *testing.T) {
	s, dir, clk := openBodyStore(t)
	st, err := s.OpenStream("notes", ModeSingleRevision)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Head("r1"); err != nil {
		t.Fatal(err)
	}
	headsPath := filepath.Join(dir, streamDir("notes"), "heads.idx")
	fired := false
	s.opts.Clock = clockFunc(func() time.Time {
		if !fired {
			fired = true
			if err := os.Remove(headsPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(headsPath, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		return clk.t
	})
	_, err = st.Append(ctx, Write{
		Record:         recordJSON("notes", "r1", 1, nil, `{"a":1}`, nil),
		IdempotencyKey: "k1",
	})
	if err == nil {
		t.Fatal("expected post-commit index write fault")
	}
	if !IsIndeterminate(err) {
		t.Fatalf("post-commit index write failure returned determinate error %v, want IsIndeterminate(err)==true", err)
	}
	if err := os.Remove(headsPath); err != nil {
		t.Fatal(err)
	}
	rec, err := st.Reconcile("k1", clk.t)
	if err != nil || rec.Result != "found" {
		t.Fatalf("Reconcile after indeterminate index failure: %+v, err=%v, want found", rec, err)
	}
}
