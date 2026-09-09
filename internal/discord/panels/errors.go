package panels

import "errors"

var (
	ErrNotFound    = errors.New("panel message not found")
	ErrPermission  = errors.New("panel permission denied")
	ErrRateLimited = errors.New("panel rate limited")
)

type ErrorKind int

const (
	ErrorTransient ErrorKind = iota
	ErrorNotFoundKind
	ErrorPermissionKind
	ErrorRateLimitedKind
)

type ClassifiedError struct {
	Kind ErrorKind
	Err  error
}

func (e *ClassifiedError) Error() string { return e.Err.Error() }
func (e *ClassifiedError) Unwrap() error { return e.Err }
func Classify(err error) ErrorKind {
	if ce, ok := err.(*ClassifiedError); ok {
		return ce.Kind
	}
	var ce *ClassifiedError
	if errors.As(err, &ce) {
		return ce.Kind
	}
	return ErrorTransient
}
