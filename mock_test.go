package dalgo2oracle

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// dsnConnector opens a sqlmock connection through the sqlmock driver, so the
// adapter's own connector wraps it exactly as it wraps go-ora's.
type dsnConnector struct {
	dsn string
	drv driver.Driver
}

func (c *dsnConnector) Connect(context.Context) (driver.Conn, error) { return c.drv.Open(c.dsn) }
func (c *dsnConnector) Driver() driver.Driver                        { return c.drv }

// stubSeams replaces the seams for one test.
func stubSeams(t *testing.T, connector func(string) driver.Connector, parse func(string) error) {
	t.Helper()
	oldConnector, oldParse := newOracleConnector, parseDSN
	t.Cleanup(func() { newOracleConnector, parseDSN = oldConnector, oldParse })
	newOracleConnector, parseDSN = connector, parse
}

func noParseError(string) error { return nil }

// newMock returns a sqlmock whose queries are matched exactly and the seams that
// make NewDatabase open it.
func newMock(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	dsn := "dalgo2oracle:" + t.Name()
	db0, mock, err := sqlmock.NewWithDSN(dsn, sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db0.Close() })
	stubSeams(t, func(string) driver.Connector { return &dsnConnector{dsn: dsn, drv: db0.Driver()} }, noParseError)
	return mock
}

// newMockDB opens a Database scoped to schema APP over a sqlmock.
func newMockDB(t *testing.T) (*Database, sqlmock.Sqlmock) {
	t.Helper()
	mock := newMock(t)
	d, err := NewDatabase(context.Background(), "oracle://u:p@h/s", WithSchema("APP"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
	return d, mock
}

func owner(name string) sql.NamedArg { return sql.Named("owner", name) }
func named(name string) sql.NamedArg { return sql.Named("name", name) }
