package step

// Result is a wrapper around the result of a step; it provides a way to
// distinguish between transient and fatal errors.
type Result[T any] struct {
	Error error
	Data  T
}

// IsRetryableError returns whether the Step error is a temporary
// failure which could be overcome by resubmitting the step but needs
// to be retried.
func IsRetryableError[T any](r Result[T], err error) bool {
	return err != nil && r.Error == nil
}

// IsFatalError returns whether the Step error is a permanent
// failure which cannot be overcome by resubmitting the step.
func IsFatalError[T any](r Result[T], err error) bool {
	return err == nil && r.Error != nil
}
