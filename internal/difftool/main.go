// SPDX-License-Identifier: MIT OR Apache-2.0

// Command difftool runs language-neutral JSON-lines conformance and
// differential scenarios against the pure-Go recordprims implementation.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/3leaps/recordprims"
	"github.com/3leaps/recordprims/internal/canon"
	"github.com/3leaps/recordprims/internal/lock"
)

type stepClock struct {
	now time.Time
}

func (c *stepClock) Now() time.Time {
	return c.now
}

type handleEntry struct {
	store   *recordprims.Store
	clock   *stepClock
	streams map[string]*recordprims.Stream
}

type scenarioInput struct {
	ID       string      `json:"id"`
	StoreDir string      `json:"store_dir,omitempty"`
	Steps    []stepInput `json:"steps"`
}

type provInput struct {
	ActorKind  string `json:"actor_kind"`
	Actor      string `json:"actor"`
	Supervisor string `json:"supervisor,omitempty"`
	Model      string `json:"model,omitempty"`
	Tool       string `json:"tool,omitempty"`
}

type authInput struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

type stepInput struct {
	Op                  string          `json:"op"`
	Handle              string          `json:"handle,omitempty"`
	Subdir              string          `json:"subdir,omitempty"`
	At                  string          `json:"at,omitempty"`
	WindowMS            *int64          `json:"window_ms,omitempty"`
	BodyRemovalStream   string          `json:"body_removal_stream,omitempty"`
	PendingBodyTTLMS    *int64          `json:"pending_body_ttl_ms,omitempty"`
	LockTimeoutMS       *int64          `json:"lock_timeout_ms,omitempty"`
	DataUTF8            string          `json:"data_utf8,omitempty"`
	DataHex             string          `json:"data_hex,omitempty"`
	MediaType           string          `json:"media_type,omitempty"`
	Digest              string          `json:"digest,omitempty"`
	Length              int64           `json:"length,omitempty"`
	CompleteIncarnation *uint64         `json:"complete_incarnation,omitempty"`
	Reason              string          `json:"reason,omitempty"`
	Provenance          *provInput      `json:"provenance,omitempty"`
	Authorization       *authInput      `json:"authorization,omitempty"`
	Input               string          `json:"input,omitempty"`
	Rel                 string          `json:"rel,omitempty"`
	Mode                *uint32         `json:"mode,omitempty"`
	Stream              string          `json:"stream,omitempty"`
	StreamMode          string          `json:"stream_mode,omitempty"`
	Record              json.RawMessage `json:"record,omitempty"`
	Key                 string          `json:"key,omitempty"`
	RecordID            string          `json:"record_id,omitempty"`
	AttemptedAt         string          `json:"attempted_at,omitempty"`
	Purpose             string          `json:"purpose,omitempty"`
	Closure             string          `json:"closure,omitempty"`
	Request             json.RawMessage `json:"request,omitempty"`
	RetireWithEmptyKey  bool            `json:"retire_with_empty_key,omitempty"`
}

type stepOutput struct {
	Op                string          `json:"op"`
	OK                bool            `json:"ok"`
	ErrorCode         string          `json:"error_code,omitempty"`
	Reason            string          `json:"reason,omitempty"`
	State             string          `json:"state,omitempty"`
	Detail            string          `json:"detail,omitempty"`
	Indeterminate     *bool           `json:"indeterminate,omitempty"`
	StoreIDLen        int             `json:"store_id_len,omitempty"`
	WindowMS          int64           `json:"window_ms,omitempty"`
	Stratum           string          `json:"stratum,omitempty"`
	FailureModel      string          `json:"failure_model,omitempty"`
	CommitStrategy    string          `json:"commit_strategy,omitempty"`
	BodyRemovalStream string          `json:"body_removal_stream,omitempty"`
	Digest            string          `json:"digest,omitempty"`
	Length            *int64          `json:"length,omitempty"`
	MediaType         string          `json:"media_type,omitempty"`
	LeaseIDLen        int             `json:"lease_id_len,omitempty"`
	LeaseDigest       string          `json:"lease_digest,omitempty"`
	LeaseExpiresAt    string          `json:"lease_expires_at,omitempty"`
	Outcome           string          `json:"outcome,omitempty"`
	Formatted         string          `json:"formatted,omitempty"`
	Data              json.RawMessage `json:"data,omitempty"`
}

type scenarioOutput struct {
	ID      string       `json:"id"`
	Results []stepOutput `json:"results"`
}

func classifyErr(op string, err error) stepOutput {
	ind := recordprims.IsIndeterminate(err)
	out := stepOutput{
		Op:            op,
		OK:            false,
		Indeterminate: &ind,
	}
	var ji *recordprims.ErrJournalInvalid
	var bi *recordprims.ErrBodyIncomplete
	var dt *recordprims.ErrDamagedTail
	var ioe *recordprims.IOError
	switch {
	case errors.Is(err, recordprims.ErrStoreInvalid):
		out.ErrorCode = "store_invalid"
	case errors.Is(err, recordprims.ErrLocked):
		out.ErrorCode = "locked"
	case errors.Is(err, recordprims.ErrSchema):
		out.ErrorCode = "schema"
	case errors.Is(err, recordprims.ErrUnauthorized):
		out.ErrorCode = "unauthorized"
	case errors.As(err, &ji):
		out.ErrorCode = "journal_invalid"
		out.Reason = ji.Reason
	case errors.As(err, &bi):
		out.ErrorCode = "body_incomplete"
		out.State = bi.State
	case errors.Is(err, recordprims.ErrClockRegression):
		out.ErrorCode = "clock_regression"
	case errors.Is(err, recordprims.ErrStreamSealed):
		out.ErrorCode = "stream_sealed"
	case errors.Is(err, recordprims.ErrContinuationUnsupported):
		out.ErrorCode = "continuation_unsupported"
	case errors.Is(err, recordprims.ErrStaleReview):
		out.ErrorCode = "stale_review"
	case errors.Is(err, recordprims.ErrStaleCut):
		out.ErrorCode = "stale_cut"
	case errors.Is(err, recordprims.ErrClosed):
		out.ErrorCode = "closed"
	case errors.As(err, &dt):
		out.ErrorCode = "damaged_tail"
	case errors.Is(err, recordprims.ErrReaderRevoked):
		out.ErrorCode = "reader_revoked"
	case errors.Is(err, recordprims.ErrSinkReentered):
		out.ErrorCode = "sink_reentered"
	case errors.As(err, &ioe):
		out.ErrorCode = "io_error"
	default:
		out.ErrorCode = "other"
	}
	if out.ErrorCode != "io_error" && errors.As(err, &ioe) {
		out.Detail = ioe.Error()
	}
	return out
}

func decodeStepBytes(st stepInput) ([]byte, error) {
	if st.DataHex != "" {
		return hex.DecodeString(st.DataHex)
	}
	return []byte(st.DataUTF8), nil
}

func resolveDir(base, subdir string) string {
	if subdir == "" {
		return base
	}
	return filepath.Join(base, filepath.FromSlash(subdir))
}

func defaultClockTime(at string) (time.Time, error) {
	if at == "" {
		return recordprims.ParseInstant("2026-10-25T12:00:00.000000Z")
	}
	return recordprims.ParseInstant(at)
}

func runScenario(sc scenarioInput) (scenarioOutput, error) {
	storeDir := sc.StoreDir
	cleanup := false
	if storeDir == "" {
		td, err := os.MkdirTemp("", "recordprims-difftool-*")
		if err != nil {
			return scenarioOutput{}, err
		}
		if err := os.Chmod(td, 0o700); err != nil {
			os.RemoveAll(td)
			return scenarioOutput{}, err
		}
		storeDir = td
		cleanup = true
	}
	handles := map[string]*handleEntry{}
	defer func() {
		for _, h := range handles {
			_ = h.store.Close()
		}
		if cleanup {
			_ = os.RemoveAll(storeDir)
		}
	}()

	ctx := context.Background()
	results := make([]stepOutput, 0, len(sc.Steps))

	for _, st := range sc.Steps {
		hname := st.Handle
		if hname == "" {
			hname = "default"
		}
		switch st.Op {
		case "init":
			t0, err := defaultClockTime(st.At)
			if err != nil {
				results = append(results, classifyErr(st.Op, err))
				continue
			}
			clk := &stepClock{now: t0}
			opts := recordprims.InitOptions{
				BodyRemovalStream: st.BodyRemovalStream,
				Clock:             clk,
				AllowTmpfs:        true,
				AllowOverlay:      true,
			}
			if st.WindowMS != nil {
				if *st.WindowMS > math.MaxInt64/int64(time.Millisecond) || *st.WindowMS < 0 {
					opts.IdempotencyWindow = -1
				} else {
					opts.IdempotencyWindow = time.Duration(*st.WindowMS) * time.Millisecond
				}
			}
			targetDir := resolveDir(storeDir, st.Subdir)
			if err := recordprims.Init(targetDir, opts); err != nil {
				results = append(results, classifyErr(st.Op, err))
				continue
			}
			results = append(results, stepOutput{Op: st.Op, OK: true})

		case "open":
			t0, err := defaultClockTime(st.At)
			if err != nil {
				results = append(results, classifyErr(st.Op, err))
				continue
			}
			clk := &stepClock{now: t0}
			opts := recordprims.OpenOptions{
				Clock:        clk,
				AllowTmpfs:   true,
				AllowOverlay: true,
			}
			if st.PendingBodyTTLMS != nil {
				opts.PendingBodyTTL = time.Duration(*st.PendingBodyTTLMS) * time.Millisecond
			}
			if st.LockTimeoutMS != nil {
				opts.LockTimeout = time.Duration(*st.LockTimeoutMS) * time.Millisecond
			}
			targetDir := resolveDir(storeDir, st.Subdir)
			s, err := recordprims.Open(targetDir, opts)
			if err != nil {
				results = append(results, classifyErr(st.Op, err))
				continue
			}
			if old, ok := handles[hname]; ok {
				_ = old.store.Close()
			}
			handles[hname] = &handleEntry{store: s, clock: clk, streams: map[string]*recordprims.Stream{}}
			prof := s.Profile()
			results = append(results, stepOutput{
				Op:                st.Op,
				OK:                true,
				StoreIDLen:        len(s.StoreID()),
				WindowMS:          s.IdempotencyWindow().Milliseconds(),
				Stratum:           prof.Runtime.Stratum,
				FailureModel:      prof.Runtime.FailureModel,
				CommitStrategy:    prof.Runtime.CommitStrategy,
				BodyRemovalStream: prof.Runtime.BodyRemovalStream,
			})

		case "close":
			he, ok := handles[hname]
			if !ok {
				return scenarioOutput{}, fmt.Errorf("unknown handle %q", hname)
			}
			if err := he.store.Close(); err != nil {
				results = append(results, classifyErr(st.Op, err))
				continue
			}
			results = append(results, stepOutput{Op: st.Op, OK: true})

		case "set_clock":
			he, ok := handles[hname]
			if !ok {
				return scenarioOutput{}, fmt.Errorf("unknown handle %q", hname)
			}
			t, err := recordprims.ParseInstant(st.At)
			if err != nil {
				results = append(results, classifyErr(st.Op, err))
				continue
			}
			he.clock.now = t
			results = append(results, stepOutput{Op: st.Op, OK: true})

		case "put_body":
			he, ok := handles[hname]
			if !ok {
				return scenarioOutput{}, fmt.Errorf("unknown handle %q", hname)
			}
			b, err := decodeStepBytes(st)
			if err != nil {
				return scenarioOutput{}, err
			}
			ref, lease, err := he.store.PutBody(ctx, bytes.NewReader(b), st.MediaType)
			if err != nil {
				results = append(results, classifyErr(st.Op, err))
				continue
			}
			length := ref.Length
			results = append(results, stepOutput{
				Op:             st.Op,
				OK:             true,
				Digest:         ref.Digest,
				Length:         &length,
				MediaType:      ref.MediaType,
				LeaseIDLen:     len(lease.ID),
				LeaseDigest:    lease.Digest,
				LeaseExpiresAt: recordprims.FormatInstant(lease.ExpiresAt),
			})

		case "remove_body":
			he, ok := handles[hname]
			if !ok {
				return scenarioOutput{}, fmt.Errorf("unknown handle %q", hname)
			}
			var prov recordprims.Provenance
			if st.Provenance != nil {
				prov = recordprims.Provenance{
					ActorKind:  st.Provenance.ActorKind,
					Actor:      st.Provenance.Actor,
					Supervisor: st.Provenance.Supervisor,
					Model:      st.Provenance.Model,
					Tool:       st.Provenance.Tool,
				}
			}
			var auth *recordprims.Authorization
			if st.Authorization != nil {
				auth = &recordprims.Authorization{
					Ref:    st.Authorization.Ref,
					Digest: st.Authorization.Digest,
				}
			}
			req := recordprims.RemoveBodyRequest{
				Digest:              st.Digest,
				Length:              st.Length,
				CompleteIncarnation: st.CompleteIncarnation,
				Reason:              recordprims.RemoveBodyReason(st.Reason),
				Provenance:          prov,
				Authorization:       auth,
			}
			res, err := he.store.RemoveBody(ctx, req)
			if err != nil {
				results = append(results, classifyErr(st.Op, err))
				continue
			}
			results = append(results, stepOutput{
				Op:      st.Op,
				OK:      true,
				Outcome: string(res.Outcome),
			})

		case "parse_instant":
			ts, err := recordprims.ParseInstant(st.Input)
			if err != nil {
				results = append(results, classifyErr(st.Op, err))
				continue
			}
			results = append(results, stepOutput{
				Op:        st.Op,
				OK:        true,
				Formatted: recordprims.FormatInstant(ts),
			})

		case "fs_write":
			b, err := decodeStepBytes(st)
			if err != nil {
				return scenarioOutput{}, err
			}
			mode := os.FileMode(0o600)
			if st.Mode != nil {
				mode = os.FileMode(*st.Mode)
			}
			p := filepath.Join(storeDir, filepath.FromSlash(st.Rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return scenarioOutput{}, err
			}
			if err := os.WriteFile(p, b, mode); err != nil {
				return scenarioOutput{}, err
			}
			if err := os.Chmod(p, mode); err != nil {
				return scenarioOutput{}, err
			}
			results = append(results, stepOutput{Op: st.Op, OK: true})

		case "fs_remove":
			p := filepath.Join(storeDir, filepath.FromSlash(st.Rel))
			_ = os.RemoveAll(p)
			results = append(results, stepOutput{Op: st.Op, OK: true})

		case "fs_mkdir":
			p := filepath.Join(storeDir, filepath.FromSlash(st.Rel))
			if err := os.MkdirAll(p, 0o700); err != nil {
				return scenarioOutput{}, err
			}
			if err := os.Chmod(p, 0o700); err != nil {
				return scenarioOutput{}, err
			}
			results = append(results, stepOutput{Op: st.Op, OK: true})

		case "hold_lock_and_put":
			he, ok := handles[hname]
			if !ok {
				return scenarioOutput{}, fmt.Errorf("unknown handle %q", hname)
			}
			lockPath := filepath.Join(storeDir, "lock")
			lf, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
			if err != nil {
				return scenarioOutput{}, err
			}
			_ = os.Chmod(lockPath, 0o600)
			rel, err := lock.AcquireFile(lf, 0)
			if err != nil {
				lf.Close()
				return scenarioOutput{}, err
			}
			b, err := decodeStepBytes(st)
			if err != nil {
				_ = rel()
				lf.Close()
				return scenarioOutput{}, err
			}
			_, _, putErr := he.store.PutBody(ctx, bytes.NewReader(b), st.MediaType)
			_ = rel()
			lf.Close()
			if putErr != nil {
				results = append(results, classifyErr(st.Op, putErr))
				continue
			}
			results = append(results, stepOutput{Op: st.Op, OK: true})

		case "probe_cleanup_err":
			cleanErr := fmt.Errorf("recordprims: discarding staged body after %w: %w", recordprims.ErrSchema, &recordprims.IOError{Op: "unlink", Class: "permission"})
			results = append(results, classifyErr(st.Op, cleanErr))

		case "open_stream", "append", "retire", "protect", "head", "revisions", "reconcile", "release":
			he, ok := handles[hname]
			if !ok {
				return scenarioOutput{}, fmt.Errorf("unknown handle %q", hname)
			}
			data, err := runStreamOp(ctx, he, st)
			if err != nil {
				results = append(results, classifyErr(st.Op, err))
				continue
			}
			raw, err := canonData(data)
			if err != nil {
				return scenarioOutput{}, err
			}
			results = append(results, stepOutput{Op: st.Op, OK: true, Data: raw})

		default:
			return scenarioOutput{}, fmt.Errorf("unsupported op %q", st.Op)
		}
	}
	return scenarioOutput{ID: sc.ID, Results: results}, nil
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var sc scenarioInput
		if err := json.Unmarshal(line, &sc); err != nil {
			fmt.Fprintf(os.Stderr, "difftool: unmarshal scenario: %v\n", err)
			os.Exit(1)
		}
		out, err := runScenario(sc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "difftool: scenario %q: %v\n", sc.ID, err)
			os.Exit(1)
		}
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(os.Stderr, "difftool: encode output: %v\n", err)
			os.Exit(1)
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "difftool: read stdin: %v\n", err)
		os.Exit(1)
	}
}

// canonData renders a step's data as RFC 8785 JCS, so both drivers emit the
// same bytes for the same values.
func canonData(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return canon.Canonicalize(bytes.TrimSpace(buf.Bytes()))
}

func pinData(p recordprims.Pin) map[string]any {
	return map[string]any{"stream": p.Stream, "record_id": p.RecordID, "revision": p.Revision, "digest": p.Digest}
}

type collectSink struct {
	frames []map[string]any
}

func (c *collectSink) Frame(_ context.Context, h recordprims.FrameHeader, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	c.frames = append(c.frames, map[string]any{
		"index": h.Index, "kind": string(h.Kind), "length": h.Length, "digest": h.Digest,
		"pin": pinData(h.Pin), "bytes_hex": hex.EncodeToString(b),
	})
	return nil
}

func runStreamOp(ctx context.Context, he *handleEntry, st stepInput) (any, error) {
	if st.Op == "open_stream" {
		s, err := he.store.OpenStream(st.Stream, recordprims.Mode(st.StreamMode))
		if err != nil {
			return nil, err
		}
		he.streams[st.Stream] = s
		return map[string]any{"id": s.ID(), "mode": string(s.Mode())}, nil
	}
	s, ok := he.streams[st.Stream]
	if !ok {
		return nil, fmt.Errorf("%w: stream %q not opened in this scenario", recordprims.ErrSchema, st.Stream)
	}
	switch st.Op {
	case "append":
		r, err := s.Append(ctx, recordprims.Write{Record: st.Record, IdempotencyKey: st.Key})
		if err != nil {
			return nil, err
		}
		return r, nil
	case "retire":
		var opts []recordprims.RetireOption
		if st.Key != "" || st.RetireWithEmptyKey {
			opts = append(opts, recordprims.WithRetireKey(st.Key))
		}
		r, err := s.Retire(ctx, st.RecordID, recordprims.RetireReason(st.Reason), opts...)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"outcome": string(r.Outcome)}
		if r.Seq != 0 {
			out["seq"] = r.Seq
		}
		if r.At != "" {
			out["at"] = r.At
		}
		if len(r.Receipt) > 0 {
			out["receipt"] = r.Receipt
		}
		return out, nil
	case "protect":
		r, err := s.Protect(ctx, recordprims.ProtectionRequest{Raw: st.Request})
		if err != nil {
			return nil, err
		}
		out := map[string]any{"outcome": r.Outcome, "receipt": r.Raw}
		if r.Seq != 0 {
			out["seq"] = r.Seq
		}
		if r.At != "" {
			out["at"] = r.At
		}
		if r.Reason != "" {
			out["reason"] = r.Reason
		}
		return out, nil
	case "head":
		info, found, err := s.Head(st.RecordID)
		if err != nil {
			return nil, err
		}
		if !found {
			return map[string]any{"found": false}, nil
		}
		return map[string]any{"found": true, "pin": pinData(info.Pin), "retired": info.Retired}, nil
	case "revisions":
		revs, err := s.Revisions(st.RecordID)
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, rv := range revs {
			m := map[string]any{
				"pin": pinData(rv.Pin), "payload_contract": rv.PayloadContract,
				"has_body": rv.HasBody, "retired": rv.Retired, "at": rv.At,
			}
			if rv.Supersedes != nil {
				m["supersedes"] = pinData(*rv.Supersedes)
			}
			out = append(out, m)
		}
		return map[string]any{"revisions": out}, nil
	case "reconcile":
		att, err := recordprims.ParseInstant(st.AttemptedAt)
		if err != nil {
			return nil, err
		}
		return s.Reconcile(st.Key, att)
	case "release":
		var sel []recordprims.Pin
		sink := &collectSink{}
		res, err := s.Release(ctx, recordprims.UseRequest{
			Purpose: recordprims.UsePurpose(st.Purpose), Selection: sel, Closure: recordprims.UseClosure(st.Closure),
		}, sink)
		if err != nil {
			return nil, err
		}
		selOut := []map[string]any{}
		for _, p := range res.Selection {
			selOut = append(selOut, pinData(p))
		}
		omOut := []map[string]any{}
		for _, p := range res.Omitted {
			omOut = append(omOut, pinData(p))
		}
		frames := sink.frames
		if frames == nil {
			frames = []map[string]any{}
		}
		out := map[string]any{
			"outcome": string(res.Outcome), "cut": res.Cut, "selection": selOut, "omitted": omOut,
			"frames_completed": res.FramesCompleted, "frames": frames,
		}
		if res.RefusalReason != "" {
			out["refusal_reason"] = res.RefusalReason
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported stream op %q", st.Op)
}
