package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"harness/internal/llm"
	"harness/internal/ws"
)

type pendingLiveSteer struct {
	submission llm.SteerSubmission
	responseID string
	serverID   string
}
type liveSteering struct {
	mu                sync.Mutex
	conn              *ws.Conn
	responseID        string
	enabled, active   bool
	appliedThisStream bool
	pending           *pendingLiveSteer
}

// Steer sends at most one unresolved input per connection. A successful write
// is not acceptance; acknowledgements and application arrive on the stream.
func (p *Provider) Steer(ctx context.Context, submission llm.SteerSubmission) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.live.mu.Lock()
	defer p.live.mu.Unlock()
	if !p.live.enabled || !p.live.active || p.live.appliedThisStream || p.live.conn == nil || p.live.pending != nil || len(submission.Messages) == 0 {
		return llm.ErrSteeringUnavailable
	}
	body, err := json.Marshal(struct {
		Type     string          `json:"type"`
		Previous string          `json:"previous_response_id"`
		Input    []wireInputItem `json:"input"`
	}{"response.steer", p.live.responseID, buildInput(submission.Messages, false)})
	if err != nil {
		return err
	}
	pending := &pendingLiveSteer{submission: submission, responseID: p.live.responseID}
	p.live.pending = pending
	if err := p.live.conn.SendTextContext(ctx, string(body)); err != nil {
		return fmt.Errorf("write native steer: %w", err)
	}
	return nil
}
func (p *Provider) beginLive(conn *ws.Conn, enabled bool) {
	p.live.mu.Lock()
	defer p.live.mu.Unlock()
	p.live.conn = conn
	p.live.enabled = enabled
	p.live.appliedThisStream = false
	p.live.active = false
}
func (p *Provider) endLive(failed bool, yield func(llm.StreamEvent, error) bool) {
	p.live.mu.Lock()
	p.live.active = false
	p.live.enabled = false
	p.live.conn = nil
	pending := p.live.pending
	if failed {
		p.live.pending = nil
	}
	p.live.mu.Unlock()
	if failed && pending != nil {
		yield(liveEvent("lost", pending, false, nil), nil)
	}
}
func liveEvent(status string, pending *pendingLiveSteer, boundary bool, usage *llm.Usage) llm.StreamEvent {
	return llm.StreamEvent{Kind: llm.EventLiveSteer, Usage: usage, LiveSteer: &llm.LiveSteerEvent{Status: status, Submission: pending.submission, ServerID: pending.serverID, PreviousResponseID: pending.responseID, Boundary: boundary}}
}
func (p *Provider) liveTerminal() bool {
	p.live.mu.Lock()
	defer p.live.mu.Unlock()
	p.live.active = false
	return p.live.pending != nil
}

// handleLiveFrame handles control events before ordinary response decoding.
// Applied input is emitted only after a distinct successor response exists.
func (p *Provider) handleLiveFrame(data string, terminal *llm.StreamEvent, yield func(llm.StreamEvent, error) bool) (handled, successor, stop bool) {
	var event struct {
		Type     string        `json:"type"`
		Response *wireResponse `json:"response"`
		Steer    struct {
			ID       string `json:"id"`
			Previous string `json:"previous_response_id"`
		} `json:"steer"`
	}
	if json.Unmarshal([]byte(data), &event) != nil {
		return false, false, false
	}
	p.live.mu.Lock()
	pending := p.live.pending
	if pending != nil && ((event.Steer.Previous != "" && event.Steer.Previous != pending.responseID) || (pending.serverID != "" && event.Steer.ID != "" && event.Steer.ID != pending.serverID)) {
		pending = nil
	}
	var output *llm.StreamEvent
	switch event.Type {
	case "response.created":
		if event.Response != nil {
			p.live.responseID = event.Response.ID
			p.live.active = true
			if pending != nil && event.Response.ID != pending.responseID {
				var usage *llm.Usage
				if terminal != nil {
					usage = terminal.Usage
				}
				ev := liveEvent("applied", pending, terminal != nil, usage)
				output = &ev
				p.live.pending = nil
				p.live.appliedThisStream = true
				successor = true
			}
		}
	case "response.steer.accepted":
		handled = true
		if pending != nil {
			pending.serverID = event.Steer.ID
			ev := liveEvent("accepted", pending, false, nil)
			output = &ev
		}
	case "response.steer.pending":
		handled = true
	case "response.steer.failed":
		handled = true
		if pending != nil {
			ev := liveEvent("failed", pending, false, nil)
			output = &ev
			p.live.pending = nil
		}
	}
	p.live.mu.Unlock()
	if output != nil && !yield(*output, nil) {
		return handled, successor, true
	}
	return handled, successor, false
}
