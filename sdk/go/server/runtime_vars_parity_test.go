package server

import (
	"testing"

	"github.com/stretchr/testify/require"
	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// runtimeVarValueOneofShape 收集 RuntimeVarValue 唯一 oneof 的字段形状：
// 字段名 → (编号, 类型)。两包（server/client）的 RuntimeVarValue 是 client
// 侧独立定义的同形副本（client/server 资源类型互不 import 是仓库现状，
// docs/design/runtime-vars.md §2.3），本测试锚定两副本不单边漂移。
type oneofField struct {
	number protoreflect.FieldNumber
	kind   protoreflect.Kind
}

func runtimeVarValueOneofShape(t *testing.T, m protoreflect.ProtoMessage) map[string]oneofField {
	t.Helper()
	oneofs := m.ProtoReflect().Descriptor().Oneofs()
	require.Equal(t, 1, oneofs.Len(), "RuntimeVarValue 应恰有一个 oneof（kind）")
	fields := oneofs.Get(0).Fields()
	out := make(map[string]oneofField, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		out[string(f.Name())] = oneofField{number: f.Number(), kind: f.Kind()}
	}
	return out
}

// TestRuntimeVarValueParity 反射对比 serverv1 与 clientv1 的 RuntimeVarValue：
// oneof 字段集、字段编号与类型三方一致（阶段 3 遗留的 SDK 同形锚定）。任一
// 侧 proto 改动 oneof（增删分支/换编号/换类型）而另一侧未跟随时失败。
func TestRuntimeVarValueParity(t *testing.T) {
	serverShape := runtimeVarValueOneofShape(t, &serverv1.RuntimeVarValue{})
	clientShape := runtimeVarValueOneofShape(t, &clientv1.RuntimeVarValue{})
	require.Equal(t, serverShape, clientShape,
		"serverv1/clientv1 RuntimeVarValue oneof 同形被破坏：两包需同步修改 proto 并重新生成")
}
