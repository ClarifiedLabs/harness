package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"harness/internal/llm"
	"harness/internal/modelproxy/protocol"
)

func (p *Provider) Steer(ctx context.Context, submission llm.SteerSubmission) error {
	body, err := json.Marshal(protocol.SteerRequest{TargetID: p.targetID, Submission: submission})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.client.baseURL+"/v1/steer", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	p.client.setAuth(req)
	resp, err := p.client.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusNotFound {
		return llm.ErrSteeringUnavailable
	}
	if resp.StatusCode != http.StatusAccepted {
		return readHTTPError(resp)
	}
	return nil
}
