// Package handoff carries the small approval request shared by the
// request_implementation tool and interactive drivers. Structured planning and
// execution state belongs to internal/workstate.
package handoff

import "sync"

// Request is one proposed transition from a ready WorkState to implementation.
// Brief is optional supplementary model-authored context; Message is optional
// user guidance supplied at approval time.
type Request struct {
	Brief        string
	Agent        string
	ArtifactPath string
	Model        string
	Message      string
}

// Pending holds at most one approval request. Request replaces any older one.
type Pending struct {
	mu  sync.Mutex
	req *Request
}

func NewPending() *Pending { return &Pending{} }

func (p *Pending) Request(req Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	clone := req
	p.req = &clone
}

func (p *Pending) Peek() (Request, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.req == nil {
		return Request{}, false
	}
	return *p.req, true
}

func (p *Pending) Take() (Request, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.req == nil {
		return Request{}, false
	}
	req := *p.req
	p.req = nil
	return req, true
}
