package app

import "sync"

// soFlightGroup coalesces identical in-flight SO lookups so N concurrent
// requests for the same state share one bounded POST. The flight key is the
// canonical gate key, which never contains a credential value.
type soFlightGroup struct {
	mu    sync.Mutex
	calls map[[32]byte]*soFlightCall
}

type soFlightCall struct {
	done chan struct{}
	d    routeResult
}

func newSOFlightGroup() *soFlightGroup {
	return &soFlightGroup{calls: make(map[[32]byte]*soFlightCall)}
}

// do runs fn once per key while a call is live. A waiter returns the same
// typed decision; no raw prompt, key or confidence crosses the boundary.
func (g *soFlightGroup) do(k [32]byte, fn func() routeResult) routeResult {
	g.mu.Lock()
	if c, ok := g.calls[k]; ok {
		g.mu.Unlock()
		<-c.done
		return c.d
	}
	c := &soFlightCall{done: make(chan struct{})}
	g.calls[k] = c
	g.mu.Unlock()
	c.d = fn()
	close(c.done)
	g.mu.Lock()
	delete(g.calls, k)
	g.mu.Unlock()
	return c.d
}
