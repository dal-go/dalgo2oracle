package dalgo2oracle

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

const (
	foreignKeysSQL = `SELECT c.constraint_name, cc.column_name, r.owner, r.table_name, rc.column_name, c.delete_rule, c.status
FROM all_constraints c
JOIN all_cons_columns cc ON cc.owner = c.owner AND cc.constraint_name = c.constraint_name AND cc.table_name = c.table_name
JOIN all_constraints r ON r.owner = c.r_owner AND r.constraint_name = c.r_constraint_name
JOIN all_cons_columns rc ON rc.owner = r.owner AND rc.constraint_name = r.constraint_name
     AND rc.table_name = r.table_name AND rc.position = cc.position
WHERE c.owner = :owner AND c.table_name = :name AND c.constraint_type = 'R'
ORDER BY c.constraint_name, cc.position`

	constraintsSQL = `SELECT constraint_name, constraint_type, search_condition_vc
FROM all_constraints
WHERE owner = :owner AND table_name = :name AND constraint_type IN ('P', 'U', 'R', 'C')
ORDER BY constraint_type, constraint_name`

	referrersSQL = `SELECT DISTINCT c.table_name, cc.column_name
FROM all_constraints c
JOIN all_cons_columns cc ON cc.owner = c.owner AND cc.constraint_name = c.constraint_name AND cc.table_name = c.table_name
JOIN all_constraints r ON r.owner = c.r_owner AND r.constraint_name = c.r_constraint_name
WHERE c.constraint_type = 'R' AND c.owner = :owner AND r.owner = :owner AND r.table_name = :name
  AND c.table_name NOT LIKE 'BIN$%'
ORDER BY c.table_name, cc.column_name`
)

// readForeignKeys reads the outgoing foreign keys of a table. A key to a table
// of another schema names that schema in ReferencedNamespace. Oracle has no ON
// UPDATE action, so OnUpdate is "NO ACTION"; OnDelete is the delete rule.
// Enforcement is the constraint's status.
func (d *Database) readForeignKeys(ctx context.Context, owner string, args []any) ([]dbschema.ForeignKeyDef, error) {
	rows, err := d.sqlDB.QueryContext(ctx, foreignKeysSQL, args...)
	if err != nil {
		return nil, fmt.Errorf("dalgo2oracle: read foreign keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var keys []dbschema.ForeignKeyDef
	for rows.Next() {
		var name, from, targetOwner, target, to, onDelete, status string
		if err = rows.Scan(&name, &from, &targetOwner, &target, &to, &onDelete, &status); err != nil {
			return nil, fmt.Errorf("dalgo2oracle: read foreign keys scan: %w", err)
		}
		if len(keys) == 0 || keys[len(keys)-1].Name != name {
			namespace := targetOwner
			if namespace == owner {
				namespace = ""
			}
			enforcement := dbschema.ForeignKeyEnforcementDisabled
			if status == "ENABLED" {
				enforcement = dbschema.ForeignKeyEnforcementEnabled
			}
			keys = append(keys, dbschema.ForeignKeyDef{Name: name, ReferencedCollection: target, ReferencedNamespace: namespace,
				Enforcement: enforcement, OnUpdate: "NO ACTION", OnDelete: onDelete})
		}
		key := &keys[len(keys)-1]
		key.Fields = append(key.Fields, dal.FieldName(from))
		key.ReferencedFields = append(key.ReferencedFields, dal.FieldName(to))
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("dalgo2oracle: read foreign keys rows: %w", err)
	}
	return keys, nil
}

// notNullCheck is the check constraint Oracle records for a NOT NULL column.
var notNullCheck = regexp.MustCompile(`^"[^"]*" IS NOT NULL$`)

var constraintTypes = map[string]string{"P": "primary-key", "U": "unique", "R": "foreign-key", "C": "check"}

// ListConstraints returns the primary-key, unique, foreign-key and check
// constraints of a table, typed "primary-key", "unique", "foreign-key" and
// "check". The check constraints Oracle records for NOT NULL columns are left
// out: DescribeCollection reports them as non-nullable fields.
func (d *Database) ListConstraints(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.ConstraintDef, error) {
	rows, err := d.sqlDB.QueryContext(ctx, constraintsSQL, sql.Named("owner", d.owner(ref)), sql.Named("name", ref.Name()))
	if err != nil {
		return nil, fmt.Errorf("dalgo2oracle: ListConstraints %q: %w", ref.Name(), err)
	}
	defer func() { _ = rows.Close() }()
	var out []dbschema.ConstraintDef
	for rows.Next() {
		var name, ctype string
		var condition sql.NullString
		if err = rows.Scan(&name, &ctype, &condition); err != nil {
			return nil, fmt.Errorf("dalgo2oracle: ListConstraints scan: %w", err)
		}
		if ctype == "C" && notNullCheck.MatchString(strings.TrimSpace(condition.String)) {
			continue
		}
		out = append(out, dbschema.ConstraintDef{Name: name, Type: constraintTypes[ctype]})
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("dalgo2oracle: ListConstraints rows: %w", err)
	}
	return out, nil
}

// ListReferrers returns the tables of the schema whose foreign keys reference
// the table, each with its referencing columns.
func (d *Database) ListReferrers(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.Referrer, error) {
	rows, err := d.sqlDB.QueryContext(ctx, referrersSQL, sql.Named("owner", d.owner(ref)), sql.Named("name", ref.Name()))
	if err != nil {
		return nil, fmt.Errorf("dalgo2oracle: ListReferrers %q: %w", ref.Name(), err)
	}
	defer func() { _ = rows.Close() }()
	var out []dbschema.Referrer
	for rows.Next() {
		var table, col string
		if err = rows.Scan(&table, &col); err != nil {
			return nil, fmt.Errorf("dalgo2oracle: ListReferrers scan: %w", err)
		}
		if len(out) == 0 || out[len(out)-1].Collection.Name() != table {
			out = append(out, dbschema.Referrer{Collection: dal.NewRootCollectionRef(table, "")})
		}
		out[len(out)-1].Fields = append(out[len(out)-1].Fields, dal.FieldName(col))
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("dalgo2oracle: ListReferrers rows: %w", err)
	}
	return out, nil
}
