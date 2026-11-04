package modelproxy

// UpstreamError distinguishes an operation that reached its provider from a
// local preparation/admission failure. The original error remains available
// through errors.Is/errors.As; HTTP adapters choose their own status mapping.
type UpstreamError struct{ Err error }

func (e *UpstreamError) Error() string { return e.Err.Error() }
func (e *UpstreamError) Unwrap() error { return e.Err }
