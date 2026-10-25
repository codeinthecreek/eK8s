package loadgen

import (
	"encoding/binary"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codeinthecreek/eK8s/internal/classifier"
)

// These benchmarks measure the memory DESIGN.md sec 10 budgets for: a
// Record held in a Ring, a salt held in a Checker's set and a request
// stalled in its headers. They are
// Benchmark functions so plain go test does not run them. Each builds
// its structure once, however large b.N is, so run them with
//
//	go test -run '^$' -bench . -benchtime=1x ./loadgen/
//
// and read the B/entry metric: heap in use after the structure is built
// minus before, each after runtime.GC().
//
// A map's bytes per key step with its size, so each salt figure is for
// exactly memSalts keys. [16]byte and uint64 keys measure the same, and
// that is the map, not the benchmark: the struct{} value pads each slot
// (16 B for uint64, 17 B for [16]byte), a 1024-slot table is then 17408 B
// or 18432 B, and the allocator rounds both to its 18432 B size class. A
// uint64 key also risks a collision between random salts, about n^2 / 2^65.

// heapAlloc returns HeapAlloc after a collection.
func heapAlloc() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// measure calls build, which returns the structure it built holding n
// entries, and reports its retained heap per entry. Everything build
// allocates and does not return is garbage by the second reading.
func measure(b *testing.B, n int, build func() any) {
	b.Helper()
	before := heapAlloc()
	keep := build()
	after := heapAlloc()
	runtime.KeepAlive(keep)
	b.ReportMetric(float64(after-before)/float64(n), "B/entry")
}

const memRecords = 10_000

// ringOf fills a ring of memRecords with records made by mk(i).
func ringOf(b *testing.B, mk func(i int) Record) *Ring {
	r, err := NewRing(memRecords)
	if err != nil {
		b.Fatal(err)
	}
	for i := range memRecords {
		r.Add(mk(i))
	}
	return r
}

// shape is a Record the way RunWindow makes one, without a pair or
// evidence.
func shape(i int) Record {
	return Record{
		Target: "v1", RunID: "w1", Index: i, Kind: KindHash,
		IntendedWall: time.Now().Round(0).UTC(), IntendedMono: time.Duration(i) * time.Millisecond,
		ActualMono: time.Duration(i) * time.Millisecond, Latency: 10 * time.Millisecond,
		Verdict: Verdict{classifier.Success, classifier.ReasonNone},
	}
}

// BenchmarkRingRecordNoPair: a Record with neither Pair nor Evidence.
func BenchmarkRingRecordNoPair(b *testing.B) {
	measure(b, memRecords, func() any { return ringOf(b, shape) })
}

// BenchmarkRingRecordPair: a Record with a Pair, a password drawn as
// Stream draws them (16 to 1024 bytes) and a 97-byte canonical hash.
func BenchmarkRingRecordPair(b *testing.B) {
	measure(b, memRecords, func() any {
		return ringOf(b, func(i int) Record {
			rec := shape(i)
			rec.Pair = &HashPair{Password: newPassword(), Hash: withSalt(newSalt())}
			return rec
		})
	})
}

// BenchmarkRingRecordEvidence: a Record with Evidence, a request body
// for such a password and a response body of MaxBodyBytes+1 bytes.
func BenchmarkRingRecordEvidence(b *testing.B) {
	measure(b, memRecords, func() any {
		return ringOf(b, func(i int) Record {
			rec := shape(i)
			rec.Evidence = &Evidence{
				RequestBody: hashBody(newPassword()),
				Status:      http.StatusOK,
				Body:        make([]byte, classifier.MaxBodyBytes+1),
			}
			return rec
		})
	})
}

const memSalts = 750_000

// BenchmarkSaltString: a set keyed by the 22-character salt, each key
// its own 22-byte string.
func BenchmarkSaltString(b *testing.B) {
	measure(b, memSalts, func() any {
		m := make(map[string]struct{})
		for range memSalts {
			m[strings.Clone(newSalt())] = struct{}{}
		}
		return m
	})
}

// BenchmarkSaltArray16: a set keyed by the salt's 16 bytes.
func BenchmarkSaltArray16(b *testing.B) {
	measure(b, memSalts, func() any {
		m := make(map[[16]byte]struct{})
		for range memSalts {
			m[[16]byte(randomBytes(saltLen))] = struct{}{}
		}
		return m
	})
}

// BenchmarkSaltUint64: a set keyed by 8 bytes of the salt.
func BenchmarkSaltUint64(b *testing.B) {
	measure(b, memSalts, func() any {
		m := make(map[uint64]struct{})
		for range memSalts {
			m[binary.LittleEndian.Uint64(randomBytes(saltLen))] = struct{}{}
		}
		return m
	})
}

// BenchmarkCheckerSalts: a real Checker after memSalts distinct-salt 200
// /hash responses through Check.
func BenchmarkCheckerSalts(b *testing.B) {
	req := hashRequest(newPassword())
	measure(b, memSalts, func() any {
		c := NewChecker()
		for range memSalts {
			v, _, err := c.Check(req, Response{Status: http.StatusOK, Body: hashJSON(withSalt(newSalt())), Latency: 10 * time.Millisecond})
			if err != nil || v.Reason != classifier.ReasonNone {
				b.Fatalf("Check = %v, %v", v, err)
			}
		}
		return c
	})
}

const memStalled = 200

// BenchmarkStalledRequest: the live heap a Sender holds per request
// stalled mid-header at the cap, in the worst header shape: distinct
// 2-3 byte names with empty values, so each 5-6 byte line is its own
// header map entry. Each of memStalled sends gets just under
// maxResponseHeaderBytes of such lines and then nothing, until the
// server closes. It feeds the in-flight term of the PR #45 memory sum
// (docs/decisions.md), as KiB/request. It leaves out goroutine stacks
// (three per request: send's, the transport's read and write loops) and
// counts the fake server's side of each connection (one net.Conn and a
// 1-byte buffer), so it reads slightly high.
func BenchmarkStalledRequest(b *testing.B) {
	const names = "abcdefghijklmnopqrstuvwxyz0123456789"
	head := []byte("HTTP/1.1 200 OK\r\n")
	for i := 0; ; i++ {
		name := []byte{names[i%36], names[i/36%36]}
		if i >= 36*36 {
			name = append(name, names[i/(36*36)%36])
		}
		line := append(name, ":\r\n"...)
		if len(head)+len(line) > maxResponseHeaderBytes-64 {
			break
		}
		head = append(head, line...)
	}
	srv := newHeaderServer(b, 0, [][]byte{head})
	s := NewSender()
	var read atomic.Int64
	countReads(b, s, &read)
	req := hashRequest(newPassword())

	before := heapAlloc()
	var wg sync.WaitGroup
	for range memStalled {
		wg.Go(func() { s.send(srv.URL, req) })
	}
	// Wait until every send has read its whole head and waits for more.
	want := int64(memStalled * len(head))
	for deadline := time.Now().Add(clientTimeout / 2); read.Load() < want; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			srv.Close()
			wg.Wait()
			b.Fatalf("read %d of %d head bytes within %v", read.Load(), want, clientTimeout/2)
		}
	}
	after := heapAlloc()
	srv.Close()
	wg.Wait()
	b.ReportMetric(float64(after-before)/memStalled/1024, "KiB/request")
}
