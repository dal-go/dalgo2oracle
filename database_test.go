package dalgo2oracle

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo2sql"
	"github.com/sijms/go-ora/v3/network"
)

// The adapter's surface.
var (
	_ dal.DB                     = (*Database)(nil)
	_ dbschema.SchemaReader      = (*Database)(nil)
	_ interface{ Close() error } = (*Database)(nil)
)

const anyDSN = "oracle://scott:tiger@secret-host:1521/SECRETSVC"

func TestParseDSN_Default(t *testing.T) {
	if err := parseDSN("oracle://u:p@localhost:1521/FREEPDB1"); err != nil {
		t.Fatalf("valid DSN: %v", err)
	}
	if err := parseDSN("oracle://u:p@localhost:notaport/FREEPDB1"); err == nil {
		t.Fatal("want a parse error")
	}
}

func TestNewDatabase_ReadsCurrentSchema(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(currentSchemaSQL).WillReturnRows(sqlmock.NewRows([]string{"s"}).AddRow("APP"))
	d, err := NewDatabase(context.Background(), anyDSN, WithDbOptions(dalgo2sql.DbOptions{ID: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	if d.SchemaName() != "APP" || d.SQLDB() == nil || d.ID() != "x" {
		t.Fatalf("schema %q", d.SchemaName())
	}
	if !d.SupportsConcurrentConnections() || d.Adapter().Name() != "dalgo2oracle" {
		t.Fatal("adapter")
	}
	_ = d.Close()
}

func TestNewDatabase_CurrentSchemaFails(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(currentSchemaSQL).WillReturnError(network.NewOracleError(942))
	if _, err := NewDatabase(context.Background(), anyDSN); err == nil {
		t.Fatal("want error")
	}
}

func TestNewDatabase_InvalidDSN(t *testing.T) {
	stubSeams(t, newOracleConnector, func(string) error { return errors.New("parse " + anyDSN) })
	_, err := NewDatabase(context.Background(), anyDSN)
	assertNoSecret(t, err)
	var ce *ConnectionError
	if !errors.As(err, &ce) || ce.Kind != FailureInvalidDSN {
		t.Fatalf("got %v", err)
	}
}

func TestNewDatabase_ConnectFails(t *testing.T) {
	stubSeams(t, func(string) driver.Connector { return &fakeConnector{err: leakyErr} }, noParseError)
	_, err := NewDatabase(context.Background(), anyDSN)
	assertNoSecret(t, err)
	var ce *ConnectionError
	if !errors.As(err, &ce) || ce.Kind != FailureNetwork || ce.op != "connect" {
		t.Fatalf("got %#v", err)
	}
}

func TestNewDatabase_PingRefused(t *testing.T) {
	refusing := &fullConn{bareConn: bareConn{err: network.NewOracleError(1017)}}
	stubSeams(t, func(string) driver.Connector { return &fakeConnector{conn: refusing} }, noParseError)
	_, err := NewDatabase(context.Background(), anyDSN)
	var ce *ConnectionError
	if !errors.As(err, &ce) || ce.Kind != FailureAuthentication || ce.Code != 1017 || ce.op != "ping" {
		t.Fatalf("got %#v", err)
	}
}

func TestIsAlreadyExists(t *testing.T) {
	if !IsAlreadyExists(network.NewOracleError(1)) || IsAlreadyExists(network.NewOracleError(2291)) || IsAlreadyExists(errors.New("x")) {
		t.Fatal("IsAlreadyExists")
	}
}

func expectReadOnlyBegin(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectExec(setTransactionReadOnly).WillReturnResult(sqlmock.NewResult(0, 0))
}

func TestQueryReadOnly(t *testing.T) {
	ctx := context.Background()
	const q = "SELECT 1 FROM DUAL"
	t.Run("ok", func(t *testing.T) {
		d, mock := newMockDB(t)
		expectReadOnlyBegin(mock)
		mock.ExpectQuery(q).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
		mock.ExpectRollback()
		var n int
		err := d.QueryReadOnly(ctx, q, nil, func(rows *sql.Rows) error {
			for rows.Next() {
				if err := rows.Scan(&n); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil || n != 1 {
			t.Fatalf("n=%d err=%v", n, err)
		}
	})
	t.Run("begin fails", func(t *testing.T) {
		d, mock := newMockDB(t)
		mock.ExpectBegin().WillReturnError(errors.New("no"))
		if err := d.QueryReadOnly(ctx, q, nil, nil); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("query fails", func(t *testing.T) {
		d, mock := newMockDB(t)
		expectReadOnlyBegin(mock)
		mock.ExpectQuery(q).WillReturnError(network.NewOracleError(942))
		mock.ExpectRollback()
		if err := d.QueryReadOnly(ctx, q, nil, nil); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("read fails", func(t *testing.T) {
		d, mock := newMockDB(t)
		expectReadOnlyBegin(mock)
		mock.ExpectQuery(q).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
		mock.ExpectRollback()
		want := errors.New("read")
		if err := d.QueryReadOnly(ctx, q, nil, func(*sql.Rows) error { return want }); !errors.Is(err, want) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestRunReadonlyTransaction_SetsTransactionReadOnly(t *testing.T) {
	d, mock := newMockDB(t)
	expectReadOnlyBegin(mock)
	mock.ExpectCommit()
	if err := d.RunReadonlyTransaction(context.Background(), func(context.Context, dal.ReadTransaction) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
