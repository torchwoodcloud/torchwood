package functions

import (
	"context"
	"strings"

	appshared "github.com/torchwooddev/torchwood/internal/app/shared"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// secretMask 是 GetVariables 返回的 secret 值掩码（值仅在 SetVariables 可见）。
const secretMask = "******"

// SetVariables 全量替换函数环境变量（校验键名与总量上限）。
// 掩码约定：请求中值等于 secretMask 的 key 保留旧值不覆盖（key 不存在则跳过，
// 不创建）；其余 key 按新值写入。响应与 GetVariables 一致返回掩码视图，
// 真实值仅在 SetVariables 请求中可见一次。
// 变量是平台级敏感写操作：允许 admin 会话与 API key（scope 门禁），
// 拒绝端用户/匿名（纵深防御，G2-1；G12 产品决策 B 调整）。
func (f *Functions) SetVariables(ctx context.Context, projectID, functionID string, vars map[string]string) (map[string]string, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	fn, err := f.repo.GetFunction(ctx, projectID, functionID)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, status.Error(codes.NotFound, "function not found")
	}
	for k := range vars {
		if strings.TrimSpace(k) == "" || strings.ContainsAny(k, "\n\r\x00") {
			return nil, status.Errorf(codes.InvalidArgument, "invalid variable key %q", k)
		}
	}
	// 合并掩码保留后的最终值集合（掩码项回填旧值，未知 key 的掩码项丢弃）。
	current, err := f.repo.GetVariables(ctx, projectID, functionID)
	if err != nil {
		return nil, err
	}
	merged := make(map[string]string, len(vars))
	for k, v := range vars {
		if v == secretMask {
			if old, ok := current[k]; ok {
				merged[k] = old
			}
			continue
		}
		merged[k] = v
	}
	if envSize(merged) > maxEnvBytes {
		return nil, status.Errorf(codes.InvalidArgument, "environment variables exceed maximum total size of %d bytes", maxEnvBytes)
	}
	if err := f.repo.SetVariables(ctx, projectID, functionID, merged); err != nil {
		return nil, err
	}
	// 响应返回掩码视图：非空值一律脱敏，避免回显旧 secret。
	resp := make(map[string]string, len(merged))
	for k, v := range merged {
		if v != "" {
			resp[k] = secretMask
		} else {
			resp[k] = ""
		}
	}
	return resp, nil
}

func (f *Functions) GetVariables(ctx context.Context, projectID, functionID string) (map[string]string, error) {
	fn, err := f.repo.GetFunction(ctx, projectID, functionID)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, status.Error(codes.NotFound, "function not found")
	}
	vars, err := f.repo.GetVariables(ctx, projectID, functionID)
	if err != nil {
		return nil, err
	}
	for k, v := range vars {
		if v != "" {
			vars[k] = secretMask
		}
	}
	return vars, nil
}
