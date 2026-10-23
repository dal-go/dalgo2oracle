package dalgo2oracle

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net/url"
	"reflect"
	"strings"

	goora "github.com/sijms/go-ora/v3"
)

// setTransactionReadOnly is the first statement of a read-only transaction.
const setTransactionReadOnly = "SET TRANSACTION READ ONLY"

// newOracleConnector is the go-ora connector constructor, a seam so tests run
// without a server. go-ora parses the string only when it connects.
var newOracleConnector = func(dsn string) driver.Connector { return goora.NewConnector(dsn) }

// NewConnector returns a driver.Connector for dsn (oracle://user:pass@host:port/service)
// that a caller can open with sql.OpenDB for native SQL. It differs from go-ora's
// own connector in two ways:
//
//   - A transaction begun with ReadOnly (sql.TxOptions{ReadOnly: true}) is
//     accepted: go-ora refuses one, so it is begun as a plain transaction whose
//     first statement is SET TRANSACTION READ ONLY. Oracle then refuses every
//     write in it (ORA-01456).
//   - Errors never carry the connection string or the server's address: a
//     failure to connect is a *ConnectionError, and so is a transport failure
//     later on (see [ConnectionError]).
//
// It also turns go-ora's "fast login" off unless dsn sets it: with it on,
// go-ora v3.0.1 caches a negotiation cookie per service, and against Oracle 23ai
// Free every later connection of the process then ends in EOF, so a pool cannot
// open a second connection.
func NewConnector(dsn string) driver.Connector {
	return &connector{inner: newOracleConnector(withFastLoginOff(dsn))}
}

// withFastLoginOff adds fast login=false to a URL that does not set the option.
// A string that is not a URL is returned unchanged.
func withFastLoginOff(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	for key := range u.Query() {
		if strings.EqualFold(strings.TrimSpace(key), "fast login") {
			return dsn
		}
	}
	const option = "fast+login=false"
	switch {
	case strings.HasSuffix(dsn, "?"):
		return dsn + option
	case u.RawQuery == "":
		return dsn + "?" + option
	}
	return dsn + "&" + option
}

type connector struct{ inner driver.Connector }

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	inner, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, newConnectionError("connect", err)
	}
	return &conn{Conn: inner}, nil
}

func (c *connector) Driver() driver.Driver { return c.inner.Driver() }

// conn wraps a go-ora connection. Every optional interface database/sql looks
// for is implemented here and delegated, with database/sql's own fallback when
// the wrapped connection lacks it.
type conn struct{ driver.Conn }

var (
	_ driver.ConnBeginTx        = (*conn)(nil)
	_ driver.ConnPrepareContext = (*conn)(nil)
	_ driver.QueryerContext     = (*conn)(nil)
	_ driver.ExecerContext      = (*conn)(nil)
	_ driver.Pinger             = (*conn)(nil)
	_ driver.SessionResetter    = (*conn)(nil)
	_ driver.Validator          = (*conn)(nil)
	_ driver.NamedValueChecker  = (*conn)(nil)
)

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *conn) Close() error { return sanitize("close", c.Conn.Close()) }

func (c *conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// errIsolation is go-ora's refusal of an isolation level, for a wrapped
// connection that cannot take options at all.
var errIsolation = errors.New("dalgo2oracle: only the default isolation level is supported")

// BeginTx begins a transaction. A read-only one is begun as a plain transaction
// whose first statement is SET TRANSACTION READ ONLY.
func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	readOnly := opts.ReadOnly
	opts.ReadOnly = false
	var tx driver.Tx
	var err error
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		tx, err = b.BeginTx(ctx, opts)
	} else if opts.Isolation != driver.IsolationLevel(0) {
		return nil, errIsolation
	} else {
		tx, err = c.Conn.Begin() //nolint:staticcheck // the fallback database/sql itself uses
	}
	if err != nil {
		return nil, sanitize("begin", err)
	}
	if readOnly {
		if err = c.execNoArgs(ctx, setTransactionReadOnly); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	}
	return &tx_{Tx: tx}, nil
}

// execNoArgs runs a statement with no arguments, through ExecerContext when the
// wrapped connection has it and through a prepared statement when not.
func (c *conn) execNoArgs(ctx context.Context, query string) error {
	_, err := c.ExecContext(ctx, query, nil)
	if !errors.Is(err, driver.ErrSkip) {
		return err
	}
	stmt, err := c.PrepareContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	_, err = stmt.(*stmt_).ExecContext(ctx, nil)
	return err
}

func (c *conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	var s driver.Stmt
	var err error
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		s, err = p.PrepareContext(ctx, query)
	} else {
		s, err = c.Conn.Prepare(query)
	}
	if err != nil {
		return nil, sanitize("prepare", err)
	}
	return &stmt_{Stmt: s}, nil
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	r, err := q.QueryContext(ctx, query, args)
	if err != nil {
		return nil, sanitize("query", err)
	}
	return &rows{Rows: r}, nil
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	res, err := e.ExecContext(ctx, query, args)
	if err != nil {
		return nil, sanitize("exec", err)
	}
	return res, nil
}

func (c *conn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return sanitize("ping", p.Ping(ctx))
	}
	return nil
}

func (c *conn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return sanitize("reset session", r.ResetSession(ctx))
	}
	return nil
}

func (c *conn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *conn) CheckNamedValue(nv *driver.NamedValue) error {
	if n, ok := c.Conn.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

// tx_ sanitizes the errors of the end of a transaction.
type tx_ struct{ driver.Tx }

func (t *tx_) Commit() error   { return sanitize("commit", t.Tx.Commit()) }
func (t *tx_) Rollback() error { return sanitize("rollback", t.Tx.Rollback()) }

// stmt_ wraps a prepared statement.
type stmt_ struct{ driver.Stmt }

var (
	_ driver.StmtExecContext   = (*stmt_)(nil)
	_ driver.StmtQueryContext  = (*stmt_)(nil)
	_ driver.NamedValueChecker = (*stmt_)(nil)
)

// errNamedArgs is database/sql's refusal of a named argument for a statement
// that cannot take one.
var errNamedArgs = errors.New("dalgo2oracle: driver does not support the use of named parameters")

func values(args []driver.NamedValue) ([]driver.Value, error) {
	out := make([]driver.Value, len(args))
	for i, a := range args {
		if a.Name != "" {
			return nil, errNamedArgs
		}
		out[i] = a.Value
	}
	return out, nil
}

func (s *stmt_) Close() error { return sanitize("close statement", s.Stmt.Close()) }

func (s *stmt_) Exec(args []driver.Value) (driver.Result, error) {
	res, err := s.Stmt.Exec(args) //nolint:staticcheck // delegated
	return res, sanitize("exec", err)
}

func (s *stmt_) Query(args []driver.Value) (driver.Rows, error) {
	r, err := s.Stmt.Query(args) //nolint:staticcheck // delegated
	if err != nil {
		return nil, sanitize("query", err)
	}
	return &rows{Rows: r}, nil
}

func (s *stmt_) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if e, ok := s.Stmt.(driver.StmtExecContext); ok {
		res, err := e.ExecContext(ctx, args)
		return res, sanitize("exec", err)
	}
	v, err := values(args)
	if err != nil {
		return nil, err
	}
	return s.Exec(v)
}

func (s *stmt_) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := s.Stmt.(driver.StmtQueryContext)
	if !ok {
		v, err := values(args)
		if err != nil {
			return nil, err
		}
		return s.Query(v)
	}
	r, err := q.QueryContext(ctx, args)
	if err != nil {
		return nil, sanitize("query", err)
	}
	return &rows{Rows: r}, nil
}

func (s *stmt_) CheckNamedValue(nv *driver.NamedValue) error {
	if n, ok := s.Stmt.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

// rows wraps a result set: a fetch can fail on the transport.
type rows struct{ driver.Rows }

var (
	_ driver.RowsColumnTypeDatabaseTypeName = (*rows)(nil)
	_ driver.RowsColumnTypeLength           = (*rows)(nil)
	_ driver.RowsColumnTypeNullable         = (*rows)(nil)
	_ driver.RowsColumnTypePrecisionScale   = (*rows)(nil)
	_ driver.RowsColumnTypeScanType         = (*rows)(nil)
	_ driver.RowsNextResultSet              = (*rows)(nil)
)

func (r *rows) Close() error { return sanitize("close rows", r.Rows.Close()) }

func (r *rows) Next(dest []driver.Value) error {
	return endOrSanitized("fetch", r.Rows.Next(dest))
}

func (r *rows) ColumnTypeDatabaseTypeName(index int) string {
	if t, ok := r.Rows.(driver.RowsColumnTypeDatabaseTypeName); ok {
		return t.ColumnTypeDatabaseTypeName(index)
	}
	return ""
}

func (r *rows) ColumnTypeLength(index int) (int64, bool) {
	if t, ok := r.Rows.(driver.RowsColumnTypeLength); ok {
		return t.ColumnTypeLength(index)
	}
	return 0, false
}

func (r *rows) ColumnTypeNullable(index int) (bool, bool) {
	if t, ok := r.Rows.(driver.RowsColumnTypeNullable); ok {
		return t.ColumnTypeNullable(index)
	}
	return false, false
}

func (r *rows) ColumnTypePrecisionScale(index int) (int64, int64, bool) {
	if t, ok := r.Rows.(driver.RowsColumnTypePrecisionScale); ok {
		return t.ColumnTypePrecisionScale(index)
	}
	return 0, 0, false
}

var anyType = reflect.TypeOf(new(any)).Elem()

func (r *rows) ColumnTypeScanType(index int) reflect.Type {
	if t, ok := r.Rows.(driver.RowsColumnTypeScanType); ok {
		return t.ColumnTypeScanType(index)
	}
	return anyType
}

func (r *rows) HasNextResultSet() bool {
	if n, ok := r.Rows.(driver.RowsNextResultSet); ok {
		return n.HasNextResultSet()
	}
	return false
}

func (r *rows) NextResultSet() error {
	if n, ok := r.Rows.(driver.RowsNextResultSet); ok {
		return endOrSanitized("next result set", n.NextResultSet())
	}
	return io.EOF
}

// endOrSanitized returns io.EOF, the driver contract's end of a sequence, bare,
// and sanitizes any other error.
func endOrSanitized(op string, err error) error {
	if err == io.EOF { //nolint:errorlint // the driver contract's own sentinel, returned bare
		return io.EOF
	}
	return sanitize(op, err)
}
