package presencebroker

import "errors"

// wrapOpaque returns an error whose Error() text is exactly message, with
// cause reachable only through Unwrap. Use it for any raw OS error that
// might embed a filesystem path — a snapshot destination, a temp file
// name, a directory — so this package never leaks a path through a log
// line (see snapshot.go, whose *fs.PathError/*os.LinkError-returning calls
// all go through this). Mirrors producerprotocol's own wrapOpaque/
// wrappedError; kept as a separate, private copy here rather than an
// exported cross-package helper, since neither package should depend on
// the other's error internals.
func wrapOpaque(message string, cause error) error {
	if cause == nil {
		return nil
	}
	return &wrappedError{sentinel: errors.New(message), cause: cause}
}

// wrappedError lets wrapOpaque attach a sentinel (via errors.Is) while
// preserving the original cause for Unwrap, without ever formatting the
// cause into the sentinel's own message: cause may be a *fs.PathError or
// *os.LinkError carrying a filesystem path, and Error() must stay
// content-free regardless of what it wraps.
type wrappedError struct {
	sentinel error
	cause    error
}

func (err *wrappedError) Error() string        { return err.sentinel.Error() }
func (err *wrappedError) Is(target error) bool { return target == err.sentinel }
func (err *wrappedError) Unwrap() error        { return err.cause }
