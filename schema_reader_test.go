package dalgo2oracle

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/sijms/go-ora/v3/network"
)

var (
	ctx        = context.Background()
	tableRef   = dal.NewRootCollectionRef("ORDERS", "")
	errQuery   = network.NewOracleError(942)
	errRow     = errors.New("row")
	relCols    = []string{"name", "kind"}
	columnCols = []string{"column_name", "data_type", "data_length", "char_length", "char_used", "data_precision",
		"data_scale", "nullable", "identity_column", "virtual_column", "data_default"}
	indexCols = []string{"index_name", "uniqueness", "column_name", "column_expression"}
	fkCols    = []string{"constraint_name", "column_name", "r_owner", "r_table", "r_column", "delete_rule", "status"}
)

func relations() *sqlmock.Rows {
	return sqlmock.NewRows(relCols).AddRow("CUSTOMERS", "TABLE").AddRow("ORDERS_MV", "MATERIALIZED VIEW").AddRow("V_ORDERS", "VIEW")
}

func TestListCollectionsAndViews(t *testing.T) {
	d, mock := newMockDB(t)
	mock.ExpectQuery(listRelationsSQL).WithArgs(owner("APP")).WillReturnRows(relations())
	mock.ExpectQuery(listRelationsSQL).WillReturnRows(relations())
	all, err := d.ListCollections(ctx, nil)
	if err != nil || len(all) != 3 || all[0].Name() != "CUSTOMERS" {
		t.Fatalf("%v %v", all, err)
	}
	views, err := d.ListViews(ctx)
	if err != nil || len(views) != 2 || views[0].Name() != "ORDERS_MV" || views[1].Name() != "V_ORDERS" {
		t.Fatalf("%v %v", views, err)
	}
}

func TestListRelations_Errors(t *testing.T) {
	d, mock := newMockDB(t)
	mock.ExpectQuery(listRelationsSQL).WillReturnError(errQuery)
	mock.ExpectQuery(listRelationsSQL).WillReturnRows(sqlmock.NewRows(relCols).AddRow(nil, "TABLE"))
	mock.ExpectQuery(listRelationsSQL).WillReturnRows(sqlmock.NewRows(relCols).AddRow("A", "TABLE").RowError(0, errRow))
	if _, err := d.ListCollections(ctx, nil); !errors.Is(err, errQuery) {
		t.Fatalf("query: %v", err)
	}
	if _, err := d.ListViews(ctx); err == nil {
		t.Fatal("scan: want error")
	}
	if _, err := d.ListCollections(ctx, nil); !errors.Is(err, errRow) {
		t.Fatalf("rows: %v", err)
	}
}

func columnRows() *sqlmock.Rows {
	return sqlmock.NewRows(columnCols).
		AddRow("ID", "NUMBER", 22, 0, nil, nil, 0, "N", "YES", "NO", `"APP"."ISEQ$$_1".nextval`).
		AddRow("NAME", "VARCHAR2", 400, 100, "C", nil, nil, "N", "NO", "NO", "'x' ").
		AddRow("CODE", "CHAR", 4, 4, "B", nil, nil, "Y", "NO", "NO", " NULL ").
		AddRow("AMOUNT", "NUMBER", 22, 0, nil, 10, 2, "Y", "NO", "NO", nil).
		AddRow("BIG", "NUMBER", 22, 0, nil, 20, 0, "Y", "NO", "NO", nil).
		AddRow("N", "NUMBER", 22, 0, nil, nil, nil, "Y", "NO", "NO", nil).
		AddRow("SMALL", "NUMBER", 22, 0, nil, 5, 0, "Y", "NO", "NO", nil).
		AddRow("F", "BINARY_DOUBLE", 8, 0, nil, nil, nil, "Y", "NO", "NO", nil).
		AddRow("D", "DATE", 7, 0, nil, nil, nil, "Y", "NO", "NO", "SYSDATE").
		AddRow("TS", "TIMESTAMP(6) WITH TIME ZONE", 13, 0, nil, nil, 6, "Y", "NO", "NO", "systimestamp(3)").
		AddRow("R", "RAW", 16, 0, nil, nil, nil, "Y", "NO", "NO", nil).
		AddRow("B", "BLOB", 4000, 0, nil, nil, nil, "Y", "NO", "NO", nil).
		AddRow("FLAG", "BOOLEAN", 1, 0, nil, nil, nil, "Y", "NO", "NO", nil).
		AddRow("V", "VARCHAR2", 400, 100, "C", nil, nil, "Y", "NO", "YES", `UPPER("NAME")`).
		AddRow("X", "INTERVAL DAY(2) TO SECOND(6)", 11, 0, nil, 2, 6, "Y", "NO", "NO", nil)
}

func expectDescribe(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(probeRelationSQL).WithArgs(owner("APP"), named("ORDERS")).WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("TABLE"))
	mock.ExpectQuery(primaryKeySQL).WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow("ID"))
	mock.ExpectQuery(columnsSQL).WillReturnRows(columnRows())
	mock.ExpectQuery(indexesSQL).WillReturnRows(sqlmock.NewRows(indexCols).
		AddRow("IX_A", "NONUNIQUE", "NAME", nil).
		AddRow("IX_A", "NONUNIQUE", "SYS_NC00016$", `"CODE"`).
		AddRow("IX_F", "UNIQUE", "SYS_NC00017$", `UPPER("NAME")`))
	mock.ExpectQuery(foreignKeysSQL).WillReturnRows(sqlmock.NewRows(fkCols).
		AddRow("FK1", "CUST_ID", "APP", "CUSTOMERS", "ID", "CASCADE", "ENABLED").
		AddRow("FK1", "CUST_ID2", "APP", "CUSTOMERS", "ID2", "CASCADE", "ENABLED").
		AddRow("FK2", "C", "OTHER", "T", "ID", "NO ACTION", "DISABLED"))
}

func TestDescribeCollection(t *testing.T) {
	d, mock := newMockDB(t)
	expectDescribe(mock)
	def, err := d.DescribeCollection(ctx, &tableRef)
	if err != nil {
		t.Fatal(err)
	}
	if def.Name != "ORDERS" || !reflect.DeepEqual(def.PrimaryKey, []dal.FieldName{"ID"}) || len(def.Fields) != 15 {
		t.Fatalf("%+v", def)
	}
	byName := map[dal.FieldName]dbschema.FieldDef{}
	for _, f := range def.Fields {
		byName[f.Name] = f
	}
	check := func(name dal.FieldName, typ dbschema.Type) dbschema.FieldDef {
		t.Helper()
		f := byName[name]
		if f.Type != typ {
			t.Errorf("%s: type %v want %v", name, f.Type, typ)
		}
		return f
	}
	if f := check("ID", dbschema.Int); !f.AutoIncrement || f.Default != nil || f.Nullable {
		t.Errorf("ID %+v", f)
	}
	if f := check("NAME", dbschema.String); *f.Length != 100 || f.Default != (dbschema.DefaultLiteral{Value: "'x'"}) || f.Nullable {
		t.Errorf("NAME %+v", f)
	}
	if f := check("CODE", dbschema.String); f.Default != nil || !f.Nullable {
		t.Errorf("CODE %+v", f)
	}
	if f := check("AMOUNT", dbschema.Decimal); *f.Precision != (dbschema.Precision{Total: 10, Scale: 2}) {
		t.Errorf("AMOUNT %+v", f)
	}
	if f := check("BIG", dbschema.Decimal); *f.Precision != (dbschema.Precision{Total: 20}) {
		t.Errorf("BIG %+v", f)
	}
	if f := check("N", dbschema.Decimal); f.Precision != nil {
		t.Errorf("N %+v", f)
	}
	check("SMALL", dbschema.Int)
	check("F", dbschema.Float)
	if f := check("D", dbschema.Time); f.Default != (dbschema.DefaultCurrentTimestamp{}) {
		t.Errorf("D %+v", f)
	}
	if f := check("TS", dbschema.Time); f.Default != (dbschema.DefaultCurrentTimestamp{}) {
		t.Errorf("TS %+v", f)
	}
	if f := check("R", dbschema.Bytes); *f.Length != 16 {
		t.Errorf("R %+v", f)
	}
	check("B", dbschema.Bytes)
	check("FLAG", dbschema.Bool)
	if f := check("V", dbschema.String); f.Default != (dbschema.DefaultLiteral{Value: `GENERATED ALWAYS AS (UPPER("NAME"))`}) {
		t.Errorf("V %+v", f)
	}
	check("X", dbschema.String)

	declared := map[string]string{}
	for _, c := range def.SourceDefinition.Columns {
		declared[c.Name] = c.DeclaredType
	}
	want := map[string]string{"ID": "integer", "NAME": "varchar2(100 char)", "CODE": "char(4 byte)", "AMOUNT": "number(10,2)",
		"N": "number", "R": "raw(16)", "TS": "timestamp(6) with time zone"}
	for k, v := range want {
		if declared[k] != v {
			t.Errorf("declared %s = %q want %q", k, declared[k], v)
		}
	}
	if def.SourceDefinition.Columns[0].PrimaryKeyPosition != 1 || def.SourceDefinition.Columns[2].DefaultSQL != nil {
		t.Errorf("source %+v", def.SourceDefinition.Columns[:3])
	}

	wantIdx := []dbschema.IndexDef{
		{Name: "IX_A", Collection: "ORDERS", Fields: []dal.FieldName{"NAME", "CODE"}},
		{Name: "IX_F", Collection: "ORDERS", Fields: []dal.FieldName{`UPPER("NAME")`}, Unique: true},
	}
	if !reflect.DeepEqual(def.Indexes, wantIdx) {
		t.Errorf("indexes %+v", def.Indexes)
	}
	wantFK := []dbschema.ForeignKeyDef{
		{Name: "FK1", Fields: []dal.FieldName{"CUST_ID", "CUST_ID2"}, ReferencedCollection: "CUSTOMERS",
			ReferencedFields: []dal.FieldName{"ID", "ID2"}, Enforcement: dbschema.ForeignKeyEnforcementEnabled, OnUpdate: "NO ACTION", OnDelete: "CASCADE"},
		{Name: "FK2", Fields: []dal.FieldName{"C"}, ReferencedCollection: "T", ReferencedNamespace: "OTHER",
			ReferencedFields: []dal.FieldName{"ID"}, Enforcement: dbschema.ForeignKeyEnforcementDisabled, OnUpdate: "NO ACTION", OnDelete: "NO ACTION"},
	}
	if !reflect.DeepEqual(def.ForeignKeys, wantFK) {
		t.Errorf("fks %+v", def.ForeignKeys)
	}
}

func TestDescribeCollection_Errors(t *testing.T) {
	probeOK := func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(probeRelationSQL).WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("TABLE"))
	}
	pkOK := func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(primaryKeySQL).WillReturnRows(sqlmock.NewRows([]string{"c"}))
	}
	columnsOK := func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(columnsSQL).WillReturnRows(sqlmock.NewRows(columnCols))
	}
	cases := map[string]func(sqlmock.Sqlmock){
		"not found": func(m sqlmock.Sqlmock) {
			m.ExpectQuery(probeRelationSQL).WillReturnRows(sqlmock.NewRows([]string{"kind"}))
		},
		"probe": func(m sqlmock.Sqlmock) { m.ExpectQuery(probeRelationSQL).WillReturnError(errQuery) },
		"pk query": func(m sqlmock.Sqlmock) {
			probeOK(m)
			m.ExpectQuery(primaryKeySQL).WillReturnError(errQuery)
		},
		"pk scan": func(m sqlmock.Sqlmock) {
			probeOK(m)
			m.ExpectQuery(primaryKeySQL).WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(nil))
		},
		"pk rows": func(m sqlmock.Sqlmock) {
			probeOK(m)
			m.ExpectQuery(primaryKeySQL).WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow("A").RowError(0, errRow))
		},
		"columns query": func(m sqlmock.Sqlmock) {
			probeOK(m)
			pkOK(m)
			m.ExpectQuery(columnsSQL).WillReturnError(errQuery)
		},
		"columns scan": func(m sqlmock.Sqlmock) {
			probeOK(m)
			pkOK(m)
			m.ExpectQuery(columnsSQL).WillReturnRows(sqlmock.NewRows(columnCols).AddRow(nil, "X", 1, 1, nil, nil, nil, "Y", "NO", "NO", nil))
		},
		"columns rows": func(m sqlmock.Sqlmock) {
			probeOK(m)
			pkOK(m)
			m.ExpectQuery(columnsSQL).WillReturnRows(sqlmock.NewRows(columnCols).AddRow("A", "X", 1, 1, nil, nil, nil, "Y", "NO", "NO", nil).RowError(0, errRow))
		},
		"indexes": func(m sqlmock.Sqlmock) {
			probeOK(m)
			pkOK(m)
			columnsOK(m)
			m.ExpectQuery(indexesSQL).WillReturnError(errQuery)
		},
		"foreign keys": func(m sqlmock.Sqlmock) {
			probeOK(m)
			pkOK(m)
			columnsOK(m)
			m.ExpectQuery(indexesSQL).WillReturnRows(sqlmock.NewRows(indexCols))
			m.ExpectQuery(foreignKeysSQL).WillReturnError(errQuery)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			d, mock := newMockDB(t)
			setup(mock)
			if _, err := d.DescribeCollection(ctx, &tableRef); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestForeignKeys_ScanAndRowErrors(t *testing.T) {
	for name, rows := range map[string]*sqlmock.Rows{
		"scan": sqlmock.NewRows(fkCols).AddRow(nil, "C", "APP", "T", "ID", "NO ACTION", "ENABLED"),
		"rows": sqlmock.NewRows(fkCols).AddRow("F", "C", "APP", "T", "ID", "NO ACTION", "ENABLED").RowError(0, errRow),
	} {
		t.Run(name, func(t *testing.T) {
			d, mock := newMockDB(t)
			mock.ExpectQuery(foreignKeysSQL).WillReturnRows(rows)
			if _, err := d.readForeignKeys(ctx, "APP", nil); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestListIndexes(t *testing.T) {
	ref := dal.NewQualifiedRootCollectionRef("OTHER", "T", "")
	t.Run("schema of the reference", func(t *testing.T) {
		d, mock := newMockDB(t)
		mock.ExpectQuery(indexesSQL).WithArgs(owner("OTHER"), named("T")).WillReturnRows(sqlmock.NewRows(indexCols).AddRow("IX", "UNIQUE", "A", nil))
		got, err := d.ListIndexes(ctx, &ref)
		if err != nil || len(got) != 1 || got[0].Collection != "T" || !got[0].Unique {
			t.Fatalf("%+v %v", got, err)
		}
	})
	for name, rows := range map[string]*sqlmock.Rows{
		"scan": sqlmock.NewRows(indexCols).AddRow(nil, "UNIQUE", "A", nil),
		"rows": sqlmock.NewRows(indexCols).AddRow("IX", "UNIQUE", "A", nil).RowError(0, errRow),
	} {
		t.Run(name, func(t *testing.T) {
			d, mock := newMockDB(t)
			mock.ExpectQuery(indexesSQL).WillReturnRows(rows)
			if _, err := d.ListIndexes(ctx, &tableRef); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

var constraintCols = []string{"constraint_name", "constraint_type", "search_condition_vc"}

func TestListConstraints(t *testing.T) {
	d, mock := newMockDB(t)
	mock.ExpectQuery(constraintsSQL).WithArgs(owner("APP"), named("ORDERS")).WillReturnRows(sqlmock.NewRows(constraintCols).
		AddRow("CK_AMOUNT", "C", "AMOUNT > 0").
		AddRow("SYS_C001", "C", `"ID" IS NOT NULL`).
		AddRow("PK_ORDERS", "P", nil).
		AddRow("FK1", "R", nil).
		AddRow("UQ_CODE", "U", nil))
	got, err := d.ListConstraints(ctx, &tableRef)
	want := []dbschema.ConstraintDef{{Name: "CK_AMOUNT", Type: "check"}, {Name: "PK_ORDERS", Type: "primary-key"},
		{Name: "FK1", Type: "foreign-key"}, {Name: "UQ_CODE", Type: "unique"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestListConstraints_Errors(t *testing.T) {
	for name, setup := range map[string]func(sqlmock.Sqlmock){
		"query": func(m sqlmock.Sqlmock) { m.ExpectQuery(constraintsSQL).WillReturnError(errQuery) },
		"scan": func(m sqlmock.Sqlmock) {
			m.ExpectQuery(constraintsSQL).WillReturnRows(sqlmock.NewRows(constraintCols).AddRow(nil, "P", nil))
		},
		"rows": func(m sqlmock.Sqlmock) {
			m.ExpectQuery(constraintsSQL).WillReturnRows(sqlmock.NewRows(constraintCols).AddRow("A", "P", nil).RowError(0, errRow))
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, mock := newMockDB(t)
			setup(mock)
			if _, err := d.ListConstraints(ctx, &tableRef); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

var referrerCols = []string{"table_name", "column_name"}

func TestListReferrers(t *testing.T) {
	d, mock := newMockDB(t)
	mock.ExpectQuery(referrersSQL).WithArgs(owner("APP"), named("CUSTOMERS")).WillReturnRows(sqlmock.NewRows(referrerCols).
		AddRow("INVOICES", "CUST_ID").AddRow("ORDERS", "BILL_TO").AddRow("ORDERS", "SHIP_TO"))
	ref := dal.NewRootCollectionRef("CUSTOMERS", "")
	got, err := d.ListReferrers(ctx, &ref)
	if err != nil || len(got) != 2 || got[1].Collection.Name() != "ORDERS" ||
		!reflect.DeepEqual(got[1].Fields, []dal.FieldName{"BILL_TO", "SHIP_TO"}) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestListReferrers_Errors(t *testing.T) {
	for name, setup := range map[string]func(sqlmock.Sqlmock){
		"query": func(m sqlmock.Sqlmock) { m.ExpectQuery(referrersSQL).WillReturnError(errQuery) },
		"scan": func(m sqlmock.Sqlmock) {
			m.ExpectQuery(referrersSQL).WillReturnRows(sqlmock.NewRows(referrerCols).AddRow(nil, "C"))
		},
		"rows": func(m sqlmock.Sqlmock) {
			m.ExpectQuery(referrersSQL).WillReturnRows(sqlmock.NewRows(referrerCols).AddRow("A", "C").RowError(0, errRow))
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, mock := newMockDB(t)
			setup(mock)
			if _, err := d.ListReferrers(ctx, &tableRef); err == nil {
				t.Fatal("want error")
			}
		})
	}
}
