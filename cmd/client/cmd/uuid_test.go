package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/lynx-go/commands"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestUUIDCmdNoAPIKey：uuid 动词以 noKey 声明（Run 内按豁免校验）。
func TestUUIDCmdNoAPIKey(t *testing.T) {
	g := &globalFlags{output: "json", timeout: "30s"}
	if err := g.validate(false); err != nil {
		t.Fatalf("uuid 命令应豁免 api-key 校验：%v", err)
	}
}

// TestUUIDCmdPrintsUniqueUUIDv4：输出经 env.Stdout（可注入），两次生成
// 均为合法 UUID v4 且互不相同，以换行结尾。
func TestUUIDCmdPrintsUniqueUUIDv4(t *testing.T) {
	v := newUUIDCmd(&globalFlags{output: "json", timeout: "30s"})
	run := func() (string, error) {
		var buf bytes.Buffer
		err := v.Run(context.Background(), &commands.Environment{Stdout: &buf}, nil)
		return buf.String(), err
	}
	a, err := run()
	require.NoError(t, err)
	b, err := run()
	require.NoError(t, err)
	idA := strings.TrimSpace(a)
	idB := strings.TrimSpace(b)
	_, err = uuid.Parse(idA)
	require.NoError(t, err, "输出不是合法 UUID：%q", idA)
	require.NotEqual(t, idA, idB, "连续两次生成应不同")
	require.True(t, strings.HasSuffix(a, "\n"), "应输出换行，got %q", a)
}
