package loadgen

import (
	"sync"
	"testing"
)

// addN adds n records to r, with Index i on the i-th, and checks that
// Add returns Seqs first, first+1, and so on.
func addN(t *testing.T, r *Ring, n int, first uint64) {
	t.Helper()
	for i := range n {
		want := first + uint64(i)
		if got := r.Add(Record{Seq: 999, Index: int(want)}); got != want {
			t.Fatalf("Add = Seq %d, want %d", got, want)
		}
	}
}

// wantRead checks that r.Read(since) returns the records with Seqs from
// to to-1, in order, and the gap flag gap.
func wantRead(t *testing.T, r *Ring, since, from, to uint64, gap bool) {
	t.Helper()
	recs, gotGap := r.Read(since)
	if gotGap != gap {
		t.Errorf("Read(%d) gap = %v, want %v", since, gotGap, gap)
	}
	if uint64(len(recs)) != to-from {
		t.Fatalf("Read(%d) = %d records, want %d (Seq %d to %d)", since, len(recs), to-from, from, to-1)
	}
	for i, rec := range recs {
		want := from + uint64(i)
		if rec.Seq != want || rec.Index != int(want) {
			t.Errorf("Read(%d)[%d] = Seq %d Index %d, want %d", since, i, rec.Seq, rec.Index, want)
		}
	}
}

func TestNewRingCapacity(t *testing.T) {
	for _, capacity := range []int{0, -1} {
		if r, err := NewRing(capacity); err == nil {
			t.Errorf("NewRing(%d) = %v, nil; want an error", capacity, r)
		}
	}
}

// TestRingCapacityOne checks the smallest ring: each Add overwrites the
// one record it holds.
func TestRingCapacityOne(t *testing.T) {
	r, err := NewRing(1)
	if err != nil {
		t.Fatal(err)
	}
	addN(t, r, 1, 0)
	wantRead(t, r, 0, 0, 1, false)
	addN(t, r, 1, 1)
	wantRead(t, r, 0, 1, 2, true)
	wantRead(t, r, 1, 1, 2, false)
}

// TestRingFull checks the edge of a full ring: exactly capacity records
// leave no gap, and one more overwrites Seq 0, so Read(0) has a gap.
func TestRingFull(t *testing.T) {
	const capacity = 5
	r, err := NewRing(capacity)
	if err != nil {
		t.Fatal(err)
	}
	addN(t, r, capacity, 0)
	wantRead(t, r, 0, 0, capacity, false)
	addN(t, r, 1, capacity)
	wantRead(t, r, 0, 1, capacity+1, true)
	wantRead(t, r, 1, 1, capacity+1, false)
}

// TestRingReadMid reads a wrapped ring from Seqs in the middle: from the
// oldest retained record on there is no gap.
func TestRingReadMid(t *testing.T) {
	r, err := NewRing(5)
	if err != nil {
		t.Fatal(err)
	}
	addN(t, r, 8, 0) // retains Seq 3 to 7
	wantRead(t, r, 5, 5, 8, false)
	wantRead(t, r, 3, 3, 8, false)
	wantRead(t, r, 2, 3, 8, true)
	wantRead(t, r, 7, 7, 8, false)
}

func TestRingReadPastNewest(t *testing.T) {
	r, err := NewRing(5)
	if err != nil {
		t.Fatal(err)
	}
	for _, since := range []uint64{0, 1} {
		if recs, gap := r.Read(since); recs != nil || gap {
			t.Errorf("empty Read(%d) = %v, %v; want nil, false", since, recs, gap)
		}
	}
	addN(t, r, 3, 0)
	for _, since := range []uint64{3, 4, 1 << 63} {
		if recs, gap := r.Read(since); recs != nil || gap {
			t.Errorf("Read(%d) after Seq 2 = %v, %v; want nil, false", since, recs, gap)
		}
	}
}

// TestRingReadCopy checks that Read returns a copy: changing it leaves
// the ring's records alone.
func TestRingReadCopy(t *testing.T) {
	r, err := NewRing(5)
	if err != nil {
		t.Fatal(err)
	}
	addN(t, r, 3, 0)
	recs, _ := r.Read(0)
	recs[0].Index = 99
	wantRead(t, r, 0, 0, 3, false)
}

// TestRingConcurrent adds records from many goroutines while a reader
// follows along, at a capacity that holds them all and at one that
// wraps. Every Seq Add returns is unique, Read returns contiguous Seqs
// in order, and the reader sees each record it reads exactly once. Run
// it under -race.
func TestRingConcurrent(t *testing.T) {
	const (
		writers   = 16
		perWriter = 500
		total     = writers * perWriter
	)
	for _, capacity := range []int{total, total / 7} {
		r, err := NewRing(capacity)
		if err != nil {
			t.Fatal(err)
		}
		seqs := make([][]uint64, writers)
		var wg sync.WaitGroup
		for w := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range perWriter {
					seqs[w] = append(seqs[w], r.Add(Record{Target: "t", Index: w*perWriter + i}))
				}
			}()
		}
		done := make(chan struct{})
		readerDone := make(chan struct{})
		var read []Record
		go func() {
			defer close(readerDone)
			var since uint64
			for {
				select {
				case <-done:
					return
				default:
				}
				recs, gap := r.Read(since)
				if gap && capacity == total {
					t.Errorf("Read(%d) gap with room for every record", since)
				}
				for i, rec := range recs {
					if i > 0 && rec.Seq != recs[i-1].Seq+1 {
						t.Errorf("Read(%d): Seq %d follows %d", since, rec.Seq, recs[i-1].Seq)
					}
				}
				if len(recs) > 0 {
					if !gap && recs[0].Seq != since {
						t.Errorf("Read(%d) with no gap starts at Seq %d", since, recs[0].Seq)
					}
					read = append(read, recs...)
					since = recs[len(recs)-1].Seq + 1
				}
			}
		}()
		wg.Wait()
		close(done)
		<-readerDone

		seen := make([]bool, total)
		for _, ss := range seqs {
			for _, s := range ss {
				if s >= total || seen[s] {
					t.Fatalf("capacity %d: Add returned Seq %d twice or out of range", capacity, s)
				}
				seen[s] = true
			}
		}
		// total unique Seqs below total: Add returned 0 to total-1.

		indexes := map[int]bool{}
		for _, rec := range read {
			if indexes[rec.Index] {
				t.Errorf("capacity %d: reader saw Index %d twice", capacity, rec.Index)
			}
			indexes[rec.Index] = true
		}

		recs, gap := r.Read(0)
		if gap != (capacity < total) {
			t.Errorf("capacity %d: final Read(0) gap = %v, want %v", capacity, gap, capacity < total)
		}
		if len(recs) != capacity {
			t.Fatalf("capacity %d: final Read(0) = %d records, want %d", capacity, len(recs), capacity)
		}
		indexes = map[int]bool{}
		for i, rec := range recs {
			if want := uint64(total - capacity + i); rec.Seq != want {
				t.Errorf("capacity %d: final Read(0)[%d] = Seq %d, want %d", capacity, i, rec.Seq, want)
			}
			if indexes[rec.Index] {
				t.Errorf("capacity %d: final Read(0) has Index %d twice", capacity, rec.Index)
			}
			indexes[rec.Index] = true
		}
	}
}
