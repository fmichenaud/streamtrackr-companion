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
func TestPushSyncCancelsOnlyTheNamedPushes(t *testing.T) {
	pushes := newPushSync()
	a := pushes.startUnlock("ACH_A")
	b := pushes.startUnlock("ACH_B")

	stopped := pushes.cancelUnlocks([]string{"ACH_A", "ACH_NEVER_PUSHED"})

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
func TestPushSyncFinishedPushIsNotCancelled(t *testing.T) {
	pushes := newPushSync()
	c := pushes.startUnlock("ACH_A")
	pushes.finishUnlock("ACH_A", c)

	if stopped := pushes.cancelUnlocks([]string{"ACH_A"}); len(stopped) != 0 {
		t.Errorf("cancel reported %v for a push that already finished", stopped)
	}
	if isClosed(c) {
		t.Error("a finished push must not be closed as if cancelled")
	}
}

// Earning an achievement again (after a reset) while the previous push
// somehow still lingers: the stale one goes, the new one lives.
func TestPushSyncStartReplacesAStalePush(t *testing.T) {
	pushes := newPushSync()
	first := pushes.startUnlock("ACH_A")
	second := pushes.startUnlock("ACH_A")

	if !isClosed(first) {
		t.Error("the stale push should have been cancelled by the new one")
	}
	if isClosed(second) {
		t.Error("the new push must still be live")
	}
	if stopped := pushes.cancelUnlocks([]string{"ACH_A"}); len(stopped) != 1 {
		t.Errorf("cancel reported %v, want the live push", stopped)
	}
}

// A goroutine finishing late must not deregister the push that replaced
// it — otherwise the next relock has nothing to cancel.
func TestPushSyncFinishIgnoresAReplacedChannel(t *testing.T) {
	pushes := newPushSync()
	first := pushes.startUnlock("ACH_A")
	second := pushes.startUnlock("ACH_A")

	pushes.finishUnlock("ACH_A", first) // the stale goroutine, waking up late

	if stopped := pushes.cancelUnlocks([]string{"ACH_A"}); len(stopped) != 1 {
		t.Errorf("cancel reported %v, want the live push still registered", stopped)
	}
	if !isClosed(second) {
		t.Error("the live push should have been cancelled")
	}
}

// The barrier is scoped the same way the cancellation is: an unlock waits
// only for the relocks that carry its own name. A relock of A stuck in
// retries must not hold up the announcement of B.
func TestPushSyncUnlockWaitsOnlyOnItsOwnRelocks(t *testing.T) {
	pushes := newPushSync()
	settled := pushes.startRelock([]string{"ACH_A", "ACH_B"})

	if got := pushes.relocksFor("ACH_A"); len(got) != 1 || got[0] != settled {
		t.Errorf("an unlock of ACH_A should wait for the relock covering it, got %v", got)
	}
	if got := pushes.relocksFor("ACH_C"); got != nil {
		t.Errorf("an unlock of ACH_C waited for an unrelated relock: %v", got)
	}

	pushes.finishRelock([]string{"ACH_A", "ACH_B"}, settled)

	if !isClosed(settled) {
		t.Error("finishing a relock should release what waits on it")
	}
	if got := pushes.relocksFor("ACH_A"); got != nil {
		t.Errorf("a finished relock is still registered: %v", got)
	}
}

// Two relocks can overlap (they are not serialised against each other),
// so a name covered by both releases only when both are done.
func TestPushSyncOverlappingRelocks(t *testing.T) {
	pushes := newPushSync()
	first := pushes.startRelock([]string{"ACH_A"})
	second := pushes.startRelock([]string{"ACH_A", "ACH_B"})

	if got := pushes.relocksFor("ACH_A"); len(got) != 2 {
		t.Fatalf("ACH_A should wait for both relocks, got %d", len(got))
	}

	pushes.finishRelock([]string{"ACH_A"}, first)

	got := pushes.relocksFor("ACH_A")
	if len(got) != 1 || got[0] != second {
		t.Fatalf("ACH_A should still wait for the second relock, got %v", got)
	}
	if got := pushes.relocksFor("ACH_B"); len(got) != 1 {
		t.Errorf("ACH_B lost its own relock when an unrelated one finished: %v", got)
	}
}

// A relock snapshotted at detection time is what an unlock waits for; one
// registered afterwards cancels that push instead (see cancelUnlocks), so
// it must not retroactively appear in an earlier snapshot.
func TestPushSyncRelocksForIsASnapshot(t *testing.T) {
	pushes := newPushSync()
	before := pushes.relocksFor("ACH_A")

	pushes.startRelock([]string{"ACH_A"})

	if len(before) != 0 {
		t.Errorf("the snapshot taken before the relock changed under us: %v", before)
	}
	if len(pushes.relocksFor("ACH_A")) != 1 {
		t.Error("a later snapshot should see the relock")
	}
}
