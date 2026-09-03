package server

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Duration is a JSON duration setting. Strings use Go duration syntax such as
// "24h"; numeric values are seconds. Set distinguishes an explicit zero so
// each caller can apply its own zero-value semantics.
type Duration struct {
	Duration time.Duration
	Set      bool
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	d.Set = true
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		return d.setString(s)
	}
	var n json.Number
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("duration must be a string like \"24h\" or a number of seconds")
	}
	seconds, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil {
		return fmt.Errorf("duration seconds must be an integer: %w", err)
	}
	if seconds < 0 {
		return fmt.Errorf("duration must be non-negative")
	}
	d.Duration = time.Duration(seconds) * time.Second
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	if !d.Set {
		return []byte("null"), nil
	}
	return json.Marshal(d.Duration.String())
}

func (d Duration) IsZero() bool {
	return !d.Set
}

func (d *Duration) setString(s string) error {
	s = strings.TrimSpace(s)
	if s == "0" {
		d.Duration = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if v < 0 {
		return fmt.Errorf("duration must be non-negative")
	}
	d.Duration = v
	return nil
}
