package cordis

import "fmt"

// ErrorCode is a stable, machine-readable framework error code.
type ErrorCode string

// Framework error codes.
const (
	// ErrInactiveEffect means a registration needs a live fiber or explicit
	// effect scope, but that lifetime is already inactive. Context.On,
	// Context.OnOnce, Context.OnValue, Context.OnWaterfall, Context.OnDispose and
	// Context.Effect panic with it. Context.Provide, Context.ProvideChecked,
	// Context.Serve, plugin loading, Restart and Update return it instead.
	ErrInactiveEffect ErrorCode = "INACTIVE_EFFECT"
	// ErrInvalidPlugin is returned when a value is not a usable plugin.
	ErrInvalidPlugin ErrorCode = "INVALID_PLUGIN"
	// ErrServiceExists is returned when a service name is already provided in
	// the same isolation scope.
	ErrServiceExists ErrorCode = "SERVICE_EXISTS"
	// ErrServiceMissing is returned whenever a service is required and cannot be
	// used: MustGet when the name is unregistered, its provider is inactive, or
	// the value has another type; Provide with an empty name; Set on a name that
	// is not provided, and Set whose provider was replaced mid-update.
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
//
// A nil *Error never matches, on either side of the comparison: errors.Is hands
// the target straight to this method and does not recover, so a caller that
// leaves an optional target unset, or that compares a nil error, must get "no
// match" instead of a panic.
func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return e != nil && ok && other != nil && other.Code == e.Code
}

func newError(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}
