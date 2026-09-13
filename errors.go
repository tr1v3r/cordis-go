package cordis

import "fmt"

// ErrorCode is a stable, machine-readable framework error code.
type ErrorCode string

// Framework error codes.
const (
	// ErrInactiveEffect means the operation needs a live fiber but the fiber is
	// already disposed or is unloading. The registration helpers that go through
	// fiber.effect - ctx.On, ctx.OnOnce, ctx.OnValue, ctx.OnWaterfall,
	// ctx.OnDispose and ctx.Effect - panic with it, because a plugin body calls
	// them. ctx.Provide, ctx.ProvideChecked and ctx.Serve return it instead: the
	// owner was already inactive, or the effect registration lost a race with the
	// fiber going inactive, in which case the binding it just registered is rolled
	// back. Restart and Update return it too.
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
func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && other.Code == e.Code
}

func newError(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}
