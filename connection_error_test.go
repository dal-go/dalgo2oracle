package dalgo2oracle

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/sijms/go-ora/v3/network"
)

type timeoutNetErr struct{ leakyNetErr }

func (*timeoutNetErr) Timeout() bool { return true }

func TestClassify(t *testing.T) {
	cases := []struct {
		in   error
		kind FailureKind
		code int
	}{
		{network.NewOracleError(1017), FailureAuthentication, 1017},
		{network.NewOracleError(28000), FailureAuthentication, 28000},
		{network.NewOracleError(12514), FailureService, 12514},
		{network.NewOracleError(12505), FailureService, 12505},
		{network.NewOracleError(12170), FailureTimeout, 12170},
		{network.NewOracleError(12541), FailureNetwork, 12541},
		{network.NewOracleError(600), FailureServer, 600},
		{context.DeadlineExceeded, FailureTimeout, 0},
		{os.ErrDeadlineExceeded, FailureTimeout, 0},
		{&timeoutNetErr{}, FailureTimeout, 0},
		{leakyErr, FailureNetwork, 0},
		{syscall.ECONNREFUSED, FailureNetwork, 0},
		{errors.New("x"), FailureOther, 0},
	}
	for _, c := range cases {
		kind, code := classify(c.in)
		if kind != c.kind || code != c.code {
			t.Errorf("classify(%v) = %v, %d; want %v, %d", c.in, kind, code, c.kind, c.code)
		}
	}
}

func TestConnectionError_Text(t *testing.T) {
	e := newConnectionError("connect", network.NewOracleError(1017))
	if got, want := e.Error(), "dalgo2oracle: connect: the server refused the credentials or the account (ORA-01017)"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := (&ConnectionError{Kind: FailureKind(99)}).Error(); got != "dalgo2oracle: the connection failed" {
		t.Fatalf("got %q", got)
	}
	if FailureKind(-1).String() != FailureOther.String() {
		t.Fatal("negative kind")
	}
	if errors.Unwrap(e) != nil {
		t.Fatal("the driver error must not be reachable")
	}
}

func TestConnectionError_Is(t *testing.T) {
	canceled := newConnectionError("ping", context.Canceled)
	timedOut := newConnectionError("ping", context.DeadlineExceeded)
	if !errors.Is(canceled, context.Canceled) || errors.Is(canceled, context.DeadlineExceeded) {
		t.Fatal("canceled")
	}
	if !errors.Is(timedOut, context.DeadlineExceeded) || errors.Is(timedOut, context.Canceled) {
		t.Fatal("timed out")
	}
}
