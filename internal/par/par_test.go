package par

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// peak runs work through run and returns the most calls in flight at once.
func peak(run func(work func())) int {
	var cur, best atomic.Int32
	run(func() {
		c := cur.Add(1)
		for {
			b := best.Load()
			if c <= b || best.CompareAndSwap(b, c) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		cur.Add(-1)
	})
	return int(best.Load())
}

func TestDoRespectsLimit(t *testing.T) {
	defer SetLimit(SetLimit(0))
	for _, l := range []int{1, 2, 3} {
		SetLimit(l)
		want := min(l, Workers())
		got := peak(func(work func()) { Do(40, func(_, _ int) { work() }) })
		if got > want {
			t.Errorf("limit %d: %d calls at once", l, got)
		}
	}
}

func TestDoRunsEveryItemOnce(t *testing.T) {
	var mu sync.Mutex
	seen := map[int]int{}
	Do(1000, func(_, i int) {
		mu.Lock()
		seen[i]++
		mu.Unlock()
	})
	for i := 0; i < 1000; i++ {
		if seen[i] != 1 {
			t.Fatalf("item %d ran %d times", i, seen[i])
		}
	}
}

func TestDoPanicReachesCaller(t *testing.T) {
	for _, l := range []int{1, 4} {
		func() {
			defer SetLimit(SetLimit(l))
			defer func() {
				if r := recover(); r != "boom" {
					t.Errorf("limit %d: recovered %v", l, r)
				}
			}()
			Do(100, func(_, i int) {
				if i == 37 {
					panic("boom")
				}
			})
		}()
	}
}

// grow splits like a recursive tree build.
func grow(s *Sem, depth int, work func()) {
	if depth == 0 {
		work()
		return
	}
	t := s.TryGo(func() { grow(s, depth-1, work) })
	grow(s, depth-1, work)
	t.Wait()
}

func TestSemRespectsLimit(t *testing.T) {
	defer SetLimit(SetLimit(0))
	for _, l := range []int{1, 2, 3} {
		SetLimit(l)
		want := min(l, Workers())
		s := NewSem()
		if got := peak(func(work func()) { grow(s, 6, work) }); got > want {
			t.Errorf("limit %d: %d calls at once", l, got)
		}
	}
}

func TestSemPanicReachesCaller(t *testing.T) {
	defer SetLimit(SetLimit(4))
	defer func() {
		if r := recover(); r != "deep" {
			t.Errorf("recovered %v", r)
		}
	}()
	var n atomic.Int32
	grow(NewSem(), 5, func() {
		if n.Add(1) == 20 {
			panic("deep")
		}
	})
}
