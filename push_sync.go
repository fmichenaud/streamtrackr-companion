package main

import "sync"

// pushSync keeps the two kinds of push in step without making unrelated
// ones wait for each other. One rule, applied in both directions: pushes
// interact only when they name the same achievement. /steam/relock is
// scoped to the names it carries, so an unlock of B and a relock of A
// have nothing to say to each other.
//
//   - a relock CANCELS an unlock push still retrying for the same name.
//     A push keeps retrying for up to ~20 s (three attempts, 5 s timeout,
//     3 s of backoff, 8 s on no_baseline), so one blipped by the network
//     can land after the relock that undid it: the server ends up
//     unlocked while Steam says locked, and nothing ever notices — both
//     sides are then stable, so no rescan sees a transition, and the next
//     time the player earns it the server answers already_unlocked. The
//     achievement is never announced again. Ordering it would not help
//     either: the server would still announce a trophy the player just
//     erased.
//
//   - an unlock push WAITS for the relocks in flight that carry its name.
//     Earning an achievement again right after a reset must not reach the
//     server before the reset does, or it is refused as already unlocked
//     and then locked away.
//
// Relocks are deliberately not serialised against each other: relocking a
// name twice is idempotent, and an unlock that would need to sit between
// them is cancelled by the second one rather than ordered after it.
type pushSync struct {
	mu      sync.Mutex
	unlocks map[string]chan struct{}   // apiName → cancel channel
	relocks map[string][]chan struct{} // apiName → the settled channels covering it
}

func newPushSync() *pushSync {
	return &pushSync{
		unlocks: map[string]chan struct{}{},
		relocks: map[string][]chan struct{}{},
	}
}

// startUnlock registers a push for apiName and returns the channel that
// cancels it. Any push still registered for the same name is cancelled
// first: it can only be a stale one, since coming back to unlocked means
// having been re-locked in between.
func (p *pushSync) startUnlock(apiName string) chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if prev, ok := p.unlocks[apiName]; ok {
		close(prev)
	}
	c := make(chan struct{})
	p.unlocks[apiName] = c
	return c
}

// finishUnlock deregisters a finished push. The channel identity check
// matters: a slow goroutine must not drop the registration of the push
// that replaced it.
func (p *pushSync) finishUnlock(apiName string, c chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cur, ok := p.unlocks[apiName]; ok && cur == c {
		delete(p.unlocks, apiName)
	}
}

// cancelUnlocks calls off the pushes for these names and returns the ones
// it actually stopped, for the log — "the relock cancelled an unlock still
// in flight" is the line that explains an otherwise silent non-delivery.
func (p *pushSync) cancelUnlocks(apiNames []string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var stopped []string
	for _, name := range apiNames {
		if c, ok := p.unlocks[name]; ok {
			close(c)
			delete(p.unlocks, name)
			stopped = append(stopped, name)
		}
	}
	return stopped
}

// startRelock registers a relock push over these names and returns the
// channel closed when it is done.
func (p *pushSync) startRelock(apiNames []string) chan struct{} {
	settled := make(chan struct{})
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, name := range apiNames {
		p.relocks[name] = append(p.relocks[name], settled)
	}
	return settled
}

// finishRelock deregisters the push and releases whatever was waiting on it.
func (p *pushSync) finishRelock(apiNames []string, settled chan struct{}) {
	p.mu.Lock()
	for _, name := range apiNames {
		kept := p.relocks[name][:0]
		for _, c := range p.relocks[name] {
			if c != settled {
				kept = append(kept, c)
			}
		}
		if len(kept) == 0 {
			delete(p.relocks, name)
		} else {
			p.relocks[name] = kept
		}
	}
	p.mu.Unlock()
	close(settled)
}

// relocksFor snapshots the relocks an unlock of apiName must wait for.
// Taken at detection time: a relock registered afterwards is a later
// event, and cancels this push instead of being waited on.
func (p *pushSync) relocksFor(apiName string) []chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.relocks[apiName]) == 0 {
		return nil
	}
	return append([]chan struct{}(nil), p.relocks[apiName]...)
}
