# dalgo2oracle

Oracle Database adapter for [DALgo](https://github.com/dal-go/dalgo). It opens
the server with the pure-Go driver
[`github.com/sijms/go-ora/v3`](https://github.com/sijms/go-ora) (no Oracle
client, compiles with `CGO_ENABLED=0`), wraps
[`github.com/dal-go/dalgo2sql`](https://github.com/dal-go/dalgo2sql) for the
`dal.DB` surface, and adds Oracle-native implementations of:

- `dbschema.SchemaReader` — schema introspection over Oracle's `ALL_` catalog
  views, scoped to one schema (owner), plus `ListViews`
- read-only transactions, which go-ora itself refuses
- errors that never carry the connection string, the user, the host, the port
  or the service name
- `dal.ConcurrencyAware` — `SupportsConcurrentConnections() = true`

## Constructor usage

```go
import (
    "context"

    "github.com/dal-go/dalgo2oracle"
)

db, err := dalgo2oracle.NewDatabase(ctx, "oracle://user:pass@host:1521/FREEPDB1")
if err != nil {
    return err // a *dalgo2oracle.ConnectionError when the server cannot be reached
}
defer db.Close()
```

The URL is go-ora's: `oracle://user:password@host:port/service`, with special
characters percent-encoded. `NewDatabase` parses it, pings the server and reads
the session's `CURRENT_SCHEMA`.

For native SQL, `db.SQLDB()` returns the `*sql.DB` pool, and `NewConnector(dsn)`
returns a `driver.Connector` to open one yourself with `sql.OpenDB`; both carry
the read-only transactions, the fast-login workaround and the error sanitising
described below.

### Options

| Option | Effect |
|--------|--------|
| `WithSchema(name)` | Scopes the schema reader to `name`, spelled as Oracle stores it (an unquoted name is upper case). Default: the session's `CURRENT_SCHEMA`. |
| `WithDbOptions(dalgo2sql.DbOptions{...})` | The `dalgo2sql` options of the `dal.DB` surface (recordsets, exact numeric values, ...). `StructuredQueryDialect` is always set to `"oracle"`; `IsAlreadyExists` defaults to `dalgo2oracle.IsAlreadyExists` (ORA-00001). |

Other exported API: `Database.SchemaName()`, `Database.Adapter()` (name
`dalgo2oracle`, `Version`), `Database.BeginReadOnly`, `Database.QueryReadOnly`,
`IsAlreadyExists`, `ConnectionError` with `FailureKind`, and the constants
`Version` and `StructuredQueryDialect`.

## Schema reader

`ListCollections` returns the tables, views and materialized views of the
schema in alphabetical order, under the names Oracle stores. It leaves out
Oracle-maintained objects, recycle-bin (`BIN$`) objects, nested, secondary and
IOT overflow tables, materialized-view container tables and materialized-view
log tables. `ListViews` returns the views and materialized views among them.

`DescribeCollection` reports, for a table, view or materialized view:

- the fields in column order (hidden columns left out), with their type,
  length, precision, nullability (a primary-key column is never nullable) and
  `AutoIncrement` for an identity column;
- defaults: a `dbschema.DefaultLiteral` holding the expression as Oracle stores
  it, except `SYSDATE`, `SYSTIMESTAMP`, `CURRENT_DATE`, `CURRENT_TIMESTAMP` and
  `LOCALTIMESTAMP`, which are `dbschema.DefaultCurrentTimestamp`; an identity
  column has no default, and a virtual column's is
  `GENERATED ALWAYS AS (<expression>)`;
- the primary key, the indexes (not the one backing the primary key, not LOB
  indexes; a function-based column is named by its expression) and the
  outgoing foreign keys (a key to another schema names it in
  `ReferencedNamespace`; `OnUpdate` is always `NO ACTION`, which is all Oracle
  has; `Enforcement` follows the constraint's status);
- `SourceDefinition` with dialect `oracle` and each column's declared type
  (`varchar2(100 char)`, `number(10,2)`, `raw(16)`, ...), `NOT NULL`, default
  SQL and primary-key position.

`ListIndexes`, `ListConstraints` (typed `primary-key`, `unique`, `foreign-key`
and `check`; the check constraints Oracle records for `NOT NULL` columns are
left out) and `ListReferrers` read the same catalog.

Which schema is read: `ListCollections` and `ListViews` list only the
configured schema (`WithSchema`, else `CURRENT_SCHEMA`). `DescribeCollection`,
`ListIndexes`, `ListConstraints` and `ListReferrers` read the schema the
collection reference names (`dal.CollectionRef.Schema()`), and the configured
one only when the reference names none. Either way they see only what the
user's `ALL_*` views show, that is, objects the user owns or has been granted
access to.

## Type mapping

| Oracle column type | `dbschema.Type` |
|--------------------|-----------------|
| `VARCHAR2`, `NVARCHAR2`, `CHAR`, `NCHAR` | `String`, `Length` = length in characters |
| `NUMBER(p,0)` with p ≤ 18, `INTEGER` | `Int` |
| `NUMBER(p,s)` otherwise | `Decimal`, `Precision` = (p, s) |
| `NUMBER` | `Decimal` |
| `FLOAT`, `BINARY_FLOAT`, `BINARY_DOUBLE` | `Float` |
| `DATE`, `TIMESTAMP*` | `Time` |
| `RAW(n)` | `Bytes`, `Length` = n |
| `LONG RAW`, `BLOB`, `BFILE` | `Bytes` |
| `BOOLEAN` (23ai) | `Bool` |
| anything else (`CLOB`, `INTERVAL`, `XMLTYPE`, `JSON`, `ROWID`, ...) | `String`; `SourceDefinition` keeps the declared type |

## Read-only transactions

go-ora refuses `sql.TxOptions{ReadOnly: true}`. The adapter's connector accepts
it: it begins a plain transaction whose first statement is
`SET TRANSACTION READ ONLY`, after which Oracle refuses every write in it with
ORA-01456. `Database.BeginReadOnly` and `Database.QueryReadOnly` (one query,
rolled back afterwards) use it, and so does any `BeginTx` with `ReadOnly` on
`db.SQLDB()`. Only the default isolation level is supported.

## Fast login is turned off

With go-ora's "fast login" on, go-ora v3.0.1 caches a negotiation cookie per
service, and against Oracle Database 23ai Free every later connection of the
process ends in EOF, so a pool cannot open a second connection. The connector
therefore adds `fast login=false` (`fast+login=false`) to the URL's query,
before any `#fragment`, unless the URL sets the option itself under a key go-ora
reads (`fast login` in any case, nothing trimmed).

## Error sanitising

go-ora and the Oracle listener put the user, host, port and service name into
their error messages, and callers print errors. So:

- A failure to connect — from `NewDatabase`, or from the pool connecting again
  later — is a `*ConnectionError` whose text is only the step, a fixed sentence
  chosen by `Kind` (`FailureInvalidDSN`, `FailureNetwork`, `FailureTimeout`,
  `FailureAuthentication`, `FailureService`, `FailureServer`, `FailureOther`)
  and the ORA number in `Code`. The driver's error is not reachable through
  `errors.Unwrap`/`Is`/`As`; branch on `Kind` and `Code`. `errors.Is` still
  finds `context.Canceled` and `context.DeadlineExceeded`.
- A transport failure on an open connection (a reset socket, an EOF) becomes a
  `*ConnectionError` too.
- A server error about a statement (`*network.OracleError`, for example
  ORA-00942) passes unchanged, as do `driver.ErrBadConn` and context errors.

## Current limitations

- **Structured queries** (`dal.DB` query execution) need `dalgo2sql`'s
  `"oracle"` structured-query dialect, which `dalgo2sql` does not have yet; it
  is being added in `dal-go/dalgo2sql`. Until then the schema reader and native
  SQL through `SQLDB()` are the working surface.
- **Key reads and writes** (`Get`, `Set`, `Insert`, `Delete`, ...) need Oracle's
  `:1`, `:2`, ... bind placeholders. `dalgo2sql` emits only `?`
  (`PlaceholderQuestion`) and `$1` (`PlaceholderDollar`), so these do not work
  against Oracle yet.
- No `ddl.Applier`: the adapter does not create or alter tables.

## Running the tests

`go test ./...` runs the unit tests (sqlmock and fake drivers) and covers every
statement without a server.

`TestOracleServer` runs against a real server and skips unless
`DALGO2ORACLE_TEST_URL` names one. It creates `D2O_*` fixture objects in the
user's own schema and drops them again. With Docker:

```sh
docker run -d --name oracle-free -p 1521:1521 \
  -e ORACLE_PASSWORD=sys_pw_9 -e APP_USER=scott -e APP_USER_PASSWORD=tiger9 \
  gvenzl/oracle-free:23-slim

DALGO2ORACLE_TEST_URL='oracle://scott:tiger9@localhost:1521/FREEPDB1' \
  go test ./... -count=1 -v -run TestOracleServer
```

The user and password must not be words of the adapter's error text (such as
`dalgo` or `oracle`): the test asserts that a refused login names nothing of the
URL.

CI runs the same test against `gvenzl/oracle-free:23-slim` pinned by digest and
fails on a skip or a missing pass line. Oracle Database Free is used there under
the Oracle Free Use Terms and Conditions, for automated testing only; the image
is never redistributed.

## License

MIT — see [LICENSE](LICENSE).
