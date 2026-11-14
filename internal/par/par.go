// Package par holds the parallelism settings shared by the codec: how
// many goroutines may compute at once, and helpers that respect that
// bound and hand panics back to the caller (a panic on a goroutine of
// its own would end the program, past any recover of the caller).
package par

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
)

var limit atomic.Int32 // 0: GOMAXPROCS

// SetLimit caps Workers at n (n <= 0: no cap) and returns the previous cap.
func SetLimit(n int) int {
	return int(limit.Swap(int32(max(n, 0))))
}

// Workers is the number of goroutines that may compute at once.
func Workers() int {
	n := runtime.GOMAXPROCS(0)
	if l := int(limit.Load()); l > 0 && l < n {
		return l
	}
	return n
}

// Panic carries a panic of a worker goroutine to the caller.
type Panic struct{ Value any }

func (p Panic) Error() string { return fmt.Sprint(p.Value) }

// Do runs fn(w, i) for i in [0, n) on min(Workers(), n) goroutines taking
// items in order from a shared counter; w in [0, workers) identifies the
// goroutine (for per-worker state). With one worker everything runs on
// the calling goroutine. A panic stops the remaining items and is raised
// again on the calling goroutine.
func Do(n int, fn func(w, i int)) {
	DoN(n, Workers(), fn)
}

// DoN is Do with at most nw workers.
func DoN(n, nw int, fn func(w, i int)) {
	nw = min(nw, n, Workers())
	if nw <= 1 {
		for i := 0; i < n; i++ {
			fn(0, i)
		}
		return
	}
	var next atomic.Int64
	var panicked atomic.Pointer[Panic]
	var wg sync.WaitGroup
	for w := 0; w < nw; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panicked.CompareAndSwap(nil, &Panic{r})
					next.Store(int64(n)) // stop the others
				}
			}()
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				fn(w, i)
			}
		}()
	}
	wg.Wait()
	if p := panicked.Load(); p != nil {
		panic(p.Value)
	}
}

// Sem bounds the extra goroutines of a recursive computation: TryGo runs
// fn on a new goroutine while fewer than Workers()-1 of its goroutines
// are running, else on the caller. Without a cap below GOMAXPROCS there
// is no bound (the scheduler already runs GOMAXPROCS goroutines at once):
// a goroutine that waits for its subtasks keeps its slot, so slots would
// cost parallelism.
type Sem struct {
	slots     chan struct{}
	unbounded bool
}

// NewSem returns a Sem for the current cap.
func NewSem() *Sem {
	n := Workers()
	if n == runtime.GOMAXPROCS(0) && n > 1 {
		return &Sem{unbounded: true}
	}
	return &Sem{slots: make(chan struct{}, n-1)}
}

// Task is a computation started by TryGo.
type Task struct {
	done chan struct{}
	p    any
}

// TryGo starts fn, on a goroutine when a slot is free (nil Sem: never).
// Wait must be called on the result.
func (s *Sem) TryGo(fn func()) *Task {
	t := &Task{}
	if s != nil && s.unbounded {
		t.done = make(chan struct{})
		go func() {
			defer func() {
				t.p = recover()
				close(t.done)
			}()
			fn()
		}()
		return t
	}
	if s != nil {
		select {
		case s.slots <- struct{}{}:
			t.done = make(chan struct{})
			go func() {
				defer func() {
					t.p = recover()
					<-s.slots
					close(t.done)
				}()
				fn()
			}()
			return t
		default:
		}
	}
	fn()
	return t
}

// Wait waits for the task and raises its panic, if any, on the caller.
func (t *Task) Wait() {
	if t.done != nil {
		<-t.done
	}
	if t.p != nil {
		panic(t.p)
	}
}
