package shared

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
)

// 域错误码体系（redesign §4.1）：域码稳定 snake_case 静态映射 gRPC code，
// 消息格式 "CODE: message"，ErrorInfo detail 携带 reason/retryable——Agent
// 可机器判定恢复路径。POC 直接替换旧文案，无兼容映射。

// domainCodeGRPC 是域码 → gRPC code 的静态映射（唯一真相源）。
var domainCodeGRPC = map[string]codes.Code{
	databases.ErrCodePermissionDenied:         codes.PermissionDenied,
	databases.ErrCodeAlreadyExists:            codes.AlreadyExists,
	databases.ErrCodeNotFound:                 codes.NotFound,
	databases.ErrCodeNoFieldsToUpdate:         codes.InvalidArgument,
	databases.ErrCodeVersionRequired:          codes.FailedPrecondition,
	databases.ErrCodeVersionInvalid:           codes.InvalidArgument,
	databases.ErrCodeVersionConflict:          codes.FailedPrecondition,
	databases.ErrCodeVersionColumnConflict:    codes.FailedPrecondition,
	databases.ErrCodeVersionColumnUnavailable: codes.InvalidArgument,
	databases.ErrCodeInvalidArgument:          codes.InvalidArgument,
	databases.ErrCodeTooLarge:                 codes.InvalidArgument,
	databases.ErrCodeACLTooLarge:              codes.InvalidArgument,
	databases.ErrCodeAttributeUnserializable:  codes.InvalidArgument,
	databases.ErrCodeExhausted:                codes.ResourceExhausted,
	databases.ErrCodeIdempotencyKeyConflict:   codes.InvalidArgument,
	databases.ErrCodeIdempotencyInProgress:    codes.Aborted,
	databases.ErrCodeAggregateOverflow:        codes.InvalidArgument,
	databases.ErrCodeDDLConflict:              codes.Aborted,
	databases.ErrCodeColumnLimitExceeded:      codes.InvalidArgument,
	databases.ErrCodeResumeExpired:            codes.FailedPrecondition,
}

// domainCodeMessage 是域码的人类可读消息（与领域哨兵文案同源）。
var domainCodeMessage = map[string]string{
	databases.ErrCodePermissionDenied:         databases.ErrPermissionDenied.Error(),
	databases.ErrCodeAlreadyExists:            databases.ErrDuplicateKey.Error(),
	databases.ErrCodeNotFound:                 databases.ErrDocumentNotFound.Error(),
	databases.ErrCodeNoFieldsToUpdate:         databases.ErrNoFieldsToUpdate.Error(),
	databases.ErrCodeVersionRequired:          databases.ErrVersionRequired.Error(),
	databases.ErrCodeVersionInvalid:           databases.ErrVersionInvalid.Error(),
	databases.ErrCodeVersionConflict:          databases.ErrVersionMismatch.Error(),
	databases.ErrCodeVersionColumnConflict:    databases.ErrVersionColumnConflict.Error(),
	databases.ErrCodeVersionColumnUnavailable: databases.ErrVersionColumnUnavailable.Error(),
	databases.ErrCodeInvalidArgument:          "invalid argument",
	databases.ErrCodeTooLarge:                 "document payload too large",
	databases.ErrCodeACLTooLarge:              "document acl has too many access control entries",
	databases.ErrCodeAttributeUnserializable:  "attribute is not serializable",
	databases.ErrCodeExhausted:                "resource exhausted",
	databases.ErrCodeIdempotencyKeyConflict:   databases.ErrIdempotencyKeyConflict.Error(),
	databases.ErrCodeIdempotencyInProgress:    "request with the same idempotency key is still in progress",
	databases.ErrCodeAggregateOverflow:        databases.ErrAggregateOverflow.Error(),
	databases.ErrCodeDDLConflict:              "concurrent schema modification conflict; re-read the collection and retry",
	databases.ErrCodeColumnLimitExceeded:      "collection attribute count exceeds the soft limit",
	databases.ErrCodeResumeExpired:            "resume cursor predates the oldest available event; re-sync with a full listing and resume from the latest seq",
}

const errorInfoDomain = "torchwood.document"

// withErrorInfo 为 status 附 ErrorInfo detail（reason / retryable / error_id）。
// error_id 由 idgen 现场生成，与 infra SQLSTATE 路径（documentdb/errors.go）
// 对齐——每个错误实例都有可上报的唯一标识（redesign §4.1）。
func withErrorInfo(st *status.Status, domainCode string, extra map[string]string) *status.Status {
	md := map[string]string{
		"retryable": strconv.FormatBool(databases.ErrorCodeRetryable(domainCode)),
		"error_id":  idgen.UUID().String(),
	}
	for k, v := range extra {
		md[k] = v
	}
	st, _ = st.WithDetails(&errdetails.ErrorInfo{
		Reason:   domainCode,
		Domain:   errorInfoDomain,
		Metadata: md,
	})
	return st
}

// DomainStatus 构造携带稳定域码的 gRPC status：消息 "CODE: message"，
// ErrorInfo detail（reason / retryable / error_id）。
func DomainStatus(domainCode string) error {
	st := status.New(domainCodeGRPC[domainCode], domainCode+": "+domainCodeMessage[domainCode])
	return withErrorInfo(st, domainCode, nil).Err()
}

// DomainStatusWithMetadata 是 DomainStatus 的 metadata 扩展通道：extra 键值
// 并入 ErrorInfo metadata（redesign §10.1：OCC 冲突错误体带 current_version
// ——Agent 直接取该值合并重试）。
func DomainStatusWithMetadata(domainCode string, extra map[string]string) error {
	st := status.New(domainCodeGRPC[domainCode], domainCode+": "+domainCodeMessage[domainCode])
	return withErrorInfo(st, domainCode, extra).Err()
}

// FieldViolation 描述一个违规字段（google.rpc.BadRequest.FieldViolation 的
// 薄包装；field 为请求字段路径，如 "ops[3].expected_version"、"data.blob"）。
type FieldViolation struct {
	Field       string
	Description string
}

// DomainStatusWithViolations 构造带 violations 的域码错误：机器可读定位走
// google.rpc.BadRequest 标准 detail（field_violations），替代散落的 metadata；
// 人类可读消息附各 violation 的 "field: description"。
func DomainStatusWithViolations(domainCode string, violations ...FieldViolation) error {
	msg := domainCode + ": " + domainCodeMessage[domainCode]
	var fvs []*errdetails.BadRequest_FieldViolation
	parts := make([]string, 0, len(violations))
	for _, v := range violations {
		fvs = append(fvs, &errdetails.BadRequest_FieldViolation{
			Field:       v.Field,
			Description: v.Description,
		})
		parts = append(parts, v.Field+": "+v.Description)
	}
	if len(parts) > 0 {
		msg += "; " + strings.Join(parts, "; ")
	}
	st := status.New(domainCodeGRPC[domainCode], msg)
	st = withErrorInfo(st, domainCode, nil)
	if len(fvs) > 0 {
		st, _ = st.WithDetails(&errdetails.BadRequest{FieldViolations: fvs})
	}
	return st.Err()
}

// opViolationField 把失败 op 的域码映射到 violations 字段路径
// （redesign §4.1：op 定位的机器可读形态，如 "ops[3].expected_version"）。
func opViolationField(domainCode string, opIndex int) string {
	sub := ""
	switch domainCode {
	case databases.ErrCodeVersionRequired, databases.ErrCodeVersionInvalid,
		databases.ErrCodeVersionConflict:
		sub = "expected_version"
	case databases.ErrCodePermissionDenied:
		sub = "permissions"
	case databases.ErrCodeNotFound, databases.ErrCodeAlreadyExists:
		sub = "document_id"
	case databases.ErrCodeNoFieldsToUpdate:
		sub = "data"
	}
	if sub == "" {
		return fmt.Sprintf("ops[%d]", opIndex)
	}
	return fmt.Sprintf("ops[%d].%s", opIndex, sub)
}

// DomainStatusWithOp 是 DomainStatus 的批事务变体：消息保留 "CODE: op[N]: msg"
// 人类定位；机器可读 op 定位迁移到 BadRequest detail 的字段路径形态
// （opViolationField，如 ops[3].expected_version），不再走 ErrorInfo metadata。
func DomainStatusWithOp(domainCode string, opIndex int) error {
	desc := fmt.Sprintf("op[%d]: %s", opIndex, domainCodeMessage[domainCode])
	return DomainStatusWithViolations(domainCode, FieldViolation{
		Field:       opViolationField(domainCode, opIndex),
		Description: desc,
	})
}

// UpdateDocumentVersionRequired 校验用户集合 Update/Delete 的 OCC 版本参数
// （Phase 1 裁决②三态拆分）：
//   - 缺省（nil）→ FailedPrecondition / DOCUMENT.VERSION_REQUIRED；
//   - 显式 ≤0（非法值）→ InvalidArgument / DOCUMENT.VERSION_INVALID；
//   - 正确值 → 通过。
func UpdateDocumentVersionRequired(version *int64) error {
	if version == nil {
		return DomainStatus(databases.ErrCodeVersionRequired)
	}
	if *version <= 0 {
		return DomainStatus(databases.ErrCodeVersionInvalid)
	}
	return nil
}

// MapDocumentDBError 将文档库错误映射为携带稳定域码的 gRPC status：
// ① 领域哨兵（可穿透 fmt.Errorf 包装链）→ DomainStatus；
// ② infra 已产出的域码 status（SQLSTATE 路径）从包装链提取后透传保真——
//
//	不提取会因丢失 GRPCStatus() 实现而退化为 Internal；
//
// ③ 其余原样返回。
func MapDocumentDBError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ident.ErrInvalidSchemaResourceID) {
		return MapIdentError(err)
	}
	if code := databases.ErrorDomainCode(err); code != "" {
		// OCC 冲突（redesign §10.1）：infra 以 *VersionConflictError 携带探测
		// 读到的当前 _version，此处提取后塞进 ErrorInfo metadata 的
		// current_version（Agent 免额外读回即可合并重试）。
		var vc *databases.VersionConflictError
		if errors.As(err, &vc) {
			return DomainStatusWithMetadata(code, map[string]string{
				"current_version": strconv.FormatInt(vc.CurrentVersion, 10),
			})
		}
		return DomainStatus(code)
	}
	var gs interface{ GRPCStatus() *status.Status }
	if errors.As(err, &gs) {
		return gs.GRPCStatus().Err()
	}
	return err
}
