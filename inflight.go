package main

import "sync"

// inflightUnlocks tracks the unlock pushes that are still alive — a push
// keeps retrying for up to ~20 s (three attempts, 5 s timeout, 3 s of
// backoff, 8 s on no_baseline) — so that a re-lock of the SAME
// achievement can call them off.
//
// Without this, a push that was blipped by the network could land after
// the relock that undid it: the server ends up unlocked while Steam says
// locked, and nothing ever notices. The next rescan sees no transition
// (both sides are stable), and the next time the player earns it the
// server answers already_unlocked — the achievement is never announced
// again. Exactly the class of bug re-locks exist to fix, in reverse.
//
// Only the same appid + apiName is a conflict: /api/companion/steam/relock
// is scoped to the names it carries, so an unlock of B and a relock of A
// have nothing to say to each other and must not wait for one another.
type inflightUnlocks struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}

func newInflightUnlocks() *inflightUnlocks {
	return &inflightUnlocks{m: map[string]chan struct{}{}}
}

// start registers a push for apiName and returns the channel that cancels
// it. Any push still registered for the same name is cancelled first: it
// can only be a stale one, since coming back to unlocked means having been
// re-locked in between.
func (i *inflightUnlocks) start(apiName string) chan struct{} {
	i.mu.Lock()
	defer i.mu.Unlock()
	if prev, ok := i.m[apiName]; ok {
		close(prev)
	}
	c := make(chan struct{})
	i.m[apiName] = c
	return c
}

// finish deregisters a finished push. The channel identity check matters:
// a slow goroutine must not drop the registration of the push that
// replaced it.
func (i *inflightUnlocks) finish(apiName string, c chan struct{}) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if cur, ok := i.m[apiName]; ok && cur == c {
		delete(i.m, apiName)
	}
}

// cancel calls off the pushes for these names and returns the ones it
// actually stopped, for the log — "the relock cancelled an unlock still
// in flight" is the line that explains an otherwise silent non-delivery.
func (i *inflightUnlocks) cancel(apiNames []string) []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	var stopped []string
	for _, name := range apiNames {
		if c, ok := i.m[name]; ok {
			close(c)
			delete(i.m, name)
			stopped = append(stopped, name)
		}
	}
	return stopped
}
