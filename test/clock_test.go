package test

import (
	"go-mcast/internal"
	"sync"
	"testing"
)

func TestLogicalClock_GroupTick(t *testing.T) {
	concurrentMembers := 50
	clk := new(internal.LogicalClock)

	wg := &sync.WaitGroup{}
	wg.Add(concurrentMembers)

	act := func() {
		defer wg.Done()
		clk.Tick()
	}

	for i := 0; i < concurrentMembers; i++ {
		go act()
	}

	wg.Wait()

	if clk.Tock() != uint64(concurrentMembers) {
		t.Fatalf("failed on concurrent increment %d: %d", concurrentMembers, clk.Tock())
	}

	clk.Defines(0)
	if clk.Tock() != 0 {
		t.Fatalf("failed on define: %d", clk.Tock())
	}
}
