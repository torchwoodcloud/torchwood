// RLS 判定执行点（redesign §3.2/§4.3，阶段③包 C）：用户集合表（c_*）建表时
// 生成四条 policy（RLS-as-code 模板），DDL touch 路径顺带 reconcile（对齐
// ensureTenantCreatedIndex 模式）。集合级权限经 (SELECT ...) 标量子查询实时读
// catalog——权限变更零 DDL；所有跨表取值 InitPlan 化（§3.2 工程纪律）。
//
// 四条 policy（预决策 4）：
//
//	SELECT  USING = tw_visible（可写即可读，产品语义）
//	INSERT  WITH CHECK = 集合级 create（catalog 子查询）
//	UPDATE  USING = CASE docsec → tw_can(update) ELSE 集合级 update；
//	        WITH CHECK = 恒真（保现状"允许自锁"——C1 种子与事件写前快照依赖）
//	DELETE  USING = CASE docsec → tw_can(delete) ELSE 集合级 delete
//
// 全表 ENABLE + FORCE ROW LEVEL SECURITY（owner 亦受 policy，仅 BYPASSRLS 旁路）。
// 列级 GRANT：tw_app 仅 SELECT 全列 + INSERT/UPDATE 数据列与除 _tenant 外的
// 系统列（_tenant 锁死不可写，预决策 6）；tw_system 表级 ALL。
package documentdb

import (
	"context"
	"fmt"
	"strings"

	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
)

// policyCatalogLookup 生成按 (project, database, 物理名) 点查 catalog_collections
// 的标量子查询（InitPlan 化：无行变量相关性，规划器转为每语句一次）。
// 物理名 = collectionID 后（2026-09-06 勘误）跨项目/跨库同名合法，子查询必须
// 三元组等值收窄，否则同名集合 >1 行触发 21000。
func policyCatalogLookup(schema, physical, expr string) string {
	projectID, databaseID, err := ident.ParseSchemaName(schema)
	if err != nil {
		// 理论不可达：schema 由 ident.SchemaName 生成。保留断言作纵深防御，
		// 不让非法 schema 静默生成宽匹配 policy。
		panic(fmt.Sprintf("rls policy: parse schema %q: %v", schema, err))
	}
	return fmt.Sprintf(
		`(SELECT %s FROM public.catalog_collections cc WHERE cc.project_id = '%s' AND cc.database_id = '%s' AND cc.physical_name = '%s')`,
		expr, escapeSQLStringLiteral(projectID), escapeSQLStringLiteral(databaseID), escapeSQLStringLiteral(physical))
}

// rlsPolicySQL 生成 <tbl> 的四条 policy 语句（DROP IF EXISTS + CREATE，幂等重建）。
// SELECT policy 的 USING 在 tw_visible 之前内联空 _acl 快速路径（与 tw_visible
// 内部同源）：绝大多数行（空 _acl + 集合级任一授权）免函数调用短路——大集合
// 全扫的相对基准由它压住（I1）；非空 _acl 行回落 tw_visible 完整判定。
func rlsPolicySQL(schema, physical string) []string {
	tbl := tableName(schema, physical)
	roles := `(SELECT public.tw_roles())`
	docsec := policyCatalogLookup(schema, physical, "cc.document_security")
	collAllows := func(typ string) string {
		return policyCatalogLookup(schema, physical, fmt.Sprintf("public.tw_coll_allows(cc.permissions, public.tw_roles(), '%s')", typ))
	}
	fastPath := fmt.Sprintf(
		`COALESCE(cardinality(_acl) = 0 AND cardinality(%s) > 0 AND (%s OR %s OR %s), false)`,
		roles, collAllows("read"), collAllows("update"), collAllows("delete"))
	return []string{
		fmt.Sprintf(`DROP POLICY IF EXISTS tw_select ON %s`, tbl),
		fmt.Sprintf(`CREATE POLICY tw_select ON %s FOR SELECT USING (
			%s OR public.tw_visible(_acl, %s, %s, %s, %s, %s))`,
			tbl, fastPath, roles, docsec, collAllows("read"), collAllows("update"), collAllows("delete")),

		fmt.Sprintf(`DROP POLICY IF EXISTS tw_insert ON %s`, tbl),
		fmt.Sprintf(`CREATE POLICY tw_insert ON %s FOR INSERT WITH CHECK (%s)`,
			tbl, collAllows("create")),

		fmt.Sprintf(`DROP POLICY IF EXISTS tw_update ON %s`, tbl),
		fmt.Sprintf(`CREATE POLICY tw_update ON %s FOR UPDATE USING (
			CASE WHEN %s THEN public.tw_can(_acl, %s, 'update', %s) ELSE %s END)
			WITH CHECK (true)`,
			tbl, docsec, roles, collAllows("update"), collAllows("update")),

		fmt.Sprintf(`DROP POLICY IF EXISTS tw_delete ON %s`, tbl),
		fmt.Sprintf(`CREATE POLICY tw_delete ON %s FOR DELETE USING (
			CASE WHEN %s THEN public.tw_can(_acl, %s, 'delete', %s) ELSE %s END)`,
			tbl, docsec, roles, collAllows("delete"), collAllows("delete")),
	}
}

// escapeSQLStringLiteral 转义内嵌 SQL 字面量（physical_name 为服务端生成的
// c_<base32>，本无引号；防御性转义保持模板不可注入）。
func escapeSQLStringLiteral(s string) string {
	return strings.ReplaceAll(s, `'`, `''`)
}

// ensureCollectionRLS 为用户集合表生成 policy + FORCE RLS + 列级 GRANT（幂等，
// DDL touch 汇聚点 reconcileVersionColumn 与建表路径共用）。
func (p *postgresDocumentDB) ensureCollectionRLS(ctx context.Context, schema, physical string) error {
	tbl := tableName(schema, physical)
	for _, stmt := range rlsPolicySQL(schema, physical) {
		if _, err := p.conn(ctx).ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("ensure rls policy: %w", err)
		}
	}
	if _, err := p.conn(ctx).ExecContext(ctx, fmt.Sprintf(
		`ALTER TABLE %s ENABLE ROW LEVEL SECURITY; ALTER TABLE %s FORCE ROW LEVEL SECURITY`, tbl, tbl)); err != nil {
		return fmt.Errorf("enable row level security: %w", err)
	}
	return p.refreshColumnGrants(ctx, schema, physical)
}

// refreshColumnGrants 重刷列级 GRANT（会话 #10 从 ensureCollectionRLS 抽出）：
// 列清单读 information_schema，授权形态为 REVOKE ALL 后按清单重授（幂等重建
// ——存量表的旧授权形态在 DDL touch 时被矫正）。CreateAttribute 在 ADD COLUMN
// 之后调用，使新列（含 vector）立即获得 INSERT/UPDATE 列授权。
func (p *postgresDocumentDB) refreshColumnGrants(ctx context.Context, schema, physical string) error {
	tbl := tableName(schema, physical)
	// 列级 GRANT（预决策 6 + R13a + R16 ③）：SELECT 全列（WHERE _tenant 过滤
	// 与 to_jsonb(d.*) 载荷需要）；INSERT 授数据列 + 除 _tenant 外系统列（含
	// _acl——create/upsert 插入支的 _acl 随行携带：新行无旧行、可见性校验
	// 不适用，内容治理在 app 层授予校验，信任等价于"自己创建的内容"）；UPDATE
	// 排除 _acl——_acl 的变更通道唯一化为 tw_set_document_acl（000004，SECURITY
	// DEFINER owner=tw_system，函数内租户绑定 + 可见性校验），应用身份直改
	// _acl 的旁路从 UPDATE 列权限上封死（非自锁变更可过 SELECT policy 新行
	// 复检，必须不可达）。
	cols, err := p.tableColumns(ctx, schema, physical)
	if err != nil {
		return err
	}
	insertCols := make([]string, 0, len(cols))
	updateCols := make([]string, 0, len(cols))
	for _, c := range cols {
		if c == "_tenant" {
			continue
		}
		insertCols = append(insertCols, quoteIdent(c))
		if c != "_acl" {
			updateCols = append(updateCols, quoteIdent(c))
		}
	}
	insertList := strings.Join(insertCols, ", ")
	updateList := strings.Join(updateCols, ", ")
	stmts := []string{
		fmt.Sprintf(`REVOKE ALL ON %s FROM %s`, tbl, clients.RoleApp),
		fmt.Sprintf(`GRANT SELECT ON %s TO %s`, tbl, clients.RoleApp),
		fmt.Sprintf(`GRANT INSERT (%s) ON %s TO %s`, insertList, tbl, clients.RoleApp),
		fmt.Sprintf(`GRANT UPDATE (%s) ON %s TO %s`, updateList, tbl, clients.RoleApp),
		fmt.Sprintf(`GRANT DELETE ON %s TO %s`, tbl, clients.RoleApp),
		// tw_system（BYPASSRLS 信任根）表级全权：尾随读回/事件快照/_acl
		// choke point 与内部路径需要全列读写。
		fmt.Sprintf(`GRANT ALL ON %s TO %s`, tbl, clients.RoleSystem),
	}
	for _, stmt := range stmts {
		if _, err := p.conn(ctx).ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("column grants: %w", err)
		}
	}
	return nil
}

// tableColumns 返回表的全部列名（有序）。
func (p *postgresDocumentDB) tableColumns(ctx context.Context, schema, physical string) ([]string, error) {
	rows, err := p.conn(ctx).QueryContext(ctx,
		`SELECT column_name FROM information_schema.columns WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position`,
		schema, physical)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}
