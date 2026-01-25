package core

import "sync"

type pauseGate struct {
	mu     sync.Mutex
	paused bool
	ch     chan struct{} // closed == gate open, open == gate closed (paused)
}

func newPauseGate() *pauseGate {
	ch := make(chan struct{})
	close(ch) // start unpaused (gate open)
	return &pauseGate{ch: ch}
}

func (g *pauseGate) wait() {
	g.mu.Lock()
	ch := g.ch
	g.mu.Unlock()

	<-ch // returns immediately if unpaused; blocks if paused
}

func (g *pauseGate) pause() {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.paused {
		return
	}
	g.paused = true
	g.ch = make(chan struct{}) // open channel => waiters block
}

func (g *pauseGate) resume() {
	g.mu.Lock()
	if !g.paused {
		g.mu.Unlock()
		return
	}
	g.paused = false
	ch := g.ch
	g.mu.Unlock()

	close(ch) // release all waiters
}
