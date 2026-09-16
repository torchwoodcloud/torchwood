package clientgrpc

import (
	"context"
	"encoding/json"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	"github.com/torchwoodcloud/torchwood/internal/app/client"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RuntimeVarsService 是 client 面运行时变量拉取的 gRPC handler（docs/design/
// runtime-vars.md §2.3/§2.6）：resolveProjectID 三级回落（请求字段 →
// X-Torchwood-Project → Principal，与 databases.go 同链）+ proto ↔ domain
// 值映射；凭证布尔与可见性过滤在 app 用例与 port 实现内。
type RuntimeVarsService struct {
	clientv1.UnimplementedRuntimeVarsServiceServer
	runtimeVars *client.RuntimeVars
}

func NewRuntimeVarsService(runtimeVars *client.RuntimeVars) *RuntimeVarsService {
	return &RuntimeVarsService{runtimeVars: runtimeVars}
}

func (s *RuntimeVarsService) GetRuntimeVars(ctx context.Context, req *clientv1.GetRuntimeVarsRequest) (*clientv1.GetRuntimeVarsResponse, error) {
	projectID, err := resolveProjectID(ctx, req.GetProjectId())
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, "runtime_var_sets/"+req.GetVarSetId())
	snap, err := s.runtimeVars.GetRuntimeVars(ctx, projectID, req.GetVarSetId(), req.GetEtag())
	if err != nil {
		return nil, err
	}
	out := &clientv1.GetRuntimeVarsResponse{
		Etag:      snap.ETag,
		Unchanged: snap.Unchanged,
	}
	if !snap.Unchanged {
		out.Vars = make(map[string]*clientv1.RuntimeVarValue, len(snap.Vars))
		for _, v := range snap.Vars {
			value, err := runtimeVarValueFromStored(v.ValueType, v.Value)
			if err != nil {
				return nil, err
			}
			out.Vars[v.Key] = value
		}
	}
	return out, nil
}

// runtimeVarValueFromStored 把存储侧（value_type, JSON 文本）还原为 client 面
// oneof（client 侧独立定义的 RuntimeVarValue，与 server 面同形；存储侧值必为
// 写路径构造的合法 JSON，此处失败 = 数据损坏 → Internal）。
func runtimeVarValueFromStored(valueType, valueJSON string) (*clientv1.RuntimeVarValue, error) {
	switch valueType {
	case projects.RuntimeVarTypeString:
		var s string
		if err := json.Unmarshal([]byte(valueJSON), &s); err != nil {
			return nil, status.Errorf(codes.Internal, "corrupt runtime var value: %v", err)
		}
		return &clientv1.RuntimeVarValue{Kind: &clientv1.RuntimeVarValue_StringValue{StringValue: s}}, nil
	case projects.RuntimeVarTypeInteger:
		var n int64
		if err := json.Unmarshal([]byte(valueJSON), &n); err != nil {
			return nil, status.Errorf(codes.Internal, "corrupt runtime var value: %v", err)
		}
		return &clientv1.RuntimeVarValue{Kind: &clientv1.RuntimeVarValue_IntegerValue{IntegerValue: n}}, nil
	case projects.RuntimeVarTypeFloat:
		var f float64
		if err := json.Unmarshal([]byte(valueJSON), &f); err != nil {
			return nil, status.Errorf(codes.Internal, "corrupt runtime var value: %v", err)
		}
		return &clientv1.RuntimeVarValue{Kind: &clientv1.RuntimeVarValue_FloatValue{FloatValue: f}}, nil
	case projects.RuntimeVarTypeBoolean:
		var b bool
		if err := json.Unmarshal([]byte(valueJSON), &b); err != nil {
			return nil, status.Errorf(codes.Internal, "corrupt runtime var value: %v", err)
		}
		return &clientv1.RuntimeVarValue{Kind: &clientv1.RuntimeVarValue_BoolValue{BoolValue: b}}, nil
	case projects.RuntimeVarTypeJSON:
		return &clientv1.RuntimeVarValue{Kind: &clientv1.RuntimeVarValue_JsonValue{JsonValue: valueJSON}}, nil
	default:
		return nil, status.Errorf(codes.Internal, "unknown runtime var value type %q", valueType)
	}
}
