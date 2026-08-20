package s3gw

import "fmt"

// PanicError is the cause an observer is given when a request panicked —
// in one of the service's own hooks, or in the gateway itself.
//
// The gateway recovers at the request boundary rather than leaving it to
// net/http, which would answer the client with a dropped connection and
// write the panic to its own ErrorLog, outside everything the service
// installed. A recovered panic is an InternalError to the client and an
// ordinary failure to the observer, so it lands in the same log line, under
// the same x-amz-request-id, as every other failure.
//
// Only the message is rendered by RequestInfo's JSON, since a stack does not
// belong in an access log line by default. A service that wants it asks for
// it:
//
//	var pe *s3gw.PanicError
//	if errors.As(info.Err, &pe) {
//		slog.Error("panic", "value", pe.Value, "stack", string(pe.Stack))
//	}
type PanicError struct {
	// Value is what was passed to panic.
	Value any
	// Stack is the stack captured where the panic was recovered, which
	// includes the frames that panicked.
	Stack []byte
}

func (e *PanicError) Error() string { return fmt.Sprintf("panic: %v", e.Value) }
