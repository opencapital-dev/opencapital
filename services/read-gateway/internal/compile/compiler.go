package compile

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/portfolio-management/read-gateway/internal/surface"
	"github.com/portfolio-management/read-gateway/internal/viewschema"
	"github.com/opencapital-dev/oc-plugin-sdk/dsl"
)

// schema is the column-introspection dependency the Compiler reads to validate
// matchers/projection and to enforce tenancy. *viewschema.Cache satisfies it;
// tests inject canned columns.
type schema interface {
	Columns(view string) ([]viewschema.Column, error)
}

// Compiler renders a parsed DSL selector to parameterized SQL using live view
// introspection instead of a hand-maintained catalog descriptor.
type Compiler struct {
	schema schema
}

// NewCompiler returns a Compiler backed by s (a *viewschema.Cache in production).
func NewCompiler(s schema) *Compiler { return &Compiler{schema: s} }

// Compile renders selector s to parameterized SQL. org is always injected as
// $1; portfolio (when the view is portfolio-scoped and portfolio is non-nil) is
// injected next. from/to are epoch microseconds compared directly against the
// int-us `ts` column. Ownership of the requested portfolio against the caller's
// org is verified by the HTTP handler before this is called.
func (c *Compiler) Compile(s dsl.Selector, org uuid.UUID, portfolio *uuid.UUID, pluginID string, from, to int64) (string, []any, error) {
	view, ok := surface.Resolve(s.Entity)
	if !ok {
		return "", nil, fmt.Errorf("entity %q is not queryable", s.Entity)
	}

	cols, err := c.schema.Columns(view)
	if err != nil {
		return "", nil, err
	}
	colSet := make(map[string]bool, len(cols))
	numericSet := make(map[string]bool)
	for _, col := range cols {
		colSet[col.Name] = true
		if col.Numeric {
			numericSet[col.Name] = true
		}
	}

	// Tenancy backstop: a view with no org_id column can never be safely scoped
	// to a tenant, so refuse it rather than emit an unscoped query.
	if !colSet["org_id"] {
		return "", nil, fmt.Errorf("view %q has no org_id column; refusing to emit an unscoped query", view)
	}

	var args []any
	add := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }

	where := []string{fmt.Sprintf("org_id = %s", add(org))}

	portfolioScoped := colSet["portfolio"] && portfolio != nil
	if portfolioScoped {
		where = append(where, fmt.Sprintf("portfolio = %s", add(*portfolio)))
	}

	if colSet["plugin_id"] {
		if pluginID == "" {
			return "", nil, fmt.Errorf("view %q is plugin-scoped but caller has no plugin_id", view)
		}
		where = append(where, fmt.Sprintf("plugin_id = %s", add(pluginID)))
	}

	// String matchers reference friendly columns directly. Skip a redundant
	// portfolio matcher when portfolio scope is already injected.
	for _, m := range s.Strings {
		if portfolioScoped && m.Label == "portfolio" {
			continue
		}
		if !colSet[m.Label] {
			return "", nil, fmt.Errorf("matcher column %q is not a column of view %q", m.Label, view)
		}
		op, oerr := strSQL(m.Op)
		if oerr != nil {
			return "", nil, oerr
		}
		where = append(where, fmt.Sprintf("%s %s %s", m.Label, op, add(m.Value)))
	}

	for _, m := range s.Numbers {
		if !colSet[m.Col] {
			return "", nil, fmt.Errorf("matcher column %q is not a column of view %q", m.Col, view)
		}
		if !numericSet[m.Col] {
			return "", nil, fmt.Errorf("numeric matcher on non-numeric column %q", m.Col)
		}
	}

	proj, perr := c.project(s, view, cols, colSet)
	if perr != nil {
		return "", nil, perr
	}
	colList := strings.Join(proj, ", ")

	hasTS := colSet["ts"]

	switch s.Mode {
	case dsl.Asof:
		for _, m := range s.Numbers {
			where = append(where, fmt.Sprintf("%s %s %s", m.Col, numSQL(m.Op), add(m.Value)))
		}
		if hasTS {
			where = append(where, fmt.Sprintf("ts <= %s", add(to)))
		}
		sql := fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY ts ASC",
			colList, view, strings.Join(where, " AND "))
		return sql, args, nil

	case dsl.Window:
		for _, m := range s.Numbers {
			where = append(where, fmt.Sprintf("%s %s %s", m.Col, numSQL(m.Op), add(m.Value)))
		}
		if hasTS {
			where = append(where, fmt.Sprintf("ts >= %s", add(from)))
			where = append(where, fmt.Sprintf("ts <= %s", add(to)))
		}
		sql := fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY ts ASC",
			colList, view, strings.Join(where, " AND "))
		return sql, args, nil

	case dsl.Latest:
		grain := surface.Grain(s.Entity)
		if len(grain) == 0 {
			return "", nil, fmt.Errorf("entity %q does not support @latest (no grain)", s.Entity)
		}
		for _, g := range grain {
			if !colSet[g] {
				return "", nil, fmt.Errorf("grain column %q is not a column of view %q", g, view)
			}
		}
		if hasTS {
			where = append(where, fmt.Sprintf("ts <= %s", add(to)))
		}
		grainList := strings.Join(grain, ", ")
		inner := fmt.Sprintf(
			"SELECT DISTINCT ON (%s) %s FROM %s WHERE %s ORDER BY %s, ts DESC",
			grainList, colList, view, strings.Join(where, " AND "), grainList,
		)
		if len(s.Numbers) == 0 {
			return inner, args, nil
		}
		// Numeric matchers apply to the already-deduplicated rows so they do not
		// interfere with the DISTINCT ON ordering.
		var outer []string
		for _, m := range s.Numbers {
			outer = append(outer, fmt.Sprintf("%s %s %s", m.Col, numSQL(m.Op), add(m.Value)))
		}
		sql := fmt.Sprintf("SELECT * FROM (%s) latest WHERE %s", inner, strings.Join(outer, " AND "))
		return sql, args, nil
	}

	return "", nil, fmt.Errorf("unknown mode %d", s.Mode)
}

// project returns the output column names: the selector's projection (each
// validated against the introspected columns) when present, else all introspected
// columns in their introspection order.
func (c *Compiler) project(s dsl.Selector, view string, cols []viewschema.Column, colSet map[string]bool) ([]string, error) {
	if len(s.Project) == 0 {
		names := make([]string, len(cols))
		for i, col := range cols {
			names[i] = col.Name
		}
		return names, nil
	}
	names := make([]string, len(s.Project))
	for i, col := range s.Project {
		if !colSet[col] {
			return nil, fmt.Errorf("projection column %q is not a column of view %q", col, view)
		}
		names[i] = col
	}
	return names, nil
}

func strSQL(op dsl.StringOp) (string, error) {
	switch op {
	case dsl.OpEq:
		return "=", nil
	case dsl.OpNe:
		return "<>", nil
	case dsl.OpReMatch:
		return "~", nil
	case dsl.OpReNoMatch:
		return "!~", nil
	default:
		return "", fmt.Errorf("unknown string op %d", op)
	}
}

func numSQL(op dsl.NumOp) string {
	switch op {
	case dsl.NumEq:
		return "="
	case dsl.NumNe:
		return "!="
	case dsl.NumGt:
		return ">"
	case dsl.NumGe:
		return ">="
	case dsl.NumLt:
		return "<"
	default: // NumLe
		return "<="
	}
}
