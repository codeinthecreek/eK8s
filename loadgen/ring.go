package loadgen

import (
	"fmt"
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

// Read returns copies of the retained records with Seq >= since, in Seq
// order. gap is true when since is older than the oldest retained
// record: records from since on were overwritten before they were read.
// A since past the newest record returns nil, false.
func (r *Ring) Read(since uint64) (recs []Record, gap bool) {
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
	recs = make([]Record, 0, r.next-since)
	for s := since; s < r.next; s++ {
		recs = append(recs, r.buf[s%size])
	}
	return recs, gap
}
