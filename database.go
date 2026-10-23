// Package dalgo2oracle is the Oracle Database adapter of DALgo.
//
// It opens the server with github.com/sijms/go-ora/v3 (pure Go), composes
// github.com/dal-go/dalgo2sql for the dal.DB surface with the structured-query
// dialect "oracle", and adds:
//
//   - dbschema.SchemaReader over Oracle's ALL_ catalog views, scoped to one
//     schema (the session's CURRENT_SCHEMA unless [WithSchema] names another),
//     plus ListViews, which tells a caller which collections are views;
//   - read-only transactions, which go-ora refuses: see [NewConnector];
//   - errors that never carry the connection string, the user, the host or the
//     service: see [ConnectionError].
package dalgo2oracle

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
	goora "github.com/sijms/go-ora/v3"
	"github.com/sijms/go-ora/v3/network"
)

// Version is the adapter version reported by Database.Adapter.
const Version = "0.1.0"

// StructuredQueryDialect is the dalgo2sql dialect the adapter sets.
const StructuredQueryDialect = "oracle"

// currentSchemaSQL reads the schema unqualified names resolve in.
const currentSchemaSQL = `SELECT SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA') FROM DUAL`

// Seams for tests.
var (
	// parseDSN checks the connection string the way go-ora reads it, before any
	// connection is attempted.
	parseDSN = func(dsn string) error {
		_, err := goora.ParseConfig(dsn)
		return err
	}
	// openDB opens the pool over a connector.
	openDB = sql.OpenDB
)

// Option configures [NewDatabase].
type Option func(*options)

type options struct {
	schema    string
	dbOptions dalgo2sql.DbOptions
}

// WithSchema scopes the schema reader to the named schema (owner), spelled as
// Oracle stores it (an unquoted name is stored in upper case). Without it the
// session's CURRENT_SCHEMA is read.
func WithSchema(schema string) Option {
	return func(o *options) { o.schema = schema }
}

// WithDbOptions sets the dalgo2sql options of the dal.DB surface (recordsets,
// exact numeric values, ...). StructuredQueryDialect is always "oracle", and
// IsAlreadyExists defaults to [IsAlreadyExists].
func WithDbOptions(dbOptions dalgo2sql.DbOptions) Option {
	return func(o *options) { o.dbOptions = dbOptions }
}

// Database is an Oracle database opened by [NewDatabase]. It is a dal.DB (the
// embedded dalgo2sql database), a dbschema.SchemaReader and an io.Closer.
type Database struct {
	dal.ConcurrencyAvailable
	dal.DB

	sqlDB  *sql.DB
	schema string
}

// NewDatabase opens the Oracle database dsn names (oracle://user:pass@host:port/service),
// pings it, and reads the session's CURRENT_SCHEMA unless [WithSchema] names a
// schema. A failure to connect is a *ConnectionError, whose text holds nothing of
// dsn.
func NewDatabase(ctx context.Context, dsn string, opts ...Option) (*Database, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if err := parseDSN(dsn); err != nil {
		return nil, &ConnectionError{Kind: FailureInvalidDSN, op: "parse"}
	}
	sqlDB := openDB(NewConnector(dsn))
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, asConnectionError("ping", err)
	}
	schema := o.schema
	if schema == "" {
		if err := sqlDB.QueryRowContext(ctx, currentSchemaSQL).Scan(&schema); err != nil {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("dalgo2oracle: read the current schema: %w", err)
		}
	}
	dbOptions := o.dbOptions
	dbOptions.StructuredQueryDialect = StructuredQueryDialect
	if dbOptions.IsAlreadyExists == nil {
		dbOptions.IsAlreadyExists = IsAlreadyExists
	}
	return &Database{
		DB:     dalgo2sql.NewDatabase(sqlDB, dal.NewSchema(nil, nil), dbOptions),
		sqlDB:  sqlDB,
		schema: schema,
	}, nil
}

// asConnectionError returns the *ConnectionError in err's chain, or one built
// from err for op.
func asConnectionError(op string, err error) *ConnectionError {
	var connErr *ConnectionError
	if errors.As(err, &connErr) {
		return connErr
	}
	return newConnectionError(op, err)
}

// IsAlreadyExists reports whether err is a unique-constraint violation
// (ORA-00001).
func IsAlreadyExists(err error) bool {
	var oraErr *network.OracleError
	return errors.As(err, &oraErr) && oraErr.ErrCode == 1
}

// Close closes the connection pool.
func (d *Database) Close() error { return d.sqlDB.Close() }

// SQLDB returns the connection pool, for native SQL. A transaction begun on it
// with sql.TxOptions{ReadOnly: true} is read-only (see [NewConnector]).
func (d *Database) SQLDB() *sql.DB { return d.sqlDB }

// SchemaName returns the schema (owner) the schema reader inspects.
func (d *Database) SchemaName() string { return d.schema }

// SupportsConcurrentConnections reports true: Oracle serves many sessions. The
// explicit method resolves the ambiguity between the two embedded ones.
func (d *Database) SupportsConcurrentConnections() bool {
	return d.ConcurrencyAvailable.SupportsConcurrentConnections()
}

// Adapter returns the adapter name and version.
func (d *Database) Adapter() dal.Adapter { return dal.NewAdapter("dalgo2oracle", Version) }

// BeginReadOnly begins a transaction whose first statement is SET TRANSACTION
// READ ONLY: Oracle refuses every write in it. The caller ends it; a rollback is
// the usual end of a read.
func (d *Database) BeginReadOnly(ctx context.Context) (*sql.Tx, error) {
	tx, err := d.sqlDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("dalgo2oracle: begin a read-only transaction: %w", err)
	}
	return tx, nil
}

// QueryReadOnly runs one native SQL query in a read-only transaction, hands its
// rows to read, and rolls the transaction back. The error is the first of the
// begin, the query, read and the rows' own.
func (d *Database) QueryReadOnly(ctx context.Context, query string, args []any, read func(*sql.Rows) error) error {
	tx, err := d.BeginReadOnly(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if err = read(rows); err != nil {
		return err
	}
	return rows.Err()
}

// driverConnector is used by the compile-time check below.
var _ driver.Connector = (*connector)(nil)
