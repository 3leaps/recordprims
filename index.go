package recordprims

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"slices"
	"strings"

	"github.com/3leaps/recordprims/internal/canon"
)

func (s *Store) writeIndexFileAtomic(sdir, filename string, data []byte) error {
	var id [12]byte
	if _, err := io.ReadFull(rand.Reader, id[:]); err != nil {
		return err
	}
	tmp := sdir + "/.tmp-" + filename + "-" + hex.EncodeToString(id[:])
	if err := writeFileDurable(s.root, tmp, data); err != nil {
		return err
	}
	dst := sdir + "/" + filename
	if err := s.root.Rename(tmp, dst); err != nil {
		_ = s.root.Remove(tmp)
		return ioErr("rename", err, false)
	}
	if err := s.root.SyncDir(sdir); err != nil {
		return ioErr("fsync_dir", err, false)
	}
	return nil
}

func sha256HexPrefixed(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out, err := canon.Canonicalize(raw)
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

type indexWatermark struct {
	CutSeq                  uint64 `json:"cut_seq"`
	CutOffset               int64  `json:"cut_offset"`
	JournalSHA256ThroughCut string `json:"journal_sha256_through_cut"`
	SealGeneration          uint64 `json:"seal_generation"`
}

type headIndexEntry struct {
	RecordID string `json:"record_id"`
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
	Retired  bool   `json:"retired"`
	Seq      uint64 `json:"seq"`
	Offset   int64  `json:"offset"`
}

type keyIndexEntry struct {
	KeyID  string `json:"key_id"`
	Kind   string `json:"kind"`
	Seq    uint64 `json:"seq"`
	Offset int64  `json:"offset"`
	At     string `json:"at"`
}

type recordIndexEntry struct {
	RecordID string `json:"record_id"`
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
	Seq      uint64 `json:"seq"`
	Offset   int64  `json:"offset"`
}

type indexDoc[T any] struct {
	Watermark     indexWatermark `json:"watermark"`
	ContentDigest string         `json:"content_digest"`
	Entries       []T            `json:"entries"`
}

func computeDiagKeyID(diagKey []byte, stream, rawKey string) string {
	mac := hmac.New(sha256.New, diagKey)
	mac.Write([]byte("recordprims/key_id/v1"))
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(stream)))
	mac.Write(lenBuf[:])
	mac.Write([]byte(stream))
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(rawKey)))
	mac.Write(lenBuf[:])
	mac.Write([]byte(rawKey))
	sum := mac.Sum(nil)
	return hex.EncodeToString(sum[:8])
}

func lineOffsetsOf(raw []byte) map[uint64]int64 {
	out := make(map[uint64]int64)
	var off int64
	var seq uint64
	rest := raw
	for len(rest) > 0 {
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			break
		}
		seq++
		out[seq] = off
		off += int64(nl + 1)
		rest = rest[nl+1:]
	}
	return out
}

func computeExpectedIndexes(diagKey []byte, rep *replayedStream, raw []byte) (
	wm indexWatermark,
	heads indexDoc[headIndexEntry],
	keys indexDoc[keyIndexEntry],
	recs indexDoc[recordIndexEntry],
	err error,
) {
	wm = indexWatermark{
		CutSeq:                  uint64(rep.lastSeq),
		CutOffset:               int64(len(raw)),
		JournalSHA256ThroughCut: sha256HexPrefixed(raw),
		SealGeneration:          0,
	}
	offsets := lineOffsetsOf(raw)

	type headState struct {
		rev    uint64
		digest string
		seq    uint64
		offset int64
	}
	latestByRecord := make(map[string]headState)
	var recEntries []recordIndexEntry
	var keyEntries []keyIndexEntry

	for _, e := range rep.st.Journal() {
		sq := uint64(e.Get("seq").Int)
		off := offsets[sq]
		typ := e.Get("type").Str
		atStr := e.Get("at").Str

		switch typ {
		case "appended", "revised":
			r := e.Get("record")
			rid := r.Get("record_id").Str
			rev := uint64(r.Get("revision").Int)
			dig := e.Get("digest").Str
			recEntries = append(recEntries, recordIndexEntry{
				RecordID: rid,
				Revision: rev,
				Digest:   dig,
				Seq:      sq,
				Offset:   off,
			})
			if cur, ok := latestByRecord[rid]; !ok || rev >= cur.rev {
				latestByRecord[rid] = headState{
					rev:    rev,
					digest: dig,
					seq:    sq,
					offset: off,
				}
			}
			rawKey := e.Get("idempotency_key").Str
			keyEntries = append(keyEntries, keyIndexEntry{
				KeyID:  computeDiagKeyID(diagKey, rep.st.Stream(), rawKey),
				Kind:   "content",
				Seq:    sq,
				Offset: off,
				At:     atStr,
			})
		case "tombstoned", "untombstoned":
			req := e.Get("request")
			rawKey := req.Get("idempotency_key").Str
			keyEntries = append(keyEntries, keyIndexEntry{
				KeyID:  computeDiagKeyID(diagKey, rep.st.Stream(), rawKey),
				Kind:   "protection",
				Seq:    sq,
				Offset: off,
				At:     atStr,
			})
		case "retired":
			if kNode := e.Get("idempotency_key"); kNode != nil {
				keyEntries = append(keyEntries, keyIndexEntry{
					KeyID:  computeDiagKeyID(diagKey, rep.st.Stream(), kNode.Str),
					Kind:   "retirement",
					Seq:    sq,
					Offset: off,
					At:     atStr,
				})
			}
		}
	}

	headEntries := make([]headIndexEntry, 0, len(latestByRecord))
	for rid, hs := range latestByRecord {
		headEntries = append(headEntries, headIndexEntry{
			RecordID: rid,
			Revision: hs.rev,
			Digest:   hs.digest,
			Retired:  rep.st.IsRetired(rid),
			Seq:      hs.seq,
			Offset:   hs.offset,
		})
	}
	slices.SortFunc(headEntries, func(a, b headIndexEntry) int {
		return strings.Compare(a.RecordID, b.RecordID)
	})
	slices.SortFunc(recEntries, func(a, b recordIndexEntry) int {
		if a.RecordID != b.RecordID {
			return strings.Compare(a.RecordID, b.RecordID)
		}
		if a.Revision < b.Revision {
			return -1
		}
		if a.Revision > b.Revision {
			return 1
		}
		return 0
	})
	if headEntries == nil {
		headEntries = []headIndexEntry{}
	}
	if keyEntries == nil {
		keyEntries = []keyIndexEntry{}
	}
	if recEntries == nil {
		recEntries = []recordIndexEntry{}
	}

	hBytes, err := canonicalJSON(headEntries)
	if err != nil {
		return wm, heads, keys, recs, err
	}
	kBytes, err := canonicalJSON(keyEntries)
	if err != nil {
		return wm, heads, keys, recs, err
	}
	rBytes, err := canonicalJSON(recEntries)
	if err != nil {
		return wm, heads, keys, recs, err
	}

	heads = indexDoc[headIndexEntry]{
		Watermark:     wm,
		ContentDigest: sha256HexPrefixed(hBytes[:len(hBytes)-1]),
		Entries:       headEntries,
	}
	keys = indexDoc[keyIndexEntry]{
		Watermark:     wm,
		ContentDigest: sha256HexPrefixed(kBytes[:len(kBytes)-1]),
		Entries:       keyEntries,
	}
	recs = indexDoc[recordIndexEntry]{
		Watermark:     wm,
		ContentDigest: sha256HexPrefixed(rBytes[:len(rBytes)-1]),
		Entries:       recEntries,
	}
	return wm, heads, keys, recs, nil
}

func verifyDoc[T any](s *Store, path string, want indexDoc[T], offsets map[uint64]int64, checkBackref func(T) bool) bool {
	raw, err := readFileVerified(s.root, path, s.fso, maxJournalBytes)
	if err != nil {
		return false
	}
	var got indexDoc[T]
	if err := json.Unmarshal(raw, &got); err != nil {
		return false
	}
	if got.Watermark != want.Watermark {
		return false
	}
	entBytes, err := canonicalJSON(got.Entries)
	if err != nil {
		return false
	}
	if sha256HexPrefixed(entBytes[:len(entBytes)-1]) != got.ContentDigest {
		return false
	}
	if got.ContentDigest != want.ContentDigest || len(got.Entries) != len(want.Entries) {
		return false
	}
	for _, entry := range got.Entries {
		if !checkBackref(entry) {
			return false
		}
	}
	_ = offsets
	return true
}

func (s *Store) syncStreamIndexes(rep *replayedStream, raw []byte) error {
	diagKey, err := readFileVerified(s.root, "diag.key", s.fso, keyLen)
	if err != nil {
		return ioErr("read", err, false)
	}
	_, wantHeads, wantKeys, wantRecs, err := computeExpectedIndexes(diagKey, rep, raw)
	if err != nil {
		return err
	}
	offsets := lineOffsetsOf(raw)
	entries := rep.st.Journal()

	headsPath := rep.sdir + "/heads.idx"
	keysPath := rep.sdir + "/keys.idx"
	recsPath := rep.sdir + "/records.idx"

	headsOK := verifyDoc(s, headsPath, wantHeads, offsets, func(e headIndexEntry) bool {
		if e.Seq < 1 || int(e.Seq) > len(entries) || offsets[e.Seq] != e.Offset {
			return false
		}
		je := entries[e.Seq-1]
		rec := je.Get("record")
		return rec != nil && rec.Get("record_id").Str == e.RecordID &&
			uint64(rec.Get("revision").Int) == e.Revision &&
			je.Get("digest").Str == e.Digest
	})
	keysOK := verifyDoc(s, keysPath, wantKeys, offsets, func(e keyIndexEntry) bool {
		if e.Seq < 1 || int(e.Seq) > len(entries) || offsets[e.Seq] != e.Offset {
			return false
		}
		je := entries[e.Seq-1]
		return je.Get("at").Str == e.At
	})
	recsOK := verifyDoc(s, recsPath, wantRecs, offsets, func(e recordIndexEntry) bool {
		if e.Seq < 1 || int(e.Seq) > len(entries) || offsets[e.Seq] != e.Offset {
			return false
		}
		je := entries[e.Seq-1]
		rec := je.Get("record")
		return rec != nil && rec.Get("record_id").Str == e.RecordID &&
			uint64(rec.Get("revision").Int) == e.Revision &&
			je.Get("digest").Str == e.Digest
	})

	if headsOK && keysOK && recsOK {
		return nil
	}

	hOut, err := canonicalJSON(wantHeads)
	if err != nil {
		return err
	}
	if err := s.writeIndexFileAtomic(rep.sdir, "heads.idx", hOut); err != nil {
		return err
	}
	kOut, err := canonicalJSON(wantKeys)
	if err != nil {
		return err
	}
	if err := s.writeIndexFileAtomic(rep.sdir, "keys.idx", kOut); err != nil {
		return err
	}
	rOut, err := canonicalJSON(wantRecs)
	if err != nil {
		return err
	}
	if err := s.writeIndexFileAtomic(rep.sdir, "records.idx", rOut); err != nil {
		return err
	}
	return nil
}
