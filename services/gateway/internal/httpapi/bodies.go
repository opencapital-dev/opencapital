package httpapi

// Request bodies for every write endpoint. NONE of these structs carry
// `org_id` — the gateway injects org_id from the verified JWT (ADR-0033).
// Client-supplied org_id at the top level is rejected up front by
// rejectTopLevelOrgID; nested fields like `portfolio_metadata.org_id` are
// untouched.

// portfolioEventBody is the JSON request shape for every endpoint that maps
// to portfolio_events.v2. event_type is derived from the URL path, not the
// body, so it doesn't appear here. The plugin passes the rendered payload as
// a JSON string; the gateway does not re-parse it.
type portfolioEventBody struct {
	SourceID     string            `json:"source_id"`
	PortfolioID  string            `json:"portfolio_id"`
	InstrumentID *string           `json:"instrument_id,omitempty"`
	BusinessTs   int64             `json:"business_ts"`
	TraceID      *string           `json:"trace_id,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Payload      string            `json:"payload"`
}

// dataBody is the request shape for /v6/data/{plugin_id}/{namespace}.
// source_namespace is derived from the URL path; plugin_id is similarly
// extracted from the path and checked against the JWT's plugin_id claim
// before any produce.
type dataBody struct {
	SourceID    string  `json:"source_id"`
	ObservedAt  int64   `json:"observed_at"`
	PortfolioID *string `json:"portfolio_id,omitempty"`
	TraceID     *string `json:"trace_id,omitempty"`
	Payload     string  `json:"payload"`
}
