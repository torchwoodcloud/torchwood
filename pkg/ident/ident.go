package ident

import (
	"errors"
	"regexp"
	"strings"
)

const (
	// MaxSchemaResourceIDLen 是 project.id / database.id 的最大长度。
	MaxSchemaResourceIDLen = 28
	// SchemaPrefix 是动态文档 PostgreSQL schema 的固定前缀。
	SchemaPrefix = "tw_"
	// ProjectDataPlaneID 是项目数据面（tw_<project>）的内部 database 标识。
	// 它不是合法的 SchemaResourceID（ValidateSchemaResourceID 拒绝 "_"），
	// 仅用于内部寻址：documentSchema 在 databaseID==ProjectDataPlaneID 时
	// 映射到 ProjectSchemaName。对外 database_id 走 RejectExternalDatabaseID
	// 拒绝；Create/DeleteDatabase 的 businessSchema 显式拒绝 sentinel。
	ProjectDataPlaneID = "_"
)

const errSchemaResourceID = "id must match ^[a-z][a-z0-9]{0,27}$"

// ErrInvalidSchemaResourceID 表示 project.id / database.id 未通过 charset 校验。
var ErrInvalidSchemaResourceID = errors.New(errSchemaResourceID)

// 小写字母开头，后接 0–27 个小写字母或数字。合计最长 28。
var schemaResourceIDRe = regexp.MustCompile(`^[a-z][a-z0-9]{0,27}$`)

var schemaNameRe = regexp.MustCompile(`^tw_[a-z][a-z0-9]{0,27}_[a-z][a-z0-9]{0,27}$`)

// projectSchemaNameRe 匹配一段式项目数据面 schema tw_{project.id}。
// 与 schemaNameRe（两段式）不相交：project.id 不含 "_"，故 tw_<p> 恰好一道
// 下划线（前缀后），tw_<p>_<db> 恰好两道。见 §2.1。
var projectSchemaNameRe = regexp.MustCompile(`^tw_[a-z][a-z0-9]{0,27}$`)

// ProjectSchemaName 拼出项目数据面 schema tw_{project.id}（一段式）。
// 系统静态表（users/sessions/... bun）落在该 schema，而非 tw_<p>_default。
// sentinel 仍用于 documentSchema 映射与对外拒绝。
// projectID 非法时返回 error，不拼接。
func ProjectSchemaName(projectID string) (string, error) {
	if err := ValidateSchemaResourceID(projectID); err != nil {
		return "", err
	}
	name := SchemaPrefix + projectID
	if !projectSchemaNameRe.MatchString(name) {
		// 理论不可达：projectID 已过 ValidateSchemaResourceID（小写字母+数字、
		// 无下划线）。保留断言作纵深防御。
		return "", ErrInvalidSchemaResourceID
	}
	return name, nil
}

// IsTwoSegmentSchema 报告 name 是否为两段式业务文档面 schema（tw_<project>_<database>）。
// 供 DeleteDatabase/businessSchema 硬断言：DDL 目标必须匹配两段式，绝不能是
// 一段式 ProjectSchemaName。与 ProjectSchemaName 的返回值不相交。
func IsTwoSegmentSchema(name string) bool {
	return schemaNameRe.MatchString(name)
}

// ValidateSchemaResourceID 校验 project.id / database.id。
func ValidateSchemaResourceID(id string) error {
	if id == "" || !schemaResourceIDRe.MatchString(id) {
		return ErrInvalidSchemaResourceID
	}
	return nil
}

// SchemaName 拼出 tw_{project.id}_{database.id}。两端非法时返回 error，不拼接。
func SchemaName(projectID, databaseID string) (string, error) {
	if err := ValidateSchemaResourceID(projectID); err != nil {
		return "", err
	}
	if err := ValidateSchemaResourceID(databaseID); err != nil {
		return "", err
	}
	name := SchemaPrefix + projectID + "_" + databaseID
	if !schemaNameRe.MatchString(name) {
		return "", ErrInvalidSchemaResourceID
	}
	return name, nil
}

// ParseSchemaName 反解两段式 schema tw_{project.id}_{database.id}。两端 ID
// 字符集（^[a-z][a-z0-9]{0,27}$）不含下划线，前缀后第一道 "_" 即分割点，
// 反解无歧义；一段式项目数据面 schema 返回 error。
func ParseSchemaName(name string) (projectID, databaseID string, err error) {
	rest := strings.TrimPrefix(name, SchemaPrefix)
	i := strings.IndexByte(rest, '_')
	if i <= 0 || i == len(rest)-1 {
		return "", "", ErrInvalidSchemaResourceID
	}
	projectID, databaseID = rest[:i], rest[i+1:]
	if ValidateSchemaResourceID(projectID) != nil || ValidateSchemaResourceID(databaseID) != nil {
		return "", "", ErrInvalidSchemaResourceID
	}
	return projectID, databaseID, nil
}
