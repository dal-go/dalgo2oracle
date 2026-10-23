package dalgo2oracle

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/sijms/go-ora/v3/network"
)

// leakyErr is a transport failure whose text names a secret, as go-ora's do.
var leakyErr = &leakyNetErr{}

type leakyNetErr struct{}

func (*leakyNetErr) Error() string {
	return "dial tcp secret-host:1521: connection refused (user scott/tiger)"
}
func (*leakyNetErr) Timeout() bool   { return false }
func (*leakyNetErr) Temporary() bool { return false }

// ---- fakes: "bare" implements only the required interface, "full" every optional one ----

type fakeResult struct{}

func (fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (fakeResult) RowsAffected() (int64, error) { return 0, nil }

type bareRows struct {
	nextErr  error
	closeErr error
}

func (r *bareRows) Columns() []string              { return []string{"a"} }
func (r *bareRows) Close() error                   { return r.closeErr }
func (r *bareRows) Next(dest []driver.Value) error { return r.nextErr }

type fullRows struct {
	bareRows
	nextSetErr error
}

func (r *fullRows) ColumnTypeDatabaseTypeName(int) string             { return "NUMBER" }
func (r *fullRows) ColumnTypeLength(int) (int64, bool)                { return 7, true }
func (r *fullRows) ColumnTypeNullable(int) (bool, bool)               { return true, true }
func (r *fullRows) ColumnTypePrecisionScale(int) (int64, int64, bool) { return 10, 2, true }
func (r *fullRows) ColumnTypeScanType(int) reflect.Type               { return reflect.TypeOf(int64(0)) }
func (r *fullRows) HasNextResultSet() bool                            { return true }
func (r *fullRows) NextResultSet() error                              { return r.nextSetErr }

type bareStmt struct {
	err  error
	rows driver.Rows
}

func (s *bareStmt) Close() error  { return s.err }
func (s *bareStmt) NumInput() int { return -1 }
func (s *bareStmt) Exec([]driver.Value) (driver.Result, error) {
	if s.err != nil {
		return nil, s.err
	}
	return fakeResult{}, nil
}
func (s *bareStmt) Query([]driver.Value) (driver.Rows, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

type fullStmt struct{ bareStmt }

func (s *fullStmt) ExecContext(context.Context, []driver.NamedValue) (driver.Result, error) {
	return s.Exec(nil)
}
func (s *fullStmt) QueryContext(context.Context, []driver.NamedValue) (driver.Rows, error) {
	return s.Query(nil)
}
func (s *fullStmt) CheckNamedValue(*driver.NamedValue) error { return errors.New("checked") }

type fakeTx struct {
	err        error
	rolledBack bool
}

func (t *fakeTx) Commit() error   { return t.err }
func (t *fakeTx) Rollback() error { t.rolledBack = true; return t.err }

type bareConn struct {
	err      error
	stmt     driver.Stmt
	tx       *fakeTx
	closeErr error
}

func (c *bareConn) Prepare(string) (driver.Stmt, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.stmt, nil
}
func (c *bareConn) Close() error { return c.closeErr }
func (c *bareConn) Begin() (driver.Tx, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.tx, nil
}

type fullConn struct {
	bareConn
	execErr  error
	execs    []string
	beginOpt driver.TxOptions
	valid    bool
}

func (c *fullConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.beginOpt = opts
	return c.Begin()
}
func (c *fullConn) PrepareContext(_ context.Context, q string) (driver.Stmt, error) {
	return c.Prepare(q)
}
func (c *fullConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	if c.err != nil {
		return nil, c.err
	}
	return &bareRows{}, nil
}
func (c *fullConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.execs = append(c.execs, q)
	if c.execErr != nil {
		return nil, c.execErr
	}
	return fakeResult{}, nil
}
func (c *fullConn) Ping(context.Context) error               { return c.err }
func (c *fullConn) ResetSession(context.Context) error       { return c.err }
func (c *fullConn) IsValid() bool                            { return c.valid }
func (c *fullConn) CheckNamedValue(*driver.NamedValue) error { return errors.New("checked") }

type fakeConnector struct {
	conn driver.Conn
	err  error
}

func (f *fakeConnector) Connect(context.Context) (driver.Conn, error) { return f.conn, f.err }
func (f *fakeConnector) Driver() driver.Driver                        { return nil }

func assertNoSecret(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, s := range []string{"secret", "scott", "tiger", "1521"} {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("error %q leaks %q", err, s)
		}
	}
}

// ---- tests ----

func TestNewConnector_DefaultSeam(t *testing.T) {
	c := NewConnector("oracle://u:p@localhost:1521/FREEPDB1")
	if c.Driver() == nil {
		t.Fatal("want go-ora's driver")
	}
}

func TestConnector_Connect(t *testing.T) {
	ok := &connector{inner: &fakeConnector{conn: &bareConn{}}}
	c, err := ok.Connect(context.Background())
	if err != nil || c.(*conn).Conn == nil {
		t.Fatalf("Connect: %v", err)
	}
	bad := &connector{inner: &fakeConnector{err: leakyErr}}
	_, err = bad.Connect(context.Background())
	assertNoSecret(t, err)
	var ce *ConnectionError
	if !errors.As(err, &ce) || ce.Kind != FailureNetwork || ce.op != "connect" {
		t.Fatalf("want a network ConnectionError, got %#v", err)
	}
	if bad.Driver() != nil {
		t.Fatal("Driver delegates")
	}
}

func TestConn_BeginTx(t *testing.T) {
	ctx := context.Background()
	t.Run("read-only over ConnBeginTx", func(t *testing.T) {
		fc := &fullConn{bareConn: bareConn{tx: &fakeTx{}}}
		tx, err := (&conn{Conn: fc}).BeginTx(ctx, driver.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if fc.beginOpt.ReadOnly {
			t.Fatal("go-ora must not see ReadOnly")
		}
		if len(fc.execs) != 1 || fc.execs[0] != setTransactionReadOnly {
			t.Fatalf("execs %v", fc.execs)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if err = tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("read-only statement fails", func(t *testing.T) {
		ftx := &fakeTx{}
		fc := &fullConn{bareConn: bareConn{tx: ftx}, execErr: leakyErr}
		_, err := (&conn{Conn: fc}).BeginTx(ctx, driver.TxOptions{ReadOnly: true})
		assertNoSecret(t, err)
		if !ftx.rolledBack {
			t.Fatal("want rollback")
		}
	})
	t.Run("read-only over a bare conn uses a prepared statement", func(t *testing.T) {
		bc := &bareConn{tx: &fakeTx{}, stmt: &bareStmt{}}
		if _, err := (&conn{Conn: bc}).BeginTx(ctx, driver.TxOptions{ReadOnly: true}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("bare conn prepare fails", func(t *testing.T) {
		bc := &bareConn{tx: &fakeTx{}}
		c := &conn{Conn: bc}
		tx, err := c.Begin()
		if err != nil {
			t.Fatal(err)
		}
		_ = tx
		bc.err = leakyErr
		if err = c.execNoArgs(ctx, "X"); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("bare conn refuses an isolation level", func(t *testing.T) {
		_, err := (&conn{Conn: &bareConn{}}).BeginTx(ctx, driver.TxOptions{Isolation: 1})
		if !errors.Is(err, errIsolation) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("begin fails", func(t *testing.T) {
		_, err := (&conn{Conn: &bareConn{err: leakyErr}}).BeginTx(ctx, driver.TxOptions{})
		assertNoSecret(t, err)
	})
	t.Run("commit and rollback errors are sanitized", func(t *testing.T) {
		tx := &tx_{Tx: &fakeTx{err: leakyErr}}
		assertNoSecret(t, tx.Commit())
		assertNoSecret(t, tx.Rollback())
	})
}

func TestConn_Delegation(t *testing.T) {
	ctx := context.Background()
	full := &conn{Conn: &fullConn{bareConn: bareConn{stmt: &bareStmt{}}, valid: false}}
	bare := &conn{Conn: &bareConn{stmt: &bareStmt{}}}

	if _, err := full.Prepare("q"); err != nil {
		t.Fatal(err)
	}
	if _, err := bare.PrepareContext(ctx, "q"); err != nil {
		t.Fatal(err)
	}
	if _, err := (&conn{Conn: &bareConn{err: leakyErr}}).Prepare("q"); err == nil {
		t.Fatal("want error")
	}
	if r, err := full.QueryContext(ctx, "q", nil); err != nil || r == nil {
		t.Fatal(err)
	}
	if _, err := bare.QueryContext(ctx, "q", nil); !errors.Is(err, driver.ErrSkip) {
		t.Fatal(err)
	}
	if _, err := bare.ExecContext(ctx, "q", nil); !errors.Is(err, driver.ErrSkip) {
		t.Fatal(err)
	}
	failing := &conn{Conn: &fullConn{bareConn: bareConn{err: leakyErr}, execErr: leakyErr}}
	_, err := failing.QueryContext(ctx, "q", nil)
	assertNoSecret(t, err)
	_, err = failing.ExecContext(ctx, "q", nil)
	assertNoSecret(t, err)
	assertNoSecret(t, failing.Ping(ctx))
	assertNoSecret(t, failing.ResetSession(ctx))
	if full.Ping(ctx) != nil || bare.Ping(ctx) != nil || full.ResetSession(ctx) != nil || bare.ResetSession(ctx) != nil {
		t.Fatal("ping/reset")
	}
	if full.IsValid() || !bare.IsValid() {
		t.Fatal("IsValid")
	}
	if full.CheckNamedValue(nil) == nil || !errors.Is(bare.CheckNamedValue(nil), driver.ErrSkip) {
		t.Fatal("CheckNamedValue")
	}
	if full.Close() != nil {
		t.Fatal("close")
	}
	assertNoSecret(t, (&conn{Conn: &bareConn{closeErr: leakyErr}}).Close())
}

func TestStmt(t *testing.T) {
	ctx := context.Background()
	named := []driver.NamedValue{{Name: "n", Ordinal: 1, Value: 1}}
	positional := []driver.NamedValue{{Ordinal: 1, Value: 1}}

	bare := &stmt_{Stmt: &bareStmt{rows: &bareRows{}}}
	if _, err := bare.ExecContext(ctx, positional); err != nil {
		t.Fatal(err)
	}
	if _, err := bare.ExecContext(ctx, named); !errors.Is(err, errNamedArgs) {
		t.Fatal(err)
	}
	if _, err := bare.QueryContext(ctx, positional); err != nil {
		t.Fatal(err)
	}
	if _, err := bare.QueryContext(ctx, named); !errors.Is(err, errNamedArgs) {
		t.Fatal(err)
	}
	if !errors.Is(bare.CheckNamedValue(nil), driver.ErrSkip) || bare.Close() != nil {
		t.Fatal("bare stmt")
	}

	full := &stmt_{Stmt: &fullStmt{bareStmt{rows: &bareRows{}}}}
	if _, err := full.ExecContext(ctx, named); err != nil {
		t.Fatal(err)
	}
	if _, err := full.QueryContext(ctx, named); err != nil {
		t.Fatal(err)
	}
	if full.CheckNamedValue(nil) == nil {
		t.Fatal("CheckNamedValue delegates")
	}

	failing := &stmt_{Stmt: &fullStmt{bareStmt{err: leakyErr}}}
	_, err := failing.ExecContext(ctx, nil)
	assertNoSecret(t, err)
	_, err = failing.QueryContext(ctx, nil)
	assertNoSecret(t, err)
	_, err = failing.Query(nil)
	assertNoSecret(t, err)
	assertNoSecret(t, failing.Close())
}

func TestRows(t *testing.T) {
	bare := &rows{Rows: &bareRows{nextErr: io.EOF}}
	if bare.Next(nil) != io.EOF || bare.NextResultSet() != io.EOF || bare.HasNextResultSet() {
		t.Fatal("bare rows end")
	}
	if bare.ColumnTypeDatabaseTypeName(0) != "" || bare.ColumnTypeScanType(0) != anyType {
		t.Fatal("bare names")
	}
	if n, ok := bare.ColumnTypeLength(0); n != 0 || ok {
		t.Fatal("length")
	}
	if n, ok := bare.ColumnTypeNullable(0); n || ok {
		t.Fatal("nullable")
	}
	if p, s, ok := bare.ColumnTypePrecisionScale(0); p != 0 || s != 0 || ok {
		t.Fatal("precision")
	}
	if bare.Close() != nil {
		t.Fatal("close")
	}

	full := &rows{Rows: &fullRows{bareRows: bareRows{nextErr: leakyErr, closeErr: leakyErr}, nextSetErr: leakyErr}}
	assertNoSecret(t, full.Next(nil))
	assertNoSecret(t, full.NextResultSet())
	assertNoSecret(t, full.Close())
	if full.ColumnTypeDatabaseTypeName(0) != "NUMBER" || full.ColumnTypeScanType(0) != reflect.TypeOf(int64(0)) || !full.HasNextResultSet() {
		t.Fatal("full names")
	}
	if n, ok := full.ColumnTypeLength(0); n != 7 || !ok {
		t.Fatal("length")
	}
	if n, ok := full.ColumnTypeNullable(0); !n || !ok {
		t.Fatal("nullable")
	}
	if p, s, ok := full.ColumnTypePrecisionScale(0); p != 10 || s != 2 || !ok {
		t.Fatal("precision")
	}
	if (&rows{Rows: &fullRows{nextSetErr: io.EOF}}).NextResultSet() != io.EOF {
		t.Fatal("EOF")
	}
}

func TestSanitize(t *testing.T) {
	ora := network.NewOracleError(942)
	cases := []struct {
		name string
		in   error
		want func(error) bool
	}{
		{"nil", nil, func(e error) bool { return e == nil }},
		{"skip", driver.ErrSkip, func(e error) bool { return e == driver.ErrSkip }},
		{"bad conn", errors.Join(leakyErr, driver.ErrBadConn), func(e error) bool { return e == driver.ErrBadConn }},
		{"ora", ora, func(e error) bool { return e == error(ora) }},
		{"canceled", errors.Join(leakyErr, context.Canceled), func(e error) bool { return e == context.Canceled }},
		{"deadline", errors.Join(leakyErr, context.DeadlineExceeded), func(e error) bool { return e == context.DeadlineExceeded }},
		{"network", leakyErr, func(e error) bool { var ce *ConnectionError; return errors.As(e, &ce) && ce.Kind == FailureNetwork }},
		{"errno", syscall.ECONNRESET, func(e error) bool { var ce *ConnectionError; return errors.As(e, &ce) }},
		{"eof", io.ErrUnexpectedEOF, func(e error) bool { var ce *ConnectionError; return errors.As(e, &ce) }},
		{"other", errors.New("conversion"), func(e error) bool { return e.Error() == "conversion" }},
	}
	for _, c := range cases {
		if got := sanitize("op", c.in); !c.want(got) {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestWithFastLoginOff(t *testing.T) {
	cases := map[string]string{
		"oracle://u:p@h:1521/S":                   "oracle://u:p@h:1521/S?fast+login=false",
		"oracle://u:p@h:1521/S?":                  "oracle://u:p@h:1521/S?fast+login=false",
		"oracle://u:p@h:1521/S?TIMEOUT=5":         "oracle://u:p@h:1521/S?TIMEOUT=5&fast+login=false",
		"oracle://u:p@h:1521/S?FAST%20LOGIN=true": "oracle://u:p@h:1521/S?FAST%20LOGIN=true",
		"oracle://u:p@h:1521/S?fast+login=enable": "oracle://u:p@h:1521/S?fast+login=enable",
		"oracle://u:p@h:bad port/S":               "oracle://u:p@h:bad port/S",
	}
	for in, want := range cases {
		if got := withFastLoginOff(in); got != want {
			t.Errorf("withFastLoginOff(%q) = %q, want %q", in, got, want)
		}
	}
}
