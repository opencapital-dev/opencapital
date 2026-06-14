package httpapi

type Binding struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Selector string `json:"selector"`
}
type QueryRequest struct {
	From       int64     `json:"from"`
	To         int64     `json:"to"`
	Bindings   []Binding `json:"bindings"`
	OutputMode string    `json:"outputMode"` // table|timeseries
}

// RowsRequest is the /v1/rows wire request. Mode is a pointer so JSON null
// (DSL default) is distinct from an absent field.
type RowsRequest struct {
	Selector string  `json:"selector"`
	Mode     *string `json:"mode"`
	From     int64   `json:"from"`
	To       int64   `json:"to"`
}

// RowsResponse is the /v1/rows wire response: ordered column names and row
// arrays in that column order. Cells serialize as-is (numbers as JSON numbers,
// SQL NULLs as JSON null).
type RowsResponse struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// Column and Result are the read-gateway's transport types for the /query path.
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type Result struct {
	Mode    string   `json:"mode"`
	Columns []Column `json:"columns"`
	Rows    [][]any  `json:"rows"`
}
