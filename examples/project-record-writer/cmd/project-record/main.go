// Command project-record is a thin CLI over the projectwriter package:
// record a project decision or update, and read it back.
//
//	project-record --project <file> [--as <writer>] <command> [args]
//
// Commands:
//
//	init                                 create the project's store
//	kinds                                list enabled kinds
//	inspect <kind>                       describe a kind, with its example
//	validate <request.json|->            check a request without writing
//	record <request.json|->              write a request
//	reconcile <kind> <key> <attempted>   look up an operation key after a lost reply
//	resolve <pin>                        the served revision at a pin (JSON, or a file with a pin or result)
//	history <kind> <record_id>           every revision of a record
//	render <pin>                         the Markdown page for a pin
//	hold <pin> <key> <ref> <digest> <observed_at>
//	                                     place an operator hold on a revision
//	reindex                              rebuild the search view
//	search <text>                        search the view (only revisions served now)
//
// Output is JSON on stdout (Markdown for render). Exit status: 0 success,
// 3 a refused, conflicting or indeterminate outcome, 2 usage, 1 error.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/3leaps/recordprims"
	pw "github.com/3leaps/recordprims/examples/project-record-writer/projectwriter"
)

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("project-record", flag.ContinueOnError)
	fs.SetOutput(stderr)
	project := fs.String("project", "", "project configuration file")
	caller := fs.String("as", "", "admitted writer name (from the project configuration; not authenticated)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if *project == "" || len(rest) == 0 {
		fmt.Fprintln(stderr, "usage: project-record --project <file> [--as <writer>] <command> [args]")
		return 2
	}
	opts := pw.Options{
		Caller:       *caller,
		LockTimeout:  10 * time.Second,
		AllowTmpfs:   os.Getenv("PROJECT_RECORD_ALLOW_TMPFS") == "1",
		AllowOverlay: os.Getenv("PROJECT_RECORD_ALLOW_OVERLAY") == "1",
	}
	cmd, cargs := rest[0], rest[1:]
	if cmd == "init" {
		if err := pw.Init(*project, opts); err != nil {
			fmt.Fprintln(stderr, "project-record:", err)
			return 1
		}
		return emit(stdout, map[string]string{"initialized": *project})
	}
	w, err := pw.Open(*project, opts)
	if err != nil {
		fmt.Fprintln(stderr, "project-record:", err)
		return 1
	}
	defer w.Close()
	code, err := dispatch(context.Background(), w, cmd, cargs, stdin, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "project-record:", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

func need(args []string, n int, usage string) error {
	if len(args) != n {
		return fmt.Errorf("usage: %s", usage)
	}
	return nil
}

func dispatch(ctx context.Context, w *pw.Writer, cmd string, args []string, stdin io.Reader, stdout io.Writer) (int, error) {
	switch cmd {
	case "kinds":
		return emit(stdout, w.Kinds()), nil
	case "inspect":
		if err := need(args, 1, "inspect <kind>"); err != nil {
			return 2, err
		}
		info, ok := w.Inspect(args[0])
		if !ok {
			return 3, emitErr(stdout, pw.Result{Outcome: pw.OutcomeRefused, Reason: pw.ReasonUnknownKind, Kind: args[0]})
		}
		return emit(stdout, info), nil
	case "validate", "record":
		if err := need(args, 1, cmd+" <request.json|->"); err != nil {
			return 2, err
		}
		req, err := readRequest(args[0], stdin)
		if err != nil {
			return 2, err
		}
		var res pw.Result
		if cmd == "validate" {
			res = w.Validate(req)
		} else if res, err = w.Record(ctx, req); err != nil {
			return 1, err
		}
		return result(stdout, res), nil
	case "reconcile":
		if err := need(args, 3, "reconcile <kind> <operation_key> <attempted_at>"); err != nil {
			return 2, err
		}
		at, err := recordprims.NormalizeProducerInstant(args[2])
		if err != nil {
			return 2, err
		}
		t, err := recordprims.ParseInstant(at)
		if err != nil {
			return 2, err
		}
		res, err := w.Reconcile(args[0], args[1], t)
		if err != nil {
			return 1, err
		}
		return result(stdout, res), nil
	case "resolve", "render":
		if err := need(args, 1, cmd+" <pin-json>"); err != nil {
			return 2, err
		}
		pin, err := readPin(args[0])
		if err != nil {
			return 2, err
		}
		if cmd == "resolve" {
			e, err := w.Lookup(ctx, pin)
			if err != nil {
				return readFailure(stdout, err)
			}
			return emit(stdout, e), nil
		}
		page, err := w.Render(ctx, pin)
		if err != nil {
			return readFailure(stdout, err)
		}
		_, err = io.WriteString(stdout, page)
		return 0, err
	case "history":
		if err := need(args, 2, "history <kind> <record_id>"); err != nil {
			return 2, err
		}
		h, err := w.History(args[0], args[1])
		if err != nil {
			return 1, err
		}
		return emit(stdout, map[string]any{"revisions": h}), nil
	case "hold":
		if err := need(args, 5, "hold <pin> <operation_key> <evidence_ref> <evidence_digest> <observed_at>"); err != nil {
			return 2, err
		}
		pin, err := readPin(args[0])
		if err != nil {
			return 2, err
		}
		rc, err := w.Hold(ctx, pw.HoldRequest{Pin: pin, OperationKey: args[1], EvidenceRef: args[2], EvidenceDigest: args[3], ObservedAt: args[4]})
		if err != nil {
			return 1, err
		}
		emit(stdout, rc)
		if rc.Outcome != "tombstoned" {
			return 3, nil
		}
		return 0, nil
	case "reindex":
		v, err := w.RebuildSearchView(ctx)
		if err != nil {
			return 1, err
		}
		return emit(stdout, map[string]any{"items": len(v.Items), "path": w.Project().SearchViewPath()}), nil
	case "search":
		if err := need(args, 1, "search <text>"); err != nil {
			return 2, err
		}
		hits, err := w.Search(ctx, args[0])
		if err != nil {
			return 1, fmt.Errorf("search: %w (run reindex if the view is missing)", err)
		}
		return emit(stdout, hits), nil
	}
	return 2, fmt.Errorf("unknown command %q", cmd)
}

// readPin takes a pin as JSON, or the path of a file holding a pin or a
// write result (whose "pin" member is used).
func readPin(arg string) (recordprims.Pin, error) {
	raw := []byte(arg)
	if !strings.HasPrefix(strings.TrimSpace(arg), "{") {
		var err error
		if raw, err = os.ReadFile(arg); err != nil {
			return recordprims.Pin{}, err
		}
	}
	var doc struct {
		recordprims.Pin
		Result *recordprims.Pin `json:"pin"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return recordprims.Pin{}, fmt.Errorf("pin: %w", err)
	}
	if doc.Result != nil {
		return *doc.Result, nil
	}
	if doc.Pin.Stream == "" {
		return recordprims.Pin{}, fmt.Errorf("pin: no stream")
	}
	return doc.Pin, nil
}

func readRequest(arg string, stdin io.Reader) (pw.Request, error) {
	var raw []byte
	var err error
	if arg == "-" {
		raw, err = io.ReadAll(stdin)
	} else {
		raw, err = os.ReadFile(arg)
	}
	if err != nil {
		return pw.Request{}, err
	}
	var req pw.Request
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return pw.Request{}, fmt.Errorf("request: %w", err)
	}
	return req, nil
}

func readFailure(stdout io.Writer, err error) (int, error) {
	switch {
	case errors.Is(err, pw.ErrNotServed):
		emit(stdout, map[string]string{"outcome": "not_served", "next": "the revision is held or its record is retired"})
		return 3, nil
	case errors.Is(err, pw.ErrNotFound):
		emit(stdout, map[string]string{"outcome": "not_found"})
		return 3, nil
	}
	return 1, err
}

func result(stdout io.Writer, res pw.Result) int {
	emit(stdout, res)
	if res.Outcome == pw.OutcomeAccepted || res.Outcome == "valid" {
		return 0
	}
	return 3
}

func emitErr(stdout io.Writer, res pw.Result) error {
	emit(stdout, res)
	return nil
}

func emit(stdout io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return 1
	}
	return 0
}
