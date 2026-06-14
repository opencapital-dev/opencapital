package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/opencapital-dev/oc-plugin-sdk/dsl"
	"github.com/portfolio-management/read-gateway/internal/compile"
	"github.com/portfolio-management/read-gateway/internal/rw"
	"github.com/portfolio-management/read-gateway/internal/surface"
	"github.com/portfolio-management/read-gateway/internal/viewschema"
)

type Verifier interface {
	OrgFromBearer(ctx context.Context, bearer string) (uuid.UUID, error)
	Identify(ctx context.Context, bearer string) (uuid.UUID, string, error)
}
type Ownership interface {
	Owns(ctx context.Context, org uuid.UUID, bearer string, p uuid.UUID) (bool, error)
	Portfolios(ctx context.Context, org uuid.UUID, bearer string) (map[uuid.UUID]string, error)
}
type Reader interface {
	Query(ctx context.Context, sql string, args ...any) (rw.Rows, error)
}

// Schema is satisfied by *viewschema.Cache; tests inject a stub.
type Schema interface {
	Columns(view string) ([]viewschema.Column, error)
}

type Server struct {
	Verifier  Verifier
	Ownership Ownership
	Reader    Reader
	Schema    Schema
	Compiler  *compile.Compiler
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /query", s.handleQuery)
	mux.HandleFunc("POST /v1/rows", s.handleRows)
	return mux
}

// handleRows returns one binding's org-scoped rows for the compute service,
// reusing the compute path's auth + fetchRows scoping. The selector arrives
// without its @mode (stripped by the client); it is reconstructed before fetch.
func (s *Server) handleRows(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if token == "" {
		httpErr(w, http.StatusUnauthorized, "missing bearer")
		return
	}
	org, pluginID, err := s.Verifier.Identify(ctx, token)
	if err != nil {
		httpErr(w, http.StatusUnauthorized, "invalid token: "+err.Error())
		return
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req RowsRequest
	if err := dec.Decode(&req); err != nil {
		httpErr(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}

	selector := req.Selector
	if req.Mode != nil && *req.Mode != "" {
		selector += " @" + *req.Mode
	}

	rows, status, err := s.fetchRows(ctx, org, pluginID, token, selector, req.From, req.To)
	if err != nil {
		httpErr(w, status, err.Error())
		return
	}
	out := rows.Rows
	if out == nil {
		out = [][]any{}
	}
	writeJSON(w, http.StatusOK, RowsResponse{Columns: rows.Columns, Rows: out})
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if token == "" {
		httpErr(w, http.StatusUnauthorized, "missing bearer")
		return
	}
	org, pluginID, err := s.Verifier.Identify(ctx, token)
	if err != nil {
		httpErr(w, http.StatusUnauthorized, "invalid token: "+err.Error())
		return
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req QueryRequest
	if err := dec.Decode(&req); err != nil {
		httpErr(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	if len(req.Bindings) != 1 {
		httpErr(w, http.StatusBadRequest, "requires exactly one binding")
		return
	}

	res, status, err := s.fetch(ctx, org, pluginID, token, req.Bindings[0], req.OutputMode, req.From, req.To)
	if err != nil {
		httpErr(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// fetchRows resolves one selector to scoped RW rows.
// Returns (rows, httpStatus, error).
func (s *Server) fetchRows(ctx context.Context, org uuid.UUID, pluginID, token, selector string, from, to int64) (rw.Rows, int, error) {
	sel, err := dsl.Parse(selector)
	if err != nil {
		return rw.Rows{}, http.StatusBadRequest, err
	}

	// portfolios is served from the control-plane, not from RW — same carve-out
	// as /query, so the compute path (e.g. a variable passthrough metric) can
	// bind it too.
	if sel.Entity == "portfolios" {
		ps, perr := s.Ownership.Portfolios(ctx, org, token)
		if perr != nil {
			return rw.Rows{}, http.StatusBadGateway, perr
		}
		out := rw.Rows{Columns: []string{"portfolio_id", "name"}}
		for id, name := range ps {
			out.Rows = append(out.Rows, []any{id.String(), name})
		}
		return out, http.StatusOK, nil
	}

	portfolio, status, err := s.checkPortfolioOwnership(ctx, sel, org, token)
	if err != nil {
		return rw.Rows{}, status, err
	}

	sql, args, cerr := s.Compiler.Compile(sel, org, portfolio, pluginID, from, to)
	if cerr != nil {
		return rw.Rows{}, http.StatusBadRequest, cerr
	}
	rows, qerr := s.Reader.Query(ctx, sql, args...)
	if qerr != nil {
		return rw.Rows{}, http.StatusBadGateway, qerr
	}
	return rows, http.StatusOK, nil
}

// fetch resolves one binding to a neutral Result. Returns (result, httpStatus, error).
func (s *Server) fetch(ctx context.Context, org uuid.UUID, pluginID, token string, b Binding, mode string, from, to int64) (Result, int, error) {
	sel, err := dsl.Parse(b.Selector)
	if err != nil {
		return Result{}, http.StatusBadRequest, err
	}

	// portfolios is served from the control-plane, not from RW.
	if sel.Entity == "portfolios" {
		ps, perr := s.Ownership.Portfolios(ctx, org, token)
		if perr != nil {
			return Result{}, http.StatusBadGateway, perr
		}
		if mode == "" {
			mode = "table"
		}
		res := Result{Mode: mode, Columns: []Column{{Name: "portfolio_id", Type: "string"}, {Name: "name", Type: "string"}}}
		for id, name := range ps {
			res.Rows = append(res.Rows, []any{id.String(), name})
		}
		return res, http.StatusOK, nil
	}

	portfolio, status, err := s.checkPortfolioOwnership(ctx, sel, org, token)
	if err != nil {
		return Result{}, status, err
	}

	if mode == "" {
		mode = "table"
	}

	sql, args, cerr := s.Compiler.Compile(sel, org, portfolio, pluginID, from, to)
	if cerr != nil {
		return Result{}, http.StatusBadRequest, cerr
	}
	rows, qerr := s.Reader.Query(ctx, sql, args...)
	if qerr != nil {
		return Result{}, http.StatusBadGateway, qerr
	}
	return s.rowsToResult(mode, rows, sel.Entity), http.StatusOK, nil
}

// checkPortfolioOwnership scans sel for a portfolio= matcher and verifies the
// portfolio belongs to the caller's org. Returns the resolved *uuid.UUID (nil
// when no portfolio matcher is present) or a non-nil error with the appropriate
// HTTP status.
func (s *Server) checkPortfolioOwnership(ctx context.Context, sel dsl.Selector, org uuid.UUID, token string) (*uuid.UUID, int, error) {
	for _, m := range sel.Strings {
		if m.Label != "portfolio" {
			continue
		}
		if m.Op != dsl.OpEq {
			return nil, http.StatusBadRequest, fmt.Errorf("scope label %q only supports '='", m.Label)
		}
		pid, perr := uuid.Parse(m.Value)
		if perr != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("portfolio %q is not a uuid", m.Value)
		}
		owned, oerr := s.Ownership.Owns(ctx, org, token, pid)
		if oerr != nil {
			return nil, http.StatusBadGateway, oerr
		}
		if !owned {
			return nil, http.StatusForbidden, fmt.Errorf("portfolio not in org")
		}
		return &pid, http.StatusOK, nil
	}
	return nil, http.StatusOK, nil
}

// rowsToResult builds a neutral Result from RW rows. Column type is derived
// from the view's introspected schema: numeric columns map to "number", all
// others to "string".
func (s *Server) rowsToResult(mode string, rows rw.Rows, entity string) Result {
	numericByName := s.numericColumns(entity)
	res := Result{Mode: mode}
	for _, c := range rows.Columns {
		t := "string"
		if numericByName[c] {
			t = "number"
		}
		res.Columns = append(res.Columns, Column{Name: c, Type: t})
	}
	res.Rows = rows.Rows
	if res.Rows == nil {
		res.Rows = [][]any{}
	}
	return res
}

// numericColumns returns the set of numeric column names for an entity's
// backing view by consulting the cached schema. Returns nil on any lookup
// failure (safe: columns default to "string").
func (s *Server) numericColumns(entity string) map[string]bool {
	view, ok := surface.Resolve(entity)
	if !ok {
		return nil
	}
	cols, err := s.Schema.Columns(view)
	if err != nil {
		return nil
	}
	out := make(map[string]bool, len(cols))
	for _, c := range cols {
		if c.Numeric {
			out[c.Name] = true
		}
	}
	return out
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
