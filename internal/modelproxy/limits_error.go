package modelproxy

import "fmt"

// Keep provider details in the report and the error chain, not a duplicate joined
// message whose newline separators would be stripped by plain-text rendering.
type limitsQueryError struct {
	failures []error
	total    int
}

func (e *limitsQueryError) Error() string {
	return fmt.Sprintf("%d of %d subscription quota queries failed; see provider results", len(e.failures), e.total)
}
func (e *limitsQueryError) Unwrap() []error { return e.failures }

func LimitsReportError(report LimitsReport) error {
	var failures []error
	for _, p := range report.Providers {
		if p.Error != nil {
			failures = append(failures, p.Error)
		}
	}
	if len(failures) > 0 {
		return &limitsQueryError{failures: failures, total: len(report.Providers)}
	}
	return nil
}
