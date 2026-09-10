package contexts

import "context"

// ClientInfo captures request metadata useful for session and audit records.
type ClientInfo struct {
	IP        string
	UserAgent string
}

func WithClientInfo(ctx context.Context, info ClientInfo) context.Context {
	return context.WithValue(ctx, contextKeyClientInfo, info)
}

func ClientInfoFrom(ctx context.Context) ClientInfo {
	v, _ := ctx.Value(contextKeyClientInfo).(ClientInfo)
	return v
}
