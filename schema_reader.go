package dalgo2oracle

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	dalrecord "github.com/dal-go/record"
)

// relationsFrom selects the tables, views and materialized views of :owner that
// a user made: Oracle-maintained objects, recycle-bin (BIN$) objects, nested,
// secondary (domain index) and IOT overflow tables, materialized-view container
// tables and materialized-view log tables are left out.
const relationsFrom = `FROM (
  SELECT t.table_name AS name, 'TABLE' AS kind FROM all_tables t
   WHERE t.owner = :owner AND t.nested = 'NO' AND t.secondary = 'N' AND t.dropped = 'NO'
     AND (t.iot_type IS NULL OR t.iot_type = 'IOT')
     AND NOT EXISTS (SELECT 1 FROM all_mviews m WHERE m.owner = t.owner AND m.container_name = t.table_name)
     AND NOT EXISTS (SELECT 1 FROM all_mview_logs l WHERE l.log_owner = t.owner AND l.log_table = t.table_name)
  UNION ALL
  SELECT v.view_name, 'VIEW' FROM all_views v WHERE v.owner = :owner
  UNION ALL
  SELECT m.mview_name, 'MATERIALIZED VIEW' FROM all_mviews m WHERE m.owner = :owner
) c
WHERE c.name NOT LIKE 'BIN$%'
  AND EXISTS (SELECT 1 FROM all_objects o WHERE o.owner = :owner AND o.object_name = c.name
              AND o.object_type = c.kind AND o.oracle_maintained = 'N')`

const (
	listRelationsSQL = `SELECT c.name, c.kind ` + relationsFrom + ` ORDER BY c.name`
	probeRelationSQL = `SELECT c.kind ` + relationsFrom + ` AND c.name = :name`

	columnsSQL = `SELECT column_name, data_type, data_length, char_length, char_used, data_precision, data_scale,
       nullable, identity_column, virtual_column, data_default
FROM all_tab_cols
WHERE owner = :owner AND table_name = :name AND hidden_column = 'NO'
ORDER BY column_id`

	primaryKeySQL = `SELECT cc.column_name
FROM all_constraints c
JOIN all_cons_columns cc ON cc.owner = c.owner AND cc.constraint_name = c.constraint_name AND cc.table_name = c.table_name
WHERE c.owner = :owner AND c.table_name = :name AND c.constraint_type = 'P'
ORDER BY cc.position`

	// indexesSQL leaves out the index that backs the primary key and LOB indexes.
	indexesSQL = `SELECT i.index_name, i.uniqueness, ic.column_name, ie.column_expression
FROM all_indexes i
JOIN all_ind_columns ic ON ic.index_owner = i.owner AND ic.index_name = i.index_name
LEFT JOIN all_ind_expressions ie ON ie.index_owner = ic.index_owner AND ie.index_name = ic.index_name
     AND ie.column_position = ic.column_position
WHERE i.table_owner = :owner AND i.table_name = :name AND i.index_type NOT LIKE 'LOB%'
  AND i.index_name NOT LIKE 'BIN$%'
  AND NOT EXISTS (SELECT 1 FROM all_constraints p WHERE p.owner = i.table_owner AND p.table_name = i.table_name
                  AND p.constraint_type = 'P' AND p.index_owner = i.owner AND p.index_name = i.index_name)
ORDER BY i.index_name, ic.column_position`
)

// kindTable is the kind of a base table in listRelationsSQL.
const kindTable = "TABLE"

type relation struct{ name, kind string }

func (d *Database) listRelations(ctx context.Context, op string) ([]relation, error) {
	rows, err := d.sqlDB.QueryContext(ctx, listRelationsSQL, sql.Named("owner", d.schema))
	if err != nil {
		return nil, fmt.Errorf("dalgo2oracle: %s: %w", op, err)
	}
	defer func() { _ = rows.Close() }()
	var out []relation
	for rows.Next() {
		var r relation
		if err = rows.Scan(&r.name, &r.kind); err != nil {
			return nil, fmt.Errorf("dalgo2oracle: %s scan: %w", op, err)
		}
		out = append(out, r)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("dalgo2oracle: %s rows: %w", op, err)
	}
	return out, nil
}

// ListCollections returns the tables, views and materialized views of the
// schema in alphabetical order, under the names Oracle stores. The parent key
// is ignored.
func (d *Database) ListCollections(ctx context.Context, parent *dalrecord.Key) ([]dal.CollectionRef, error) {
	_ = parent
	relations, err := d.listRelations(ctx, "ListCollections")
	if err != nil {
		return nil, err
	}
	out := make([]dal.CollectionRef, 0, len(relations))
	for _, r := range relations {
		out = append(out, dal.NewRootCollectionRef(r.name, ""))
	}
	return out, nil
}

// ListViews returns the collections of the schema that are views or
// materialized views, by the references ListCollections gives them. It is the
// method a schema consumer can look for to tell a view from a table.
func (d *Database) ListViews(ctx context.Context) ([]dal.CollectionRef, error) {
	relations, err := d.listRelations(ctx, "ListViews")
	if err != nil {
		return nil, err
	}
	out := make([]dal.CollectionRef, 0, len(relations))
	for _, r := range relations {
		if r.kind != kindTable {
			out = append(out, dal.NewRootCollectionRef(r.name, ""))
		}
	}
	return out, nil
}

// owner returns the schema ref names, else the configured one.
func (d *Database) owner(ref *dal.CollectionRef) string {
	if s := ref.Schema(); s != "" {
		return s
	}
	return d.schema
}

// DescribeCollection returns the fields, primary key, indexes and outgoing
// foreign keys of a table, view or materialized view. A column's default is a
// dbschema.DefaultLiteral holding the expression as Oracle stores it, except
// SYSDATE, SYSTIMESTAMP, CURRENT_DATE, CURRENT_TIMESTAMP and LOCALTIMESTAMP,
// which are dbschema.DefaultCurrentTimestamp; an identity column has
// AutoIncrement and no default; a virtual column's default is
// "GENERATED ALWAYS AS (<expression>)". A type with no dbschema counterpart
// (INTERVAL, XMLTYPE, JSON, ROWID, ...) is reported as a String; SourceDefinition
// keeps the declared Oracle types.
func (d *Database) DescribeCollection(ctx context.Context, ref *dal.CollectionRef) (*dbschema.CollectionDef, error) {
	owner, name := d.owner(ref), ref.Name()
	args := []any{sql.Named("owner", owner), sql.Named("name", name)}

	var kind string
	err := d.sqlDB.QueryRowContext(ctx, probeRelationSQL, args...).Scan(&kind)
	if err == sql.ErrNoRows { //nolint:errorlint // database/sql returns the sentinel bare
		return nil, fmt.Errorf("dalgo2oracle: collection %q not found", name)
	}
	if err != nil {
		return nil, fmt.Errorf("dalgo2oracle: DescribeCollection probe %q: %w", name, err)
	}

	pk, err := d.readStrings(ctx, "primary key", primaryKeySQL, args)
	if err != nil {
		return nil, err
	}
	pkPosition := make(map[string]int, len(pk))
	for i, c := range pk {
		pkPosition[c] = i + 1
	}

	fields, source, err := d.readColumns(ctx, args, pkPosition)
	if err != nil {
		return nil, err
	}
	indexes, err := d.readIndexes(ctx, name, args)
	if err != nil {
		return nil, err
	}
	foreignKeys, err := d.readForeignKeys(ctx, owner, args)
	if err != nil {
		return nil, err
	}
	def := &dbschema.CollectionDef{
		Name:             name,
		Fields:           fields,
		Indexes:          indexes,
		ForeignKeys:      foreignKeys,
		SourceDefinition: &dbschema.SourceDefinition{Dialect: "oracle", Columns: source},
	}
	for _, c := range pk {
		def.PrimaryKey = append(def.PrimaryKey, dal.FieldName(c))
	}
	return def, nil
}

// readStrings reads the first column of every row of query.
func (d *Database) readStrings(ctx context.Context, what, query string, args []any) ([]string, error) {
	rows, err := d.sqlDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("dalgo2oracle: read %s: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("dalgo2oracle: read %s scan: %w", what, err)
		}
		out = append(out, s)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("dalgo2oracle: read %s rows: %w", what, err)
	}
	return out, nil
}

type column struct {
	name, dataType           string
	dataLength, charLength   int64
	charUsed                 sql.NullString
	precision, scale         sql.NullInt64
	nullable, identity, virt string
	dataDefault              sql.NullString
}

func (d *Database) readColumns(ctx context.Context, args []any, pkPosition map[string]int) ([]dbschema.FieldDef, []dbschema.SourceColumnDef, error) {
	rows, err := d.sqlDB.QueryContext(ctx, columnsSQL, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("dalgo2oracle: read columns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var fields []dbschema.FieldDef
	var source []dbschema.SourceColumnDef
	for rows.Next() {
		var c column
		if err = rows.Scan(&c.name, &c.dataType, &c.dataLength, &c.charLength, &c.charUsed, &c.precision, &c.scale,
			&c.nullable, &c.identity, &c.virt, &c.dataDefault); err != nil {
			return nil, nil, fmt.Errorf("dalgo2oracle: read columns scan: %w", err)
		}
		f, s := c.fieldDef(pkPosition[c.name])
		fields = append(fields, f)
		source = append(source, s)
	}
	if err = rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("dalgo2oracle: read columns rows: %w", err)
	}
	return fields, source, nil
}

// fieldDef maps one catalog row to a field and its source column.
func (c column) fieldDef(pkPosition int) (dbschema.FieldDef, dbschema.SourceColumnDef) {
	t, length, precision := oracleType(c)
	f := dbschema.FieldDef{
		Name:          dal.FieldName(c.name),
		Type:          t,
		Length:        length,
		Precision:     precision,
		Nullable:      c.nullable == "Y" && pkPosition == 0,
		AutoIncrement: c.identity == "YES",
	}
	s := dbschema.SourceColumnDef{
		Name:               c.name,
		DeclaredType:       declaredType(c),
		NotNull:            c.nullable != "Y",
		PrimaryKeyPosition: pkPosition,
	}
	def := strings.TrimSpace(c.dataDefault.String)
	if def != "" && !strings.EqualFold(def, "NULL") {
		s.DefaultSQL = &def
		switch {
		case f.AutoIncrement:
		case c.virt == "YES":
			f.Default = dbschema.DefaultLiteral{Value: "GENERATED ALWAYS AS (" + def + ")"}
		case isCurrentTime(def):
			f.Default = dbschema.DefaultCurrentTimestamp{}
		default:
			f.Default = dbschema.DefaultLiteral{Value: def}
		}
	}
	return f, s
}

var currentTime = regexp.MustCompile(`^(?i)(SYSDATE|SYSTIMESTAMP|CURRENT_DATE|CURRENT_TIMESTAMP|LOCALTIMESTAMP)(\s*\(\s*\d\s*\))?$`)

func isCurrentTime(def string) bool { return currentTime.MatchString(def) }

func intPtr(n int64) *int {
	v := int(n)
	return &v
}

// oracleType maps an Oracle column type to its dbschema type, with the length
// of a character or RAW type and the precision of a NUMBER.
func oracleType(c column) (dbschema.Type, *int, *dbschema.Precision) {
	dt := c.dataType
	switch dt {
	case "VARCHAR2", "NVARCHAR2", "CHAR", "NCHAR":
		return dbschema.String, intPtr(c.charLength), nil
	case "NUMBER":
		switch {
		case c.scale.Valid && c.scale.Int64 == 0 && (!c.precision.Valid || c.precision.Int64 <= 18):
			return dbschema.Int, nil, nil
		case c.precision.Valid:
			return dbschema.Decimal, nil, &dbschema.Precision{Total: int(c.precision.Int64), Scale: int(c.scale.Int64)}
		}
		return dbschema.Decimal, nil, nil
	case "FLOAT", "BINARY_FLOAT", "BINARY_DOUBLE":
		return dbschema.Float, nil, nil
	case "DATE":
		return dbschema.Time, nil, nil
	case "RAW":
		return dbschema.Bytes, intPtr(c.dataLength), nil
	case "LONG RAW", "BLOB", "BFILE":
		return dbschema.Bytes, nil, nil
	case "BOOLEAN":
		return dbschema.Bool, nil, nil
	}
	if strings.HasPrefix(dt, "TIMESTAMP") {
		return dbschema.Time, nil, nil
	}
	return dbschema.String, nil, nil
}

// declaredType spells the column type as Oracle declares it: varchar2(255 char),
// number(10,2), raw(16), timestamp(6) with time zone.
func declaredType(c column) string {
	name := strings.ToLower(c.dataType)
	switch c.dataType {
	case "VARCHAR2", "NVARCHAR2", "CHAR", "NCHAR":
		if c.charUsed.String == "C" {
			return fmt.Sprintf("%s(%d char)", name, c.charLength)
		}
		return fmt.Sprintf("%s(%d byte)", name, c.dataLength)
	case "RAW":
		return fmt.Sprintf("raw(%d)", c.dataLength)
	case "NUMBER":
		switch {
		case c.precision.Valid:
			return fmt.Sprintf("number(%d,%d)", c.precision.Int64, c.scale.Int64)
		case c.scale.Valid:
			return "integer"
		}
	}
	return name
}

// ListIndexes returns the indexes of a table but the one that backs its primary
// key: the indexes of unique constraints are unique indexes. A column of a
// function-based index is named by its expression; a descending column, which
// Oracle stores as the expression "NAME", by its name.
func (d *Database) ListIndexes(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.IndexDef, error) {
	return d.readIndexes(ctx, ref.Name(), []any{sql.Named("owner", d.owner(ref)), sql.Named("name", ref.Name())})
}

var quotedName = regexp.MustCompile(`^"([^"]+)"$`)

func (d *Database) readIndexes(ctx context.Context, table string, args []any) ([]dbschema.IndexDef, error) {
	rows, err := d.sqlDB.QueryContext(ctx, indexesSQL, args...)
	if err != nil {
		return nil, fmt.Errorf("dalgo2oracle: read indexes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []dbschema.IndexDef
	for rows.Next() {
		var name, uniqueness, col string
		var expr sql.NullString
		if err = rows.Scan(&name, &uniqueness, &col, &expr); err != nil {
			return nil, fmt.Errorf("dalgo2oracle: read indexes scan: %w", err)
		}
		if e := strings.TrimSpace(expr.String); e != "" {
			col = e
			if m := quotedName.FindStringSubmatch(e); m != nil {
				col = m[1]
			}
		}
		if len(out) == 0 || out[len(out)-1].Name != name {
			out = append(out, dbschema.IndexDef{Name: name, Collection: table, Unique: uniqueness == "UNIQUE"})
		}
		out[len(out)-1].Fields = append(out[len(out)-1].Fields, dal.FieldName(col))
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("dalgo2oracle: read indexes rows: %w", err)
	}
	return out, nil
}
