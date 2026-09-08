package bunrepo

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// updateGuardExceptions 登记豁免"UPDATE 必须声明列"检查的位置，键为相对
// internal/infra 的 "file:line"。每条豁免必须在注释说明理由与等价的防误写
// 手段。当前为空——不要新增。
var updateGuardExceptions = map[string]bool{}

// TestUpdateQueriesDeclareColumns（bun 更新写规范护栏，2026-09-08 dev 事故）：
// infra 层任何 bun UPDATE 查询必须显式声明写入列——struct 模型走 .Column(...)
// 白名单，nil 模型走 .Set(...)。裸全模型覆盖 UPDATE（struct + WherePK 全列写）
// 会把零值/nil 字段渲染成 SET col = DEFAULT：projects.internal_id（identity
// 列）因此每次更新烧一个序列号并改写数据面租户号（_tenant / roles_sig
// Tenant 的唯一来源）。
//
// 静态扫描 internal/infra 下全部非 _test Go 文件；"变量承接"形态
// （q := NewUpdate()...; q = q.Set(...)）通过函数内标识符追踪识别。documentdb
// 等原生 SQL 更新不经 bun 查询构造器，天然不在扫描面内。
func TestUpdateQueriesDeclareColumns(t *testing.T) {
	root := "../.." // 本文件位于 internal/infra/bun/bunrepo，../.. = internal/infra

	var violations []string
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if name == "testdata" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}

		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}

		parents, calls := collectNodes(file)
		for _, call := range calls {
			if key, ok := violationFor(fset, path, parents, call); ok && !updateGuardExceptions[key] {
				violations = append(violations,
					key+": bun UPDATE 未声明写入列（.Column 白名单 / .Set），见 AGENTS.md bun 更新写规范")
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("scan %s: %v", root, walkErr)
	}
	for _, v := range violations {
		t.Error(v)
	}
}

// collectNodes 建立父节点映射并收集全部 NewUpdate 调用表达式。
func collectNodes(file *ast.File) (map[ast.Node]ast.Node, []*ast.CallExpr) {
	parents := map[ast.Node]ast.Node{}
	var newUpdates []*ast.CallExpr
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) > 0 {
			parents[n] = stack[len(stack)-1]
		}
		stack = append(stack, n)
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewUpdate" {
				newUpdates = append(newUpdates, call)
			}
		}
		return true
	})
	return parents, newUpdates
}

// violationFor 判定单个 NewUpdate 链是否违规，返回位置键与判定结果。
func violationFor(fset *token.FileSet, path string, parents map[ast.Node]ast.Node, call *ast.CallExpr) (string, bool) {
	key := relPos(fset, path, call.Pos())

	outer := call // 爬到链的最外层（NewUpdate().Model(...)...Exec(...)）
	for {
		// 链式调用的 AST 中间隔着 SelectorExpr 节点：CallExpr(NewUpdate) 的
		// 父是 SelectorExpr(Model)，其父才是 CallExpr(Model)。
		p, ok := parents[outer]
		if !ok {
			break
		}
		sel, isSel := p.(*ast.SelectorExpr)
		if !isSel {
			break
		}
		p2, ok := parents[sel]
		if !ok {
			break
		}
		pc, isCall := p2.(*ast.CallExpr)
		if !isCall || pc.Fun != sel {
			break
		}
		outer = pc
	}

	// 链内已声明列即合规。
	if chainDeclaresColumns(outer) {
		return key, false
	}

	// 变量承接：追踪同函数内对承接变量的 .Column/.Set 调用。
	if varName, ok := assignedVar(parents, outer); ok {
		if body := enclosingBody(parents, outer); body != nil && varSetLater(body, varName) {
			return key, false
		}
	}
	return key, true
}

// chainDeclaresColumns 报告调用链（自最外层向下）是否含 .Column/.Set。
func chainDeclaresColumns(outer *ast.CallExpr) bool {
	cur := outer
	for {
		sel, ok := cur.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		if sel.Sel.Name == "Column" || sel.Sel.Name == "Set" {
			return true
		}
		xc, ok := sel.X.(*ast.CallExpr)
		if !ok {
			return false
		}
		cur = xc
	}
}

// assignedVar 返回链结果的承接变量名（q := NewUpdate()... 中的 q）。
func assignedVar(parents map[ast.Node]ast.Node, outer *ast.CallExpr) (string, bool) {
	p, ok := parents[outer]
	if !ok {
		return "", false
	}
	as, ok := p.(*ast.AssignStmt)
	if !ok {
		return "", false
	}
	for i, rhs := range as.Rhs {
		if rhs != outer || i >= len(as.Lhs) {
			continue
		}
		if id, ok := as.Lhs[i].(*ast.Ident); ok && id.Name != "_" {
			return id.Name, true
		}
	}
	return "", false
}

// enclosingBody 返回节点所在的函数体（FuncDecl 或 FuncLit）。
func enclosingBody(parents map[ast.Node]ast.Node, n ast.Node) *ast.BlockStmt {
	for {
		p, ok := parents[n]
		if !ok {
			return nil
		}
		switch fn := p.(type) {
		case *ast.FuncDecl:
			return fn.Body
		case *ast.FuncLit:
			return fn.Body
		}
		n = p
	}
}

// varSetLater 报告函数体内是否存在对 varName 的 .Column/.Set 调用
// （含 q = q.Model(...).Set(...) 再赋值形态）。
func varSetLater(body *ast.BlockStmt, varName string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found || n == nil {
			return !found
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Column" && sel.Sel.Name != "Set") {
			return true
		}
		if chainReferencesIdent(sel.X, varName) {
			found = true
		}
		return !found
	})
	return found
}

// chainReferencesIdent 报告接收者链最左侧是否为指定标识符
// （q.Set / q.Model(x).Set 两种形态均命中）。
func chainReferencesIdent(x ast.Node, name string) bool {
	for {
		switch t := x.(type) {
		case *ast.Ident:
			return t.Name == name
		case *ast.SelectorExpr:
			x = t.X
		case *ast.CallExpr:
			sel, ok := t.Fun.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			x = sel.X
		default:
			return false
		}
	}
}

func relPos(fset *token.FileSet, path string, pos token.Pos) string {
	return path + ":" + strconv.Itoa(fset.Position(pos).Line)
}
