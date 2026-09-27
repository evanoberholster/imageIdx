package metrics

import (
	"testing"
	"time"
)

func TestTimerAccumulates(t *testing.T) {
	var d time.Duration
	first := Start()
	time.Sleep(time.Millisecond)
	first.Add(&d)
	if d <= 0 {
		t.Fatal("timer should accumulate positive duration")
	}
	before := d
	second := Start()
	time.Sleep(time.Millisecond)
	second.Add(&d)
	if d <= before {
		t.Fatal("timer should accumulate across phases")
	}
}

func TestTimerNoAlloc(t *testing.T) {
	var d time.Duration
	if a := testing.AllocsPerRun(100, func() {
		inner := Start()
		inner.Add(&d)
	}); a != 0 {
		t.Fatalf("timer allocs=%v", a)
	}
}

func TestOperationAndModeValues(t *testing.T) {
	if OperationSearch == OperationFilterIDs || OperationFilterIDs == OperationRankTime || OperationRankTime == OperationRankGeo {
		t.Fatal("operations must be distinct")
	}
	if StageModeSkipped == StageModeSource || StageModeSource == StageModeFilter {
		t.Fatal("stage modes must be distinct")
	}
}
