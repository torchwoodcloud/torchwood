package contexts

type contextKey string

const (
	ContextKeyPrincipal     contextKey = "_principal"
	ContextKeyProjectID     contextKey = "_project_id"
	ContextKeyTraceID       contextKey = "_trace_id"
	ContextKeyAuditResource contextKey = "_audit_resource"
	contextKeyClientInfo    contextKey = "_client_info"
)
