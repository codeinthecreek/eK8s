package gate

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/codeinthecreek/eK8s/internal/classifier"
	"github.com/codeinthecreek/eK8s/internal/hashcore"
	"github.com/codeinthecreek/eK8s/internal/hostpairs"
	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

// kindHash is loadgen's KindHash as the wire carries it
// (loadgen/request.go): the only record kind loadgen gives a Pair. It
// fails closed: if loadgen's kind string ever changes, every record with
// a Pair is a "pair on kind" gate fault, never an unverified hash.
const kindHash = "hash"

// maxQuoted is how many bytes of a record string a gate fault's error
// quotes at most.
const maxQuoted = 64

// maxIndices is how many record indices a Detail lists before "...", as
// loadgen keeps at most 8 evidence bodies (maxEvidence, loadgen/run.go).
const maxIndices = 8

// knownFailReasons are the classifier's Failed reasons, the only reasons
// a Detail prints as given; any other prints as "other", so no free text
// from loadgen or the candidate reaches a verdict.
var knownFailReasons = map[classifier.Reason]bool{
	classifier.ReasonTransportError: true, classifier.ReasonOverHangTimeout: true,
	classifier.ReasonBadStatus: true, classifier.ReasonBare503: true,
	classifier.ReasonBadRetryAfter: true, classifier.ReasonMalformedBody: true,
	classifier.ReasonWrongMatch: true, classifier.ReasonNonCanonicalPHC: true,
	classifier.ReasonSaltReused: true, classifier.ReasonVerifyMismatch: true,
	classifier.ReasonVerifyError: true, classifier.ReasonNegativeAccepted: true,
}

func failReasonText(reason string) string {
	if knownFailReasons[classifier.Reason(reason)] {
		return reason
	}
	return "other"
}

// knownBuckets are the classifier's buckets as the wire carries them.
var knownBuckets = map[string]bool{
	classifier.Success.String(): true, classifier.GracefulShed.String(): true,
	classifier.Failed.String(): true,
}

// burstVerdicts adds Stage 5 "responses" and, only if it passes,
// "hash-verify" over recs, one burst's records once they are read
// (DESIGN.md sec 7; docs/decisions.md, the PR #50 burst verdict entry);
// ok reports whether both passed. The caller must first check that recs
// are complete (one record per index, this run and target, no gap):
// burstVerdicts does not. Only the candidate's own responses
// fail it: a record loadgen could not account for is a gate fault, an
// error from checkBurstRecords before any Result is added, so a failed
// "responses" never masks one. ctx ending during hash-verify is the only
// other error, and adds no "hash-verify" Result. Details hold counts,
// record indices and allowlisted reason names only, never Pair,
// Evidence, Class or other record text as given. Verification runs on
// workers goroutines (GOMAXPROCS when workers <= 0) at about 19 MiB
// each, so the caller must not overlap it with a burst or a window
// (internal/hostpairs).
func burstVerdicts(ctx context.Context, r *Report, core *hashcore.Core, recs []wire.Record, workers int) (ok bool, err error) {
	if err = checkBurstRecords(recs); err != nil {
		return false, err
	}
	if !r.add(5, "responses", responsesDetail(recs)) {
		return false, nil
	}
	detail, err := hashVerifyDetail(ctx, core, recs, workers)
	if err != nil {
		return false, err
	}
	return r.add(5, "hash-verify", detail), nil
}

// checkBurstRecords is the one validation pass over every record. No
// records, a Skipped or Fault record, an empty or unknown Bucket, a Pair
// on a record that is not a hash, and a success hash record with no Pair
// (a hash left unverified) are errors: loadgen could not account for
// that send, which says nothing about the candidate.
func checkBurstRecords(recs []wire.Record) error {
	if len(recs) == 0 {
		return errors.New("burst verdicts: no records")
	}
	for i, rec := range recs {
		where := fmt.Sprintf("burst verdicts: record %d (index %d)", i, rec.Index)
		switch {
		case rec.Skipped:
			return errors.New(where + " is skipped")
		case rec.Fault != "":
			return errors.New(where + " has a loadgen fault " + quoteCut(rec.Fault))
		case !knownBuckets[rec.Bucket]:
			return errors.New(where + " has bucket " + quoteCut(rec.Bucket))
		case rec.Pair != nil && rec.Kind != kindHash:
			return errors.New(where + " has a pair on kind " + quoteCut(rec.Kind))
		case rec.Pair == nil && rec.Kind == kindHash && rec.Bucket == classifier.Success.String():
			return errors.New(where + " is a success hash with no pair")
		}
	}
	return nil
}

// quoteCut quotes s for a gate fault's error, keeping at most its first
// maxQuoted bytes and marking a cut with "...".
func quoteCut(s string) string {
	if len(s) > maxQuoted {
		return strconv.Quote(s[:maxQuoted]) + "..."
	}
	return strconv.Quote(s)
}

// responsesDetail is "responses" over records checkBurstRecords passed:
// a record fails iff its Bucket is "failed", whatever its Kind or Reason
// (negative_accepted and a negative's bad_status included); "success"
// and "graceful_shed" pass.
func responsesDetail(recs []wire.Record) string {
	counts := make(map[string]int)
	var failed []int
	for _, rec := range recs {
		if rec.Bucket != classifier.Failed.String() {
			continue
		}
		counts[failReasonText(rec.Reason)]++
		failed = append(failed, rec.Index)
	}
	if len(failed) == 0 {
		return ""
	}
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(counts)) {
		parts = append(parts, fmt.Sprintf("%s %d", name, counts[name]))
	}
	return fmt.Sprintf("burst: %d of %d responses failed (%s); %s",
		len(failed), len(recs), strings.Join(parts, ", "), indexList(failed))
}

// hashVerifyDetail is "hash-verify" over records checkBurstRecords
// passed: every record with a Pair is verified on the host with the
// fixed core, none sampled (DESIGN.md sec 7), its items passed in record order. A hash
// that does not verify (mismatch) and one the core rejects as malformed
// or not at the fixed parameters (rejected) both fail it: either is the
// candidate's own bytes. Its only error is ctx's, if ctx ends first.
func hashVerifyDetail(ctx context.Context, core *hashcore.Core, recs []wire.Record, workers int) (string, error) {
	var items []hostpairs.Item
	var index []int // index[k] is the Index of items[k]'s record
	for _, rec := range recs {
		if rec.Pair != nil {
			items = append(items, hostpairs.Item{Password: rec.Pair.Password, Hash: rec.Pair.Hash})
			index = append(index, rec.Index)
		}
	}
	res, err := hostpairs.Verify(ctx, core, items, workers)
	if err != nil {
		return "", fmt.Errorf("burst verdicts: hash-verify: %w", err)
	}
	var failed []int
	mismatch, rejected := 0, 0
	for k, v := range res {
		if v.OK {
			continue
		}
		if v.Err != nil {
			rejected++
		} else {
			mismatch++
		}
		failed = append(failed, index[k])
	}
	if len(failed) == 0 {
		return "", nil
	}
	var parts []string
	if mismatch > 0 {
		parts = append(parts, fmt.Sprintf("mismatch %d", mismatch))
	}
	if rejected > 0 {
		parts = append(parts, fmt.Sprintf("rejected %d", rejected))
	}
	return fmt.Sprintf("burst: %d of %d hashes failed host verification (%s); %s",
		len(failed), len(items), strings.Join(parts, ", "), indexList(failed)), nil
}

// indexList lists idx in ascending order, the first maxIndices of them
// and then "..." if there are more. It sorts idx in place.
func indexList(idx []int) string {
	slices.Sort(idx)
	var b strings.Builder
	b.WriteString("indices ")
	for i, x := range idx[:min(len(idx), maxIndices)] {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Itoa(x))
	}
	if len(idx) > maxIndices {
		b.WriteString(", ...")
	}
	return b.String()
}
