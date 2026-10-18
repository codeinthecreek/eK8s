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

// TestRingNext checks Next before any Add, through a wrap, and that a
// Read does not move it.
func TestRingNext(t *testing.T) {
	r, err := NewRing(3)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Next(); got != 0 {
		t.Errorf("empty Next = %d, want 0", got)
	}
	addN(t, r, 2, 0)
	if got := r.Next(); got != 2 {
		t.Errorf("Next after Seq 1 = %d, want 2", got)
	}
	addN(t, r, 5, 2) // wraps: retains Seq 4 to 6
	r.Read(0)
	if got := r.Next(); got != 7 {
		t.Errorf("Next after Seq 6 = %d, want 7", got)
	}
}

// wantReadN checks that r.ReadN(since, limit) returns the records with
// Seqs from to to-1, in order, and the gap flag gap.
func wantReadN(t *testing.T, r *Ring, since uint64, limit int, from, to uint64, gap bool) {
	t.Helper()
	recs, gotGap := r.ReadN(since, limit)
	if gotGap != gap {
		t.Errorf("ReadN(%d, %d) gap = %v, want %v", since, limit, gotGap, gap)
	}
	if uint64(len(recs)) != to-from {
		t.Fatalf("ReadN(%d, %d) = %d records, want %d (Seq %d to %d)", since, limit, len(recs), to-from, from, to-1)
	}
	for i, rec := range recs {
		if want := from + uint64(i); rec.Seq != want || rec.Index != int(want) {
			t.Errorf("ReadN(%d, %d)[%d] = Seq %d Index %d, want %d", since, limit, i, rec.Seq, rec.Index, want)
		}
	}
}

// TestRingReadN checks the limit at, below and above the records left,
// on a wrapped ring with a gap and without, and limits below 1.
func TestRingReadN(t *testing.T) {
	r, err := NewRing(5)
	if err != nil {
		t.Fatal(err)
	}
	addN(t, r, 8, 0) // retains Seq 3 to 7
	wantReadN(t, r, 3, 1, 3, 4, false)
	wantReadN(t, r, 3, 4, 3, 7, false)
	wantReadN(t, r, 3, 5, 3, 8, false) // exactly the records left
	wantReadN(t, r, 3, 6, 3, 8, false)
	wantReadN(t, r, 6, 1, 6, 7, false)
	wantReadN(t, r, 7, 1, 7, 8, false)
	// A gap: the read starts at the oldest retained record, under the limit.
	wantReadN(t, r, 0, 2, 3, 5, true)
	wantReadN(t, r, 2, 5, 3, 8, true)
	// No records, gap still reported.
	wantReadN(t, r, 3, 0, 0, 0, false)
	wantReadN(t, r, 0, 0, 0, 0, true)
	wantReadN(t, r, 0, -1, 0, 0, true)
	// Past the newest: nil, false whatever the limit.
	for _, limit := range []int{-1, 0, 1, 100} {
		if recs, gap := r.ReadN(8, limit); recs != nil || gap {
			t.Errorf("ReadN(8, %d) after Seq 7 = %v, %v; want nil, false", limit, recs, gap)
		}
	}
}

// TestRingReadNPaging reads a wrapped ring in pages, each starting at the
// Seq after the last page, and gets every retained record once.
func TestRingReadNPaging(t *testing.T) {
	r, err := NewRing(10)
	if err != nil {
		t.Fatal(err)
	}
	addN(t, r, 13, 0) // retains Seq 3 to 12
	var got []uint64
	since, gaps := uint64(0), 0
	for {
		recs, gap := r.ReadN(since, 3)
		if gap {
			gaps++
		}
		if len(recs) == 0 {
			break
		}
		for _, rec := range recs {
			got = append(got, rec.Seq)
		}
		since = recs[len(recs)-1].Seq + 1
	}
	if gaps != 1 {
		t.Errorf("paging from 0 reported %d gaps, want 1 (the first page)", gaps)
	}
	if len(got) != 10 || got[0] != 3 || got[9] != 12 {
		t.Fatalf("paging read Seqs %v, want 3 to 12", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] != got[i-1]+1 {
			t.Errorf("paging read Seq %d after %d", got[i], got[i-1])
		}
	}
}
