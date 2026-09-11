package contexts

type contextKey string

const (
	ContextKeyPrincipal     contextKey = "_principal"
	ContextKeyProjectID     contextKey = "_project_id"
	ContextKeyTraceID       contextKey = "_trace_id"
	ContextKeyAuditResource contextKey = "_audit_resource"
	ContextKeyAuditMetadata contextKey = "_audit_metadata"
	contextKeyClientInfo    contextKey = "_client_info"
)
