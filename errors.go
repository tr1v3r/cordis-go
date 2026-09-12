package cordis

import "fmt"

// ErrorCode is a stable, machine-readable framework error code.
type ErrorCode string

// Framework error codes.
const (
	// ErrInactiveEffect is returned when an effect is created on a context
	// whose fiber has already been disposed or is unloading.
	ErrInactiveEffect ErrorCode = "INACTIVE_EFFECT"
	// ErrInvalidPlugin is returned when a value is not a usable plugin.
	ErrInvalidPlugin ErrorCode = "INVALID_PLUGIN"
	// ErrServiceExists is returned when a service name is already provided in
	// the same isolation scope.
	ErrServiceExists ErrorCode = "SERVICE_EXISTS"
	// ErrServiceMissing is returned when a required service is not available.
	ErrServiceMissing ErrorCode = "SERVICE_MISSING"
	// ErrServiceOwnership is returned when a fiber tries to mutate a service it
	// does not own.
	ErrServiceOwnership ErrorCode = "SERVICE_OWNERSHIP"
)

// Error is a framework error carrying a stable code.
type Error struct {
	Code ErrorCode
	Msg  string
}

// Error implements the error interface, rendering the code and message.
func (e *Error) Error() string {
	if e.Msg == "" {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}

// Is reports whether target carries the same code.
func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && other.Code == e.Code
}

func newError(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}
