package wire

import (
	"bytes"
	"fmt"
)

// MaxLineBytes bounds one NDJSON journal line before it is parsed. The
// contract sets no bound; a longer line is refused as ReasonLocalLimit.
const MaxLineBytes = 16 << 20

// IngestLines is stage 1's NDJSON framing followed by Ingest on every line.
// Every line, the last included, ends with a single LF. A CR anywhere and a
// final line without its LF (an incomplete append, even one missing only the
// LF) are refused as ingest. An empty or whitespace-only line is not a JSON
// document, so Ingest refuses it as ingest too: it is never skipped. Empty
// input is an empty journal.
func IngestLines(data []byte, kind string) ([]*Node, error) {
	if bytes.IndexByte(data, '\r') >= 0 {
		return nil, ingestf("CR byte in NDJSON framing")
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		return nil, ingestf("final NDJSON line has no terminating LF (incomplete append)")
	}
	var out []*Node
	for i, rest := 1, data; len(rest) > 0; i++ {
		end := bytes.IndexByte(rest, '\n')
		line := rest[:end]
		rest = rest[end+1:]
		if len(line) > MaxLineBytes {
			return nil, &Reject{Reason: ReasonLocalLimit, Detail: fmt.Sprintf("NDJSON line %d is longer than %d bytes", i, MaxLineBytes)}
		}
		n, err := Ingest(line, kind)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}
