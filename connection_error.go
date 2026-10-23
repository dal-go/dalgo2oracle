package dalgo2oracle

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/sijms/go-ora/v3/network"
)

// FailureKind says what kind of failure a [ConnectionError] reports. It decides
// the fixed sentence the error's text uses.
type FailureKind int

const (
	// FailureOther is any failure the kinds below do not describe. It is the zero
	// value.
	FailureOther FailureKind = iota
	// FailureInvalidDSN: the driver cannot parse the connection string.
	FailureInvalidDSN
	// FailureNetwork: the server could not be reached, or the connection to it
	// was lost (name resolution, a refused or reset connection, the end of the
	// stream).
	FailureNetwork
	// FailureTimeout: the attempt timed out or was canceled.
	FailureTimeout
	// FailureAuthentication: the server refused the credentials or the account
	// (ORA-01017, ORA-28000, ORA-28001, ORA-01045).
	FailureAuthentication
	// FailureService: the listener does not know the requested service or SID
	// (ORA-12514, ORA-12505).
	FailureService
	// FailureServer: the server answered with another error. [ConnectionError.Code]
	// holds its ORA number.
	FailureServer
)

var kindSentences = [...]string{
	FailureOther:          "the connection failed",
	FailureInvalidDSN:     "the connection string cannot be parsed: use oracle://user:password@host:port/service with special characters percent-encoded",
	FailureNetwork:        "the server could not be reached or the connection was lost",
	FailureTimeout:        "the connection timed out or was canceled",
	FailureAuthentication: "the server refused the credentials or the account",
	FailureService:        "the listener does not know the requested service",
	FailureServer:         "the server rejected the connection",
}

// String returns the fixed sentence of the kind.
func (k FailureKind) String() string {
	if k < 0 || int(k) >= len(kindSentences) {
		k = FailureOther
	}
	return kindSentences[k]
}

// ConnectionError is the error a connection that fails is reported as: by
// [NewDatabase] when the connection to Oracle cannot be opened or verified, and
// by any later call whose connection fails (the pool connecting again, a socket
// that is reset or times out).
//
// Its text is built only from a fixed sentence chosen by Kind and the ORA
// number in Code. Nothing of the connection string, and nothing of the driver's
// or the server's own message, is ever copied into it: go-ora and the listener
// put the user, host, port and service name into theirs, and callers print
// errors. For the same reason the driver's error is not reachable through
// [errors.Unwrap], [errors.Is] or [errors.As]; branch on Kind and Code instead.
// The one thing kept of it is whether the attempt ended because its context was
// canceled or its deadline passed: [errors.Is] finds [context.Canceled] and
// [context.DeadlineExceeded] in such an error.
type ConnectionError struct {
	// Kind is what failed.
	Kind FailureKind
	// Code is the ORA number of a server error, for example 1017 for ORA-01017;
	// zero when the server sent none.
	Code int

	op       string
	canceled bool
	timedOut bool
}

// Error returns the text: the step, the fixed sentence and the ORA number only.
func (e *ConnectionError) Error() string {
	var b strings.Builder
	b.WriteString("dalgo2oracle: ")
	if e.op != "" {
		b.WriteString(e.op + ": ")
	}
	b.WriteString(e.Kind.String())
	if e.Code != 0 {
		b.WriteString(" (ORA-" + leftPad(strconv.Itoa(e.Code), 5) + ")")
	}
	return b.String()
}

// Is reports the two context errors of the attempt that failed, and nothing else.
func (e *ConnectionError) Is(target error) bool {
	return (e.canceled && target == context.Canceled) || (e.timedOut && target == context.DeadlineExceeded) //nolint:errorlint // the sentinels themselves
}

func leftPad(s string, n int) string {
	for len(s) < n {
		s = "0" + s
	}
	return s
}

// newConnectionError builds the error for a failure of op from the types in
// cause's chain. The cause itself goes no further.
func newConnectionError(op string, cause error) *ConnectionError {
	kind, code := classify(cause)
	return &ConnectionError{
		Kind:     kind,
		Code:     code,
		op:       op,
		canceled: errors.Is(cause, context.Canceled),
		timedOut: errors.Is(cause, context.DeadlineExceeded),
	}
}

// classify decides the kind of a failure from the types in its chain, never from
// its text.
func classify(cause error) (FailureKind, int) {
	var oraErr *network.OracleError
	if errors.As(cause, &oraErr) {
		switch oraErr.ErrCode {
		case 1017, 1045, 28000, 28001:
			return FailureAuthentication, oraErr.ErrCode
		case 12505, 12514:
			return FailureService, oraErr.ErrCode
		case 12170:
			return FailureTimeout, oraErr.ErrCode
		case 12541, 12543, 12545, 3113, 3135:
			return FailureNetwork, oraErr.ErrCode
		}
		return FailureServer, oraErr.ErrCode
	}
	switch {
	case isTimeout(cause):
		return FailureTimeout, 0
	case isNetwork(cause):
		return FailureNetwork, 0
	}
	return FailureOther, 0
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// isNetwork reports a failure of the transport: an error of package net, a
// system call's error number, or the end of the stream.
func isNetwork(err error) bool {
	var netErr net.Error
	var errno syscall.Errno
	return errors.As(err, &netErr) || errors.As(err, &errno) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// sanitize is applied to every error the driver returns after the connection is
// open. A server error about a statement (an *network.OracleError, such as
// ORA-00942) and the driver's own errors pass unchanged, since callers need
// them. database/sql's sentinels and context errors are returned bare. A failure
// of the transport, whose text names the server's address, becomes a
// *ConnectionError.
func sanitize(op string, err error) error {
	if err == nil {
		return nil
	}
	var oraErr *network.OracleError
	switch {
	case errors.Is(err, driver.ErrSkip):
		return driver.ErrSkip
	case errors.Is(err, driver.ErrBadConn):
		return driver.ErrBadConn
	case errors.As(err, &oraErr):
		return err
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case isNetwork(err):
		return newConnectionError(op, err)
	}
	return err
}
