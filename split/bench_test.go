// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package split

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

func BenchmarkCounterSplitAtomic(b *testing.B) {
	// Benchmark a simple split counter updating using atomics.
	counter := New(func(*uint64) {})

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			atomic.AddUint64(counter.Get().(*uint64), 1)
		}
	})
}

func BenchmarkCounterSplitLocked(b *testing.B) {
	// Benchmark a simple split counter using locking instead of atomics.
	type shard struct {
		sync.Mutex
		val uint64
	}
	counter := New(func(*shard) {})

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s := counter.Get().(*shard)
			s.Lock()
			s.val++
			s.Unlock()
		}
	})
}

func BenchmarkCounterShared(b *testing.B) {
	// Non-sharded version of BenchmarkCounter.
	var counter uint64

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			atomic.AddUint64(&counter, 1)
		}
	})
}

var seqCounter uint64

func BenchmarkCounterSequential(b *testing.B) {
	// Sequential version of BenchmarkCounter without atomics. For
	// fair comparison with the cost of uncontended atomics, this
	// only runs at -test.cpu=1 and uses the RunParallel mechanics
	// so the overheads are the same (pb.Next gets inlined and has
	// no atomic ops in the fast path, so this is pretty small).
	if runtime.GOMAXPROCS(-1) != 1 {
		b.Skip("requires -test.cpu=1")
	}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			seqCounter++
		}
	})
}

func BenchmarkRWMutex(b *testing.B) {
	var m RWMutex

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			m.RLock().RUnlock()
		}
	})
}
