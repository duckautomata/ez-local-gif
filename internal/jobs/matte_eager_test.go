package jobs

import (
	"context"
	"testing"
)

// TestMatteEager: the eager mark travels on the ctx, defaults to false, can
// be set and cleared, and the derived ctx is otherwise the parent (its
// Done/Err are inherited).
func TestMatteEager(t *testing.T) {
	if MatteEager(nil) {
		t.Error("MatteEager(nil) = true, want false")
	}
	if MatteEager(context.Background()) {
		t.Error("an unmarked ctx reads as eager")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eager := WithMatteEager(ctx, true)
	if !MatteEager(eager) {
		t.Error("WithMatteEager(ctx, true) does not read as eager")
	}
	if MatteEager(WithMatteEager(eager, false)) {
		t.Error("WithMatteEager(eager, false) still reads as eager")
	}
	if MatteEager(WithMatteEager(ctx, false)) {
		t.Error("WithMatteEager(ctx, false) reads as eager")
	}
	if !MatteEager(context.WithValue(eager, struct{}{}, 1)) {
		t.Error("the mark is lost on a derived ctx")
	}
	cancel()
	if eager.Err() == nil {
		t.Error("the eager ctx did not inherit the parent's cancellation")
	}
}
