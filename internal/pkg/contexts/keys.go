package contexts

type contextKey string

const (
	// torchwood 特有槽位（Principal/ProjectID 与审计元数据桥接层）留在
	// 本包；ClientInfo 与审计资源槽的存储机制已委托 grpcapi/contextx
	// （grpcapi 阶段 1 门面化）。ContextKeyAuditResource 仅作为无审计槽
	// 直调场景的不可变回退载体。
	ContextKeyPrincipal     contextKey = "_principal"
	ContextKeyProjectID     contextKey = "_project_id"
	ContextKeyAuditResource contextKey = "_audit_resource"
	ContextKeyAuditMetadata contextKey = "_audit_metadata"
)
