package workflow

type Result[T any] struct {
	Error error
	Data  T
}

// IsRetryableError returns whether the Step error is a temporary
// failure which could be overcome by resubmitting the step but needs
func IsRetryableError[T any](r Result[T], err error) bool {
	return err != nil && r.Error == nil
}

func IsFatalError[T any](r Result[T], err error) bool {
	return err == nil && r.Error != nil
}
