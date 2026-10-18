package loadgen

import (
	"fmt"
	"math"
	"sync"
)

// Ring holds one target's most recent records, up to a fixed capacity
// (DESIGN.md sec 10). Read removes nothing; a record leaves the ring only
// when a later Add overwrites it. It is made with NewRing and is safe for
// concurrent use. Records are never modified after Add: Read's copies
// share their Pair and Evidence with the ring.
type Ring struct {
	mu   sync.Mutex
	buf  []Record // record with Seq s is at buf[s % len(buf)]
	next uint64   // the Seq the next Add assigns
}

// NewRing returns an empty Ring that holds capacity records.
func NewRing(capacity int) (*Ring, error) {
	if capacity < 1 {
		return nil, fmt.Errorf("loadgen: ring capacity %d, want at least 1", capacity)
	}
	return &Ring{buf: make([]Record, capacity)}, nil
}

// Add stores rec with the next Seq (0 for the first record, then one
// more each time), ignoring any Seq rec carries, and returns that Seq.
// When the ring is full it overwrites the oldest record.
func (r *Ring) Add(rec Record) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec.Seq = r.next
	r.buf[r.next%uint64(len(r.buf))] = rec
	r.next++
	return rec.Seq
}

// Next returns the Seq the next Add assigns: one past the newest
// record's, or 0 while the ring is empty.
func (r *Ring) Next() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.next
}

// Read returns copies of the retained records with Seq >= since, in Seq
// order. gap is true when since is older than the oldest retained
// record: records from since on were overwritten before they were read.
// A since past the newest record returns nil, false.
func (r *Ring) Read(since uint64) (recs []Record, gap bool) {
	return r.ReadN(since, math.MaxInt)
}

// ReadN is Read returning at most limit records, the oldest first, so
// the next read starts at the Seq after the last one returned. Its gap
// flag is Read's. A limit below 1 returns no records, with the gap flag.
func (r *Ring) ReadN(since uint64, limit int) (recs []Record, gap bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if since >= r.next {
		return nil, false
	}
	size := uint64(len(r.buf))
	var oldest uint64
	if r.next > size {
		oldest = r.next - size
	}
	if since < oldest {
		gap, since = true, oldest
	}
	if limit < 1 {
		return nil, gap
	}
	end := r.next
	if uint64(limit) < end-since {
		end = since + uint64(limit)
	}
	recs = make([]Record, 0, end-since)
	for s := since; s < end; s++ {
		recs = append(recs, r.buf[s%size])
	}
	return recs, gap
}
