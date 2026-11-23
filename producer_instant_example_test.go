package recordprims_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/3leaps/recordprims"
)

// A producer whose clock reads whole seconds or milliseconds passes what it
// observed; the result is the canonical spelling to put in a record.
func ExampleNormalizeProducerInstant() {
	for _, observed := range []string{
		"2026-11-23T12:00:00Z",        // whole seconds
		"2026-11-23T12:00:00.123Z",    // milliseconds
		"2026-11-23T12:00:00.123456Z", // already canonical
		"2026-11-23T12:00:00+00:00",   // an offset: refused, not repaired
		"2026-11-23T12:00:00.1234Z",   // four digits: refused, not rounded
	} {
		canonical, err := recordprims.NormalizeProducerInstant(observed)
		if err != nil {
			fmt.Printf("%-28s refused\n", observed)
			continue
		}
		fmt.Printf("%-28s %s\n", observed, canonical)
	}
	// Output:
	// 2026-11-23T12:00:00Z         2026-11-23T12:00:00.000000Z
	// 2026-11-23T12:00:00.123Z     2026-11-23T12:00:00.123000Z
	// 2026-11-23T12:00:00.123456Z  2026-11-23T12:00:00.123456Z
	// 2026-11-23T12:00:00+00:00    refused
	// 2026-11-23T12:00:00.1234Z    refused
}

// A native time value is accepted only if it has no sub-microsecond part;
// the helper refuses rather than truncating it.
func ExampleProducerInstant() {
	exact := time.Date(2026, 11, 23, 12, 0, 0, 123456000, time.UTC)
	finer := time.Date(2026, 11, 23, 12, 0, 0, 123456789, time.UTC)

	s, err := recordprims.ProducerInstant(exact)
	fmt.Println(s, err)
	_, err = recordprims.ProducerInstant(finer)
	fmt.Println(err != nil)
	// Output:
	// 2026-11-23T12:00:00.123456Z <nil>
	// true
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// Normalize before building the record, so the record digest does not depend
// on how the producer spelled the instant. Here the same reading arrives once
// in whole seconds and once, on a retry, in milliseconds: the retry under the
// same idempotency key returns the original receipt.
func ExampleNormalizeProducerInstant_record() {
	dir, err := os.MkdirTemp("", "producer-instant-example-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	clk := fixedClock{time.Date(2026, 11, 23, 12, 0, 5, 0, time.UTC)}
	// AllowTmpfs and AllowOverlay admit the temporary directories of CI hosts.
	if err := recordprims.Init(dir, recordprims.InitOptions{Clock: clk, AllowTmpfs: true, AllowOverlay: true}); err != nil {
		panic(err)
	}
	store, err := recordprims.Open(dir, recordprims.OpenOptions{Clock: clk, AllowTmpfs: true, AllowOverlay: true})
	if err != nil {
		panic(err)
	}
	defer store.Close()
	stream, err := store.OpenStream("sensors.temperature", recordprims.ModeSingleRevision)
	if err != nil {
		panic(err)
	}

	write := func(observed string) recordprims.Receipt {
		at, err := recordprims.NormalizeProducerInstant(observed)
		if err != nil {
			panic(err)
		}
		record, _ := json.Marshal(map[string]any{
			"v": 1, "stream": "sensors.temperature", "record_id": "probe-7", "revision": 1,
			"payload_contract": "contract: example-reading/v1",
			"payload":          map[string]any{"celsius": 21.5},
			"produced_at":      map[string]any{"instant": at, "source": "producer_clock"},
			"provenance":       map[string]any{"actor_kind": "process", "actor": "probe-7"},
			"classification":   map[string]any{"retention": "short", "sensitivity": "0-public"},
		})
		rc, err := stream.Append(context.Background(), recordprims.Write{Record: record, IdempotencyKey: "probe-7-reading-1"})
		if err != nil {
			panic(err)
		}
		return rc
	}

	first := write("2026-11-23T12:00:00Z")
	retry := write("2026-11-23T12:00:00.000Z")
	fmt.Println(first.Outcome, retry.Outcome, retry.Seq == first.Seq, *retry.Pin == *first.Pin)
	// Output:
	// appended appended true true
}
