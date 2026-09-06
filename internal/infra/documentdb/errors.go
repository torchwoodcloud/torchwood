package documentdb

import (
	"errors"
	"fmt"
	"strconv"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/pkg/idgen"
)

// pgErrorFielder 是 pgdriver.Error 的最小接口面（Field('C') = SQLSTATE）；
// 仅做类型匹配，不做 SQLSTATE 字符串回退，普通错误保持原样透传。
// 从 internal/app/shared/docdb_errors.go 下沉至 infra 层（J4-6），app 层不再感知 pgdriver。
type pgErrorFielder interface {
	Field(byte) string
}

// docDBErrorSQLStates 将常见 PG 客户端错误映射为 InvalidArgument，
// 资源类错误映射为 ResourceExhausted（A6）。与原 app 层定义保持一致。
var docDBErrorSQLStates = map[string]codes.Code{
	"22P02": codes.InvalidArgument,   // invalid_text_representation
	"22001": codes.InvalidArgument,   // string_data_right_truncation
	"23502": codes.InvalidArgument,   // not_null_violation
	"42703": codes.InvalidArgument,   // undefined_column
	"42601": codes.InvalidArgument,   // syntax_error
	"42804": codes.InvalidArgument,   // datatype_mismatch（值与列类型不符，如字符串进 integer 列）
	"23503": codes.InvalidArgument,   // foreign_key_violation
	"42883": codes.InvalidArgument,   // undefined_function
	"42P10": codes.InvalidArgument,   // invalid_column_reference (ON CONFLICT 无匹配唯一索引)
	"23505": codes.AlreadyExists,     // unique_violation（元数据重复键兜底映射）
	"42501": codes.PermissionDenied,  // insufficient_privilege / row-level security policy 违例（阶段③包 C：INSERT WITH CHECK 拒绝等）
	"53100": codes.ResourceExhausted, // disk_full
	"53200": codes.ResourceExhausted, // out_of_memory
	"54000": codes.ResourceExhausted, // program_limit_exceeded
	"53400": codes.ResourceExhausted, // configuration_limit_exceeded
}

// mapPGError 将 pgdriver 错误按 SQLSTATE 翻译为携带稳定域码的 gRPC status
// （redesign §4.1：infra 错误必须带 error_id，禁止裸 "document database error"）。
// 仅做类型匹配（errors.As 到 pgErrorFielder），不做字符串回退。
// 23505 特殊处理：返回领域哨兵 ErrDuplicateKey（与 isUniqueViolation 路径一致，
// 由 app 层 MapDocumentDBError 统一产出域码）；其余按 docDBErrorSQLStates 映射，
// ErrorInfo detail 携带 reason/retryable/sqlstate/error_id。
func mapPGError(err error) error {
	if err == nil {
		return nil
	}
	var fielder pgErrorFielder
	if errors.As(err, &fielder) {
		state := fielder.Field('C')
		if code, ok := docDBErrorSQLStates[state]; ok {
			switch code {
			case codes.AlreadyExists:
				// 23505：领域哨兵 ErrDuplicateKey（与 isUniqueViolation 路径一致，
				// 由 app 层 MapDocumentDBError 统一产出域码）。
				return databases.ErrDuplicateKey
			case codes.PermissionDenied:
				// 42501（阶段③包 C）：RLS policy 违例 / 权限不足 → 领域哨兵
				// ErrPermissionDenied。
				return databases.ErrPermissionDenied
			}
			domainCode := databases.ErrCodeInvalidArgument
			if code == codes.ResourceExhausted {
				domainCode = databases.ErrCodeExhausted
			}
			st := status.New(code, fmt.Sprintf("%s: postgres error (sqlstate %s)", domainCode, state))
			st, _ = st.WithDetails(&errdetails.ErrorInfo{
				Reason: domainCode,
				Domain: errorInfoDomain,
				Metadata: map[string]string{
					"sqlstate":  state,
					"retryable": strconv.FormatBool(databases.ErrorCodeRetryable(domainCode)),
					"error_id":  idgen.UUID().String(),
				},
			})
			return st.Err()
		}
	}
	return err
}

// errorInfoDomain 是 ErrorInfo 的 domain 字段（错误命名空间，非物理 schema）。
const errorInfoDomain = "torchwood.document"

// MapError 供测试或外部调用，显式触发 SQLSTATE 翻译（薄包装）。
func MapError(err error) error {
	return mapPGError(err)
}

// mapError 是 adapter 内部统一出口：先尝试 SQLSTATE 翻译，失败则原样返回。
// 供各 DocumentDB 方法在返回前调用，确保裸 pgdriver.Error 不上抛至 app 层。
func (p *postgresDocumentDB) mapError(err error) error {
	return mapPGError(err)
}
