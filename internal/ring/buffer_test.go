package ring

import (
	"sync"
	"testing"
)

func TestNewDefaultCapacity(t *testing.T) {
	rb := New[int](0)
	if rb.Cap() != 64 {
		t.Fatalf("expected default capacity 64, got %d", rb.Cap())
	}
	if rb.Len() != 0 {
		t.Fatalf("expected len 0, got %d", rb.Len())
	}
}

func TestNewExplicitCapacity(t *testing.T) {
	rb := New[int](100)
	if rb.Cap() != 100 {
		t.Fatalf("expected capacity 100, got %d", rb.Cap())
	}
}

func TestPushAndLen(t *testing.T) {
	rb := New[int](5)

	for i := 0; i < 5; i++ {
		rb.Push(i)
		if rb.Len() != i+1 {
			t.Fatalf("step %d: expected len %d, got %d", i, i+1, rb.Len())
		}
	}
}

func TestOverwriteWhenFull(t *testing.T) {
	rb := New[int](3)
	for i := 0; i < 3; i++ {
		rb.Push(i) // [0,1,2]
	}
	// Push three more to overwrite entirely.
	for i := 3; i < 6; i++ {
		rb.Push(i) // [3,4,5]
	}
	if rb.Len() != 3 {
		t.Fatalf("expected len 3, got %d", rb.Len())
	}

	vals := rb.Values()
	if len(vals) != 3 {
		t.Fatalf("expected 3 values, got %d", len(vals))
	}
	for i, v := range vals {
		if v != i+3 {
			t.Fatalf("index %d: expected %d, got %d", i, i+3, v)
		}
	}
}

func TestValuesOrder(t *testing.T) {
	rb := New[string](5)
	rb.Push("a")
	rb.Push("b")
	rb.Push("c")

	vals := rb.Values()
	if len(vals) != 3 {
		t.Fatalf("expected 3 values, got %d", len(vals))
	}
	expected := []string{"a", "b", "c"}
	for i, v := range vals {
		if v != expected[i] {
			t.Fatalf("index %d: expected %q, got %q", i, expected[i], v)
		}
	}
}

func TestValuesEmpty(t *testing.T) {
	rb := New[int](5)
	vals := rb.Values()
	if vals != nil {
		t.Fatalf("expected nil, got %v", vals)
	}
}

func TestValuesAfterPartialOverwrite(t *testing.T) {
	rb := New[int](4)
	for i := 0; i < 6; i++ {
		rb.Push(i)
	}
	// Ring now has [2,3,4,5]
	vals := rb.Values()
	if len(vals) != 4 {
		t.Fatalf("expected 4 values, got %d", len(vals))
	}
	for i, v := range vals {
		if v != i+2 {
			t.Fatalf("index %d: expected %d, got %d", i, i+2, v)
		}
	}
}

func TestGetValid(t *testing.T) {
	rb := New[int](10)
	for i := 0; i < 5; i++ {
		rb.Push(i * 10)
	}

	for i := 0; i < 5; i++ {
		v, ok := rb.Get(i)
		if !ok {
			t.Fatalf("index %d: expected ok=true", i)
		}
		if v != i*10 {
			t.Fatalf("index %d: expected %d, got %d", i, i*10, v)
		}
	}
}

func TestGetOutOfRange(t *testing.T) {
	rb := New[int](5)
	rb.Push(1)
	rb.Push(2)

	_, ok := rb.Get(5)
	if ok {
		t.Fatal("expected ok=false for out-of-range index")
	}

	_, ok = rb.Get(-1)
	if ok {
		t.Fatal("expected ok=false for negative index")
	}
}

func TestGetFromEmpty(t *testing.T) {
	rb := New[int](5)
	_, ok := rb.Get(0)
	if ok {
		t.Fatal("expected ok=false for empty buffer")
	}
}

func TestGetWraparound(t *testing.T) {
	rb := New[int](3)
	for i := 0; i < 6; i++ {
		rb.Push(i)
	}
	// ring contains [3,4,5]
	for i := 0; i < 3; i++ {
		v, ok := rb.Get(i)
		if !ok {
			t.Fatalf("index %d after wraparound: expected ok=true", i)
		}
		if v != i+3 {
			t.Fatalf("index %d after wraparound: expected %d, got %d", i, i+3, v)
		}
	}
}

func TestDoIteratesOldestToNewest(t *testing.T) {
	rb := New[int](5)
	for i := 0; i < 5; i++ {
		rb.Push(i)
	}

	var got []int
	rb.Do(func(v int) bool {
		got = append(got, v)
		return true
	})

	if len(got) != 5 {
		t.Fatalf("expected 5 items, got %d", len(got))
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("index %d: expected %d, got %d", i, i, v)
		}
	}
}

func TestDoEarlyExit(t *testing.T) {
	rb := New[int](10)
	for i := 0; i < 10; i++ {
		rb.Push(i)
	}

	var got []int
	rb.Do(func(v int) bool {
		got = append(got, v)
		return v < 4 // stop after 5 items (0..4)
	})

	if len(got) != 5 {
		t.Fatalf("expected 5 items with early exit, got %d: %v", len(got), got)
	}
}

func TestDoEmpty(t *testing.T) {
	rb := New[int](5)
	called := false
	rb.Do(func(v int) bool {
		called = true
		return true
	})
	if called {
		t.Fatal("Do should not call f on empty buffer")
	}
}

func TestDoWraparound(t *testing.T) {
	rb := New[int](3)
	for i := 0; i < 6; i++ {
		rb.Push(i)
	}
	// ring contains [3,4,5], write position is at index 0

	var got []int
	rb.Do(func(v int) bool {
		got = append(got, v)
		return true
	})

	if len(got) != 3 {
		t.Fatalf("expected 3 items, got %d", len(got))
	}
	for i, v := range got {
		if v != i+3 {
			t.Fatalf("index %d: expected %d, got %d", i, i+3, v)
		}
	}
}

func TestIterateChannel(t *testing.T) {
	rb := New[int](5)
	for i := 0; i < 5; i++ {
		rb.Push(i)
	}

	var got []int
	for v := range rb.Iterate() {
		got = append(got, v)
	}

	if len(got) != 5 {
		t.Fatalf("expected 5 items, got %d", len(got))
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("index %d: expected %d, got %d", i, i, v)
		}
	}
}

func TestIterateEmpty(t *testing.T) {
	rb := New[int](5)
	count := 0
	for range rb.Iterate() {
		count++
	}
	if count != 0 {
		t.Fatalf("expected 0 items from empty buffer, got %d", count)
	}
}

func TestReset(t *testing.T) {
	rb := New[int](5)
	for i := 0; i < 5; i++ {
		rb.Push(i)
	}

	rb.Reset()
	if rb.Len() != 0 {
		t.Fatalf("expected len 0 after reset, got %d", rb.Len())
	}
	if rb.Cap() != 5 {
		t.Fatalf("expected cap 5 after reset, got %d", rb.Cap())
	}

	// Buffer should accept new items after reset.
	rb.Push(99)
	if rb.Len() != 1 {
		t.Fatalf("expected len 1 after push after reset, got %d", rb.Len())
	}
	v, ok := rb.Get(0)
	if !ok || v != 99 {
		t.Fatalf("expected 99 at index 0 after reset, got %d (ok=%v)", v, ok)
	}
}

func TestNilSafety(t *testing.T) {
	var rb *RingBuffer[int]

	if l := rb.Len(); l != 0 {
		t.Fatalf("nil Len: expected 0, got %d", l)
	}
	if c := rb.Cap(); c != 0 {
		t.Fatalf("nil Cap: expected 0, got %d", c)
	}

	rb.Push(1) // should not panic
	if vals := rb.Values(); vals != nil {
		t.Fatalf("nil Values: expected nil, got %v", vals)
	}

	rb.Do(func(v int) bool {
		t.Fatal("Do on nil buffer should not call f")
		return true
	})

	for range rb.Iterate() {
		t.Fatal("Iterate on nil buffer should not yield")
	}

	_, ok := rb.Get(0)
	if ok {
		t.Fatal("nil Get: expected ok=false")
	}

	rb.Reset() // should not panic
}

func TestConcurrentPush(t *testing.T) {
	rb := New[int](1000)
	var wg sync.WaitGroup
	n := 100

	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < n; j++ {
				rb.Push(j)
			}
		}()
	}
	wg.Wait()

	if rb.Len() != 1000 {
		t.Fatalf("expected len 1000, got %d", rb.Len())
	}
}

func TestConcurrentReadWrite(t *testing.T) {
	rb := New[int](100)
	var wg sync.WaitGroup

	// Writer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			rb.Push(i)
		}
	}()

	// Concurrent readers.
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				rb.Len()
				rb.Values()
				rb.Do(func(v int) bool { return true })
				rb.Get(0)
			}
		}()
	}
	wg.Wait()
}

func TestLargeOverwrite(t *testing.T) {
	rb := New[int](10)
	for i := 0; i < 1000; i++ {
		rb.Push(i)
	}

	if rb.Len() != 10 {
		t.Fatalf("expected len 10 after many overwrites, got %d", rb.Len())
	}

	vals := rb.Values()
	if len(vals) != 10 {
		t.Fatalf("expected 10 values, got %d", len(vals))
	}
	// Should be the last 10 values: 990..999
	for i, v := range vals {
		if v != 990+i {
			t.Fatalf("index %d: expected %d, got %d", i, 990+i, v)
		}
	}
}

func TestPointerType(t *testing.T) {
	type item struct {
		val int
	}

	rb := New[*item](5)
	for i := 0; i < 5; i++ {
		rb.Push(&item{val: i})
	}

	vals := rb.Values()
	if len(vals) != 5 {
		t.Fatalf("expected 5 pointer items, got %d", len(vals))
	}
	for i, v := range vals {
		if v.val != i {
			t.Fatalf("index %d: expected val %d, got %d", i, i, v.val)
		}
	}

	// Modify through pointer.
	vals[0].val = 99
	readBack, ok := rb.Get(0)
	if !ok || readBack.val != 99 {
		t.Fatal("pointer mutation should be visible through Get")
	}
}

func TestCapConsistency(t *testing.T) {
	for cap := 1; cap <= 100; cap++ {
		rb := New[int](cap)
		if rb.Cap() != cap {
			t.Fatalf("New(%d).Cap() = %d", cap, rb.Cap())
		}
		for i := 0; i < cap*2; i++ {
			rb.Push(i)
		}
		if rb.Len() != cap {
			t.Fatalf("after pushing %d to cap %d: Len=%d", cap*2, cap, rb.Len())
		}
	}
}

func TestZeroValueBuffer(t *testing.T) {
	var rb RingBuffer[int]

	if l := rb.Len(); l != 0 {
		t.Fatalf("zero-value Len: expected 0, got %d", l)
	}
	rb.Push(42) // should not panic; zero-value ring is nil
	if l := rb.Len(); l != 0 {
		t.Fatalf("zero-value Push should be a no-op: expected Len 0, got %d", l)
	}
}

func TestGetAfterOverwriteSequence(t *testing.T) {
	rb := New[int](3)
	// Fill, overwrite, then check specific indices.
	for i := 0; i < 10; i++ {
		rb.Push(i)
	}
	// Buffer has [7,8,9] (oldest to newest)
	tests := []struct {
		idx      int
		wantVal  int
		wantOK   bool
	}{
		{0, 7, true},
		{1, 8, true},
		{2, 9, true},
		{3, 0, false},
	}
	for _, tt := range tests {
		v, ok := rb.Get(tt.idx)
		if ok != tt.wantOK {
			t.Errorf("Get(%d) ok=%v, want %v", tt.idx, ok, tt.wantOK)
		}
		if ok && v != tt.wantVal {
			t.Errorf("Get(%d) = %d, want %d", tt.idx, v, tt.wantVal)
		}
	}
}
