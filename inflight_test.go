package main

import "testing"

func isClosed(c chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// The load-bearing property: a relock stops the push for the SAME
// achievement and nothing else. An unlock of B must not be collateral
// damage of a reset that only cleared A.
func TestInflightCancelsOnlyTheNamedPushes(t *testing.T) {
	inflight := newInflightUnlocks()
	a := inflight.start("ACH_A")
	b := inflight.start("ACH_B")

	stopped := inflight.cancel([]string{"ACH_A", "ACH_NEVER_PUSHED"})

	if len(stopped) != 1 || stopped[0] != "ACH_A" {
		t.Fatalf("cancel reported %v, want [ACH_A]", stopped)
	}
	if !isClosed(a) {
		t.Error("ACH_A push should have been cancelled")
	}
	if isClosed(b) {
		t.Error("ACH_B push was cancelled by a relock that never mentioned it")
	}
}

// A push that already finished is not something to call off — the log
// line "called off N pushes" has to mean it.
func TestInflightFinishedPushIsNotCancelled(t *testing.T) {
	inflight := newInflightUnlocks()
	c := inflight.start("ACH_A")
	inflight.finish("ACH_A", c)

	if stopped := inflight.cancel([]string{"ACH_A"}); len(stopped) != 0 {
		t.Errorf("cancel reported %v for a push that already finished", stopped)
	}
	if isClosed(c) {
		t.Error("a finished push must not be closed as if cancelled")
	}
}

// Earning an achievement again (after a reset) while the previous push
// somehow still lingers: the stale one goes, the new one lives.
func TestInflightStartReplacesAStalePush(t *testing.T) {
	inflight := newInflightUnlocks()
	first := inflight.start("ACH_A")
	second := inflight.start("ACH_A")

	if !isClosed(first) {
		t.Error("the stale push should have been cancelled by the new one")
	}
	if isClosed(second) {
		t.Error("the new push must still be live")
	}
	if stopped := inflight.cancel([]string{"ACH_A"}); len(stopped) != 1 {
		t.Errorf("cancel reported %v, want the live push", stopped)
	}
}

// A goroutine finishing late must not deregister the push that replaced
// it — otherwise the next relock has nothing to cancel.
func TestInflightFinishIgnoresAReplacedChannel(t *testing.T) {
	inflight := newInflightUnlocks()
	first := inflight.start("ACH_A")
	second := inflight.start("ACH_A")

	inflight.finish("ACH_A", first) // the stale goroutine, waking up late

	if stopped := inflight.cancel([]string{"ACH_A"}); len(stopped) != 1 {
		t.Errorf("cancel reported %v, want the live push still registered", stopped)
	}
	if !isClosed(second) {
		t.Error("the live push should have been cancelled")
	}
}
