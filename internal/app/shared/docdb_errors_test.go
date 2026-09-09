package shared

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
)

// sqlstateStub 是 pgdriver.Error 的本地替身（pgdriver.Error 无导出构造函数，
// 单测不得直接构造），实现最小接口 Field(byte) string（A6）。
type sqlstateStub struct{ state string }

func (s sqlstateStub) Error() string { return "sqlstate " + s.state }
func (s sqlstateStub) Field(b byte) string {
	if b == 'C' {
		return s.state
	}
	return ""
}

func TestMapDocumentDBError_DuplicateKey(t *testing.T) {
	// The infra layer re-exports the domain error as an alias; errors.Is must
	// still match so the mapping to AlreadyExists holds for both instances.
	err := errors.New("duplicate key")
	require.Error(t, err)

	plain := errors.New("SQLSTATE 23505 unique constraint")
	mapped := MapDocumentDBError(plain)
	require.Equal(t, plain, mapped)

	dup := databases.ErrDuplicateKey
	require.Equal(t, codes.AlreadyExists, status.Code(MapDocumentDBError(dup)))

	wrapped := errors.Join(dup, errors.New("context"))
	require.Equal(t, codes.AlreadyExists, status.Code(MapDocumentDBError(wrapped)))

	denied := databases.ErrPermissionDenied
	require.Equal(t, codes.PermissionDenied, status.Code(MapDocumentDBError(denied)))
	require.Nil(t, MapDocumentDBError(nil))
}

// TestMapDocumentDBError_SQLState (A6 → J4-6): SQLSTATE 翻译已下沉至
// internal/infra/documentdb.MapError，app 层不再感知 pgdriver。
// 此处断言 app 层的 MapDocumentDBError 对满足 Field 接口的 PG 错误一律透传，
// 映射由 infra 层负责；普通错误与未收录 SQLSTATE 同样透传。
func TestMapDocumentDBError_SQLState(t *testing.T) {
	cases := []string{"22P02", "22001", "23502", "42703", "42601", "23503", "42883", "53100", "53200", "54000", "53400", "99999"}
	for _, state := range cases {
		stub := sqlstateStub{state: state}
		require.Equal(t, stub, MapDocumentDBError(stub), "state %s should passthrough in app layer", state)
		wrapped := fmt.Errorf("pg error: %w", stub)
		// errors.Is/As 不应触发映射，返回原包装错误
		require.Equal(t, wrapped.Error(), MapDocumentDBError(wrapped).Error(), "state %s wrapped should passthrough", state)
	}

	// 普通错误（无 Field 方法）→ 原样透传（既有断言保持）。
	plain := errors.New("SQLSTATE 23505 unique constraint")
	require.Equal(t, plain, MapDocumentDBError(plain))
}

// TestMapDocumentDBError_OCCVersionErrors (PR1 → 域码体系): OCC 版本相关领域
// 错误映射为稳定域码（消息 "CODE: message" 格式，ErrorInfo.reason 同源）：
//
//	DOCUMENT.VERSION_REQUIRED / _CONFLICT / _COLUMN_CONFLICT → FailedPrecondition
//	DOCUMENT.VERSION_COLUMN_UNAVAILABLE → InvalidArgument
func TestMapDocumentDBError_OCCVersionErrors(t *testing.T) {
	cases := []struct {
		err      error
		code     codes.Code
		domain   string
		grpcCode codes.Code
	}{
		{databases.ErrVersionRequired, codes.FailedPrecondition, databases.ErrCodeVersionRequired, codes.FailedPrecondition},
		{databases.ErrVersionMismatch, codes.FailedPrecondition, databases.ErrCodeVersionConflict, codes.FailedPrecondition},
		{databases.ErrVersionColumnConflict, codes.FailedPrecondition, databases.ErrCodeVersionColumnConflict, codes.FailedPrecondition},
		{databases.ErrVersionColumnUnavailable, codes.InvalidArgument, databases.ErrCodeVersionColumnUnavailable, codes.InvalidArgument},
	}
	for _, tc := range cases {
		mapped := MapDocumentDBError(fmt.Errorf("update document: %w", tc.err))
		require.Equal(t, tc.code, status.Code(mapped), "err %v", tc.err)
		st := status.Convert(mapped)
		require.Equal(t, tc.domain+": "+tc.err.Error(), st.Message(), "err %v", tc.err)
		var info *errdetails.ErrorInfo
		for _, d := range st.Details() {
			if i, ok := d.(*errdetails.ErrorInfo); ok {
				info = i
			}
		}
		require.NotNil(t, info, "err %v", tc.err)
		require.Equal(t, tc.domain, info.Reason)
	}

	// UpdateDocumentVersionRequired 三态（Phase 1 裁决②）：缺省 →
	// FailedPrecondition/VERSION_REQUIRED；显式 0 → InvalidArgument/
	// VERSION_INVALID；正确值通过。
	require.Equal(t, codes.FailedPrecondition, status.Code(UpdateDocumentVersionRequired(nil)))
	require.Contains(t, status.Convert(UpdateDocumentVersionRequired(nil)).Message(), databases.ErrCodeVersionRequired)
	zero := int64(0)
	require.Equal(t, codes.InvalidArgument, status.Code(UpdateDocumentVersionRequired(&zero)))
	require.Contains(t, status.Convert(UpdateDocumentVersionRequired(&zero)).Message(), databases.ErrCodeVersionInvalid)
	neg := int64(-1)
	require.Equal(t, codes.InvalidArgument, status.Code(UpdateDocumentVersionRequired(&neg)))
	one := int64(1)
	require.NoError(t, UpdateDocumentVersionRequired(&one))
}

// TestMapDocumentDBError_OCCVersionConflictCurrentVersion（B10 §10.1）：
// *VersionConflictError 经包装链提取后，ErrorInfo metadata 携带
// current_version（Agent 直接取该值合并重试）；裸 ErrVersionMismatch 哨兵
// 路径行为不变（无 current_version 键）。
func TestMapDocumentDBError_OCCVersionConflictCurrentVersion(t *testing.T) {
	conflict := &databases.VersionConflictError{CurrentVersion: 7}
	require.True(t, errors.Is(conflict, databases.ErrVersionMismatch), "哨兵判定保持等价")
	mapped := MapDocumentDBError(fmt.Errorf("update document: %w", conflict))
	require.Equal(t, codes.FailedPrecondition, status.Code(mapped))

	var info *errdetails.ErrorInfo
	for _, d := range status.Convert(mapped).Details() {
		if i, ok := d.(*errdetails.ErrorInfo); ok {
			info = i
		}
	}
	require.NotNil(t, info)
	require.Equal(t, databases.ErrCodeVersionConflict, info.Reason)
	require.Equal(t, "7", info.Metadata["current_version"])
	require.Equal(t, "true", info.Metadata["retryable"])

	// 裸哨兵：无 current_version 键（旧行为不变）。
	plain := MapDocumentDBError(fmt.Errorf("update document: %w", databases.ErrVersionMismatch))
	for _, d := range status.Convert(plain).Details() {
		if i, ok := d.(*errdetails.ErrorInfo); ok {
			require.NotContains(t, i.Metadata, "current_version")
		}
	}
}

func TestMapDocumentDBError_Ident(t *testing.T) {
	mapped := MapDocumentDBError(ident.ErrInvalidSchemaResourceID)
	require.Equal(t, codes.InvalidArgument, status.Code(mapped))
	require.Equal(t, ident.ErrInvalidSchemaResourceID.Error(), status.Convert(mapped).Message())

	wrapped := fmt.Errorf("schema: %w", ident.ErrInvalidSchemaResourceID)
	require.Equal(t, codes.InvalidArgument, status.Code(MapDocumentDBError(wrapped)))
}

// TestMapDocumentDBError_StatusPassthrough：infra 已产出域码 status 的错误被
// fmt.Errorf 包装后，MapDocumentDBError 必须提取透传（不提取会因丢失
// GRPCStatus() 实现而退化为 Internal）。
func TestMapDocumentDBError_StatusPassthrough(t *testing.T) {
	infraErr := DomainStatus(databases.ErrCodeInvalidArgument)
	wrapped := fmt.Errorf("list documents: %w", infraErr)
	passthrough := MapDocumentDBError(wrapped)
	require.Equal(t, codes.InvalidArgument, status.Code(passthrough))
	require.Contains(t, status.Convert(passthrough).Message(), databases.ErrCodeInvalidArgument)
}

// TestDomainStatus_RetryableMetadata：retryable 静态表进 ErrorInfo metadata
// （Agent 自动重试决策依据）。
func TestDomainStatus_RetryableMetadata(t *testing.T) {
	conflict := DomainStatus(databases.ErrCodeVersionConflict)
	st := status.Convert(conflict)
	require.Equal(t, codes.FailedPrecondition, st.Code())
	found := false
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			found = true
			require.Equal(t, "true", info.Metadata["retryable"])
		}
	}
	require.True(t, found)

	denied := DomainStatus(databases.ErrCodePermissionDenied)
	for _, d := range status.Convert(denied).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			require.Equal(t, "false", info.Metadata["retryable"])
		}
	}
}

// TestDomainStatus_ErrorID：DomainStatus 生成处统一注入 error_id（与 infra
// SQLSTATE 路径对齐），双语并存——message（人读）+ reason/domain_code/
// retryable/error_id（机器读）。
func TestDomainStatus_ErrorID(t *testing.T) {
	for _, code := range []string{
		databases.ErrCodePermissionDenied,
		databases.ErrCodeVersionConflict,
		databases.ErrCodeTooLarge,
		databases.ErrCodeIdempotencyKeyConflict,
	} {
		st := status.Convert(DomainStatus(code))
		require.Equal(t, code, st.Message()[:len(code)], "消息以域码前缀")
		var info *errdetails.ErrorInfo
		for _, d := range st.Details() {
			if i, ok := d.(*errdetails.ErrorInfo); ok {
				info = i
			}
		}
		require.NotNil(t, info, code)
		require.Equal(t, code, info.Reason)
		require.NotEmpty(t, info.Metadata["error_id"], "%s 应携带 error_id", code)
		require.Contains(t, info.Metadata, "retryable")
	}

	// 每次生成独立实例（错误实例唯一标识）。
	a := status.Convert(DomainStatus(databases.ErrCodeNotFound))
	b := status.Convert(DomainStatus(databases.ErrCodeNotFound))
	var idA, idB string
	for _, d := range a.Details() {
		if i, ok := d.(*errdetails.ErrorInfo); ok {
			idA = i.Metadata["error_id"]
		}
	}
	for _, d := range b.Details() {
		if i, ok := d.(*errdetails.ErrorInfo); ok {
			idB = i.Metadata["error_id"]
		}
	}
	require.NotEqual(t, idA, idB)
}

// TestDomainStatusWithViolations：机器可读定位走 BadRequest 标准 detail
// （field_violations），消息附人类可读描述；ErrorInfo 不再携带 op_index。
func TestDomainStatusWithViolations(t *testing.T) {
	err := DomainStatusWithViolations(databases.ErrCodeTooLarge,
		FieldViolation{Field: "data.blob", Description: "attribute is 300000 bytes"})
	st := status.Convert(err)
	require.Equal(t, codes.InvalidArgument, st.Code())
	require.Contains(t, st.Message(), "DOCUMENT.TOO_LARGE")
	require.Contains(t, st.Message(), "data.blob")

	var br *errdetails.BadRequest
	var info *errdetails.ErrorInfo
	for _, d := range st.Details() {
		switch v := d.(type) {
		case *errdetails.BadRequest:
			br = v
		case *errdetails.ErrorInfo:
			info = v
		}
	}
	require.NotNil(t, br)
	require.Len(t, br.FieldViolations, 1)
	require.Equal(t, "data.blob", br.FieldViolations[0].Field)
	require.NotNil(t, info)
	require.Empty(t, info.Metadata["op_index"], "violations 定位不走 ErrorInfo metadata")
	require.NotEmpty(t, info.Metadata["error_id"])
}

// TestDomainStatusWithOp：execute-tx 失败 op 定位迁移到 BadRequest 字段路径
// 形态（ops[3].expected_version），消息保留 "CODE: op[N]: msg" 人类定位。
func TestDomainStatusWithOp(t *testing.T) {
	err := DomainStatusWithOp(databases.ErrCodeVersionConflict, 3)
	st := status.Convert(err)
	require.Equal(t, codes.FailedPrecondition, st.Code())
	require.Contains(t, st.Message(), "op[3]")

	var br *errdetails.BadRequest
	for _, d := range st.Details() {
		if v, ok := d.(*errdetails.BadRequest); ok {
			br = v
		}
	}
	require.NotNil(t, br)
	require.Len(t, br.FieldViolations, 1)
	require.Equal(t, "ops[3].expected_version", br.FieldViolations[0].Field)

	// 无子字段映射的域码退化为 "ops[N]" 整体定位。
	err = DomainStatusWithOp(databases.ErrCodeExhausted, 1)
	for _, d := range status.Convert(err).Details() {
		if v, ok := d.(*errdetails.BadRequest); ok {
			require.Equal(t, "ops[1]", v.FieldViolations[0].Field)
		}
	}
}
