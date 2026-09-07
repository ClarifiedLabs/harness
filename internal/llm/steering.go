package llm

import (
	"context"
	"errors"
)

var ErrSteeringInterrupted = errors.New("native steering interrupted; input retained in history")

var ErrSteeringUnavailable = errors.New("native steering is unavailable for this response")

// SteerSubmission identifiers and session routing are local control metadata;
// only Messages are sent to the provider as user input.
type SteerSubmission struct {
	ID            string    `json:"id"`
	SessionID     string    `json:"session_id,omitempty"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	Messages      []Message `json:"messages"`
}

type LiveSteerer interface {
	Steer(context.Context, SteerSubmission) error
}

// LiveSteerEvent separates queue acceptance from application by a successor
// response. Boundary marks an automatic successor within the same stream.
type LiveSteerEvent struct {
	Status             string          `json:"status"`
	Submission         SteerSubmission `json:"submission"`
	ServerID           string          `json:"server_id,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Boundary           bool            `json:"boundary,omitempty"`
}
