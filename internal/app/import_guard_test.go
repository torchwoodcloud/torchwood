package app

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// infraImportAllowlist 是 app 用例层现存 internal/infra import 的棘轮白名单：
// 只许缩减（修复一项删一行），不许新增。新依赖必须经 domain 端口或组合根
// Wire 注入进入用例层（W-A 工作流，docs/review/arch-review-2026-08-fix-plan.md；
// domain 层同款守卫见 internal/domain/assets/service_test.go）。
var infraImportAllowlist = map[string]string{
	"client/email_otp.go":    "W-A 待办：OTP 用例调 infra/auth.GenerateOTP（纯函数），应下沉 pkg",
	"client/mfa.go":          "W-A 待办：MFA 用例自建 infra/auth 适配器，应改 Wire 注入工厂",
	"client/oauth2.go":       "W-A 待办：OAuth 用例自建 infra/auth 适配器，应改 Wire 注入工厂",
	"client/phone_otp.go":    "W-A 待办：OTP 用例调 infra/auth.GenerateOTP（纯函数），应下沉 pkg",
	"client/wechat.go":       "W-A 待办：微信换码静态调 infra/auth，应改注入",
	"client/test_helpers.go": "测试装配脚手架（无 _test 后缀随包编译），改构建约束待办",
}

// TestNoInfraImports 守卫分层纪律：internal/app 非测试源码不得 import
// internal/infra（用例层依赖适配器层是方向违规）。
func TestNoInfraImports(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(p, "github.com/torchwoodcloud/torchwood/internal/infra") {
				continue
			}
			rel := filepath.ToSlash(path)
			if _, ok := infraImportAllowlist[rel]; ok {
				continue
			}
			t.Errorf("%s imports forbidden infra package %s：app 层不得依赖 infra，经 domain 端口或 Wire 注入；修复白名单内条目时请同步删除", rel, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
