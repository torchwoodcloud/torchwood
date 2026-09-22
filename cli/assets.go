package cli

import (
	"flag"
	"fmt"

	"github.com/lynx-go/commands"
)

const (
	methodAssetsDefCreate  = "/torchwood.server.v1.AssetsService/CreateAssetDef"
	methodAssetsDefList    = "/torchwood.server.v1.AssetsService/ListAssetDefs"
	methodAssetsDefGet     = "/torchwood.server.v1.AssetsService/GetAssetDef"
	methodAssetsDefUpdate  = "/torchwood.server.v1.AssetsService/UpdateAssetDef"
	methodAssetsDefDelete  = "/torchwood.server.v1.AssetsService/DeleteAssetDef"
	methodAssetsGrant      = "/torchwood.server.v1.AssetsService/Grant"
	methodAssetsConsume    = "/torchwood.server.v1.AssetsService/Consume"
	methodAssetsTransfer   = "/torchwood.server.v1.AssetsService/Transfer"
	methodAssetsMutate     = "/torchwood.server.v1.AssetsService/Mutate"
	methodAssetsExpire     = "/torchwood.server.v1.AssetsService/Expire"
	methodAssetsReconcile  = "/torchwood.server.v1.AssetsService/Reconcile"
	methodAssetsHoldings   = "/torchwood.server.v1.AssetsService/ListUserAssets"
	methodAssetsUserLedger = "/torchwood.server.v1.AssetsService/ListUserLedger"
)

// newAssetsCmd 覆盖 AssetsService 全部 13 个方法：AssetDef CRUD（defs ...）
// + 账本五动词（grant/consume/transfer/mutate/expire）+ 对账（reconcile）+
// 用户持有/流水只读（holdings/ledger）。读 assets.read；写 assets.write
// （五动词与 reconcile 服务端强制 admin 角色）。
func newAssetsCmd(g *GlobalFlags) *group {
	return newGroup(g, "assets", "asset ledger: def CRUD, grant/consume/transfer/mutate/expire, reconcile, user holdings/ledger (reads: assets.read; writes: assets.write)", func(sub *commands.App) {
		sub.Register(
			newAssetsDefsCmd(g),
			newAssetsGrantCmd(g),
			newAssetsConsumeCmd(g),
			newAssetsTransferCmd(g),
			newAssetsMutateCmd(g),
			newAssetsExpireCmd(g),
			newAssetsReconcileCmd(g),
			newAssetsHoldingsCmd(g),
			newAssetsLedgerCmd(g),
		)
	})
}

// newAssetsDefsCmd: AssetDef 配置 CRUD。class 四类：currency（可分割，
// decimals 表精度）| stack（可叠数量）| instance（唯一实例）| entitlement。
func newAssetsDefsCmd(g *GlobalFlags) *group {
	return newGroup(g, "defs", "asset definition CRUD", func(sub *commands.App) {
		sub.Register(
			newAssetsDefCreateCmd(g),
			newAssetsDefListCmd(g),
			newAssetsDefGetCmd(g),
			newAssetsDefUpdateCmd(g),
			newAssetsDefDeleteCmd(g),
		)
	})
}

func newAssetsDefCreateCmd(g *GlobalFlags) *verb {
	var code, name, class, metadata string
	var decimals int
	var maxQuantity, expiresIn int64
	var tradable, uniquePerOwner, upgradeable bool
	return newVerb(g, "create", "create an asset definition (assets.write)", "assets defs create --code <c> --name <n> --class currency|stack|instance|entitlement [...]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&code, "code", "", "asset code, unique per project (required)")
			fs.StringVar(&name, "name", "", "display name (required)")
			fs.StringVar(&class, "class", "", "currency | stack | instance | entitlement (required)")
			fs.IntVar(&decimals, "decimals", 0, "decimal places (currency class; 0 for integer currencies)")
			fs.Int64Var(&maxQuantity, "max-quantity", 0, "max quantity per owner (omit for unbounded)")
			fs.Int64Var(&expiresIn, "expires-in", 0, "validity in seconds from grant (omit for non-expiring)")
			fs.BoolVar(&tradable, "tradable", false, "allow transfer between owners")
			fs.BoolVar(&uniquePerOwner, "unique-per-owner", false, "one holding per owner")
			fs.BoolVar(&upgradeable, "upgradeable", false, "support level upgrades (mutate --level)")
			fs.StringVar(&metadata, "metadata", "", "JSON object with arbitrary metadata")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			req, err := buildCreateDefReq(code, name, class, decimals, maxQuantity, expiresIn, tradable, uniquePerOwner, upgradeable, metadata)
			if err != nil {
				return err
			}
			return call(g, env, methodAssetsDefCreate, req)
		})
}

// buildCreateDefReq 构造 CreateAssetDefRequest：必填 code/name/class；可选
// 数值走非零规则（0 = 服务端缺省）。
func buildCreateDefReq(code, name, class string, decimals int, maxQuantity, expiresIn int64, tradable, uniquePerOwner, upgradeable bool, metadata string) (map[string]any, error) {
	if code == "" || name == "" || class == "" {
		return nil, fmt.Errorf("--code, --name and --class are required")
	}
	req := map[string]any{"code": code, "name": name, "class": class}
	if decimals != 0 {
		req["decimals"] = decimals
	}
	if maxQuantity != 0 {
		req["maxQuantity"] = maxQuantity
	}
	if expiresIn != 0 {
		req["expiresIn"] = expiresIn
	}
	if tradable {
		req["tradable"] = true
	}
	if uniquePerOwner {
		req["uniquePerOwner"] = true
	}
	if upgradeable {
		req["upgradeable"] = true
	}
	if metadata != "" {
		m, err := jsonObject(metadata, "--metadata")
		if err != nil {
			return nil, err
		}
		req["metadata"] = m
	}
	return req, nil
}

func newAssetsDefListCmd(g *GlobalFlags) *verb {
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "list asset definitions", "assets defs list [--page-size <n>] [--page-token <t>]",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default when omitted)")
			fs.StringVar(&pageToken, "page-token", "", "next page token from the previous response")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			return call(g, env, methodAssetsDefList, listJSON(pageSize, pageToken))
		})
}

func newAssetsDefGetCmd(g *GlobalFlags) *verb {
	return newVerb(g, "get", "get an asset definition by ID", "assets defs get <def-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodAssetsDefGet, map[string]any{"defId": args[0]})
		})
}

func newAssetsDefUpdateCmd(g *GlobalFlags) *verb {
	var name, status, metadata string
	var decimals int
	var maxQuantity, expiresIn int64
	var tradable, uniquePerOwner, upgradeable bool
	return newVerb(g, "update", "update an asset definition (assets.write; only explicitly passed fields — pass bools as --tradable=true/false explicitly)", "assets defs update [--name] [--decimals] [--max-quantity] [--expires-in] [--tradable] [--unique-per-owner] [--upgradeable] [--metadata '<json>'] [--status active|archived] <def-id>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&name, "name", "", "display name")
			fs.IntVar(&decimals, "decimals", 0, "decimal places (currency class)")
			fs.Int64Var(&maxQuantity, "max-quantity", 0, "max quantity per owner")
			fs.Int64Var(&expiresIn, "expires-in", 0, "validity in seconds from grant")
			fs.BoolVar(&tradable, "tradable", false, "allow transfer between owners")
			fs.BoolVar(&uniquePerOwner, "unique-per-owner", false, "one holding per owner")
			fs.BoolVar(&upgradeable, "upgradeable", false, "support level upgrades")
			fs.StringVar(&metadata, "metadata", "", "JSON object (only applied when passed)")
			fs.StringVar(&status, "status", "", "status: active | archived")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildUpdateDefReq(v, args[0], defPatch{
				name: name, decimals: decimals, maxQuantity: maxQuantity, expiresIn: expiresIn,
				tradable: tradable, uniquePerOwner: uniquePerOwner, upgradeable: upgradeable,
				metadata: metadata, status: status,
			})
			if err != nil {
				return err
			}
			return call(g, env, methodAssetsDefUpdate, req)
		})
}

// defPatch 聚合 UpdateAssetDefRequest 的可修改字段。
type defPatch struct {
	name, status, metadata   string
	decimals                 int
	maxQuantity, expiresIn   int64
	tradable, uniquePerOwner bool
	upgradeable              bool
}

// buildUpdateDefReq 构造 UpdateAssetDefRequest（proto3 optional：未显式传入 =
// 不修改）。metadata 是普通 Struct 字段，仅显式传入才携带，避免误清。
func buildUpdateDefReq(v *verb, defID string, p defPatch) (map[string]any, error) {
	if defID == "" {
		return nil, fmt.Errorf("missing def-id positional argument")
	}
	req := map[string]any{"defId": defID}
	setChanged(v, "name", req, "name", p.name)
	setChanged(v, "decimals", req, "decimals", p.decimals)
	setChanged(v, "max-quantity", req, "maxQuantity", p.maxQuantity)
	setChanged(v, "expires-in", req, "expiresIn", p.expiresIn)
	setChanged(v, "tradable", req, "tradable", p.tradable)
	setChanged(v, "unique-per-owner", req, "uniquePerOwner", p.uniquePerOwner)
	setChanged(v, "upgradeable", req, "upgradeable", p.upgradeable)
	setChanged(v, "status", req, "status", p.status)
	if v.changed("metadata") {
		m, err := jsonObject(p.metadata, "--metadata")
		if err != nil {
			return nil, err
		}
		req["metadata"] = m
	}
	return req, nil
}

func newAssetsDefDeleteCmd(g *GlobalFlags) *verb {
	return newVerb(g, "delete", "delete an asset definition (assets.write)", "assets defs delete <def-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodAssetsDefDelete, map[string]any{"defId": args[0]})
		})
}

// idempotencyFlag 声明账本动词共用的幂等旗标：项目级全局键空间，动词间不
// 区分——复用已有键返回首次操作的重放结果。
func idempotencyFlag(fs *flag.FlagSet, p *string) {
	fs.StringVar(p, "idempotency-key", "", "project-global idempotency key (keys are shared across verbs; replaying a key returns the first operation's result)")
}

// refFlags 声明审计引用旗标（对账时回溯业务来源）。
func refFlags(fs *flag.FlagSet, refType, refID *string) {
	fs.StringVar(refType, "ref-type", "", "reference type, e.g. order | quest | admin")
	fs.StringVar(refID, "ref-id", "", "reference ID (pairs with --ref-type)")
}

func newAssetsGrantCmd(g *GlobalFlags) *verb {
	var quantity int64
	var idempotencyKey, expiresAt, metadata, refType, refID string
	var level int
	return newVerb(g, "grant", "grant assets to an owner (assets.write, admin; audit-logged)", "assets grant --quantity <n> [--idempotency-key <k>] [--expires-at <ts>] [--level <n>] [--metadata '<json>'] [--ref-type <t>] [--ref-id <id>] <owner-id> <def-code>",
		func(fs *flag.FlagSet) {
			fs.Int64Var(&quantity, "quantity", 0, "amount to grant (required; must be positive)")
			idempotencyFlag(fs, &idempotencyKey)
			fs.StringVar(&expiresAt, "expires-at", "", "absolute expiry (RFC3339, or YYYY-MM-DD = UTC midnight)")
			fs.IntVar(&level, "level", 0, "initial level (upgradeable defs)")
			fs.StringVar(&metadata, "metadata", "", "JSON object attached to the holding")
			refFlags(fs, &refType, &refID)
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			req, err := buildGrantReq(args[0], args[1], quantity, idempotencyKey, expiresAt, level, metadata, refType, refID)
			if err != nil {
				return err
			}
			return call(g, env, methodAssetsGrant, req)
		})
}

// buildGrantReq 构造 GrantRequest：quantity 必填且恒正（0/负由服务端再拒）。
func buildGrantReq(ownerID, defCode string, quantity int64, idempotencyKey, expiresAt string, level int, metadata, refType, refID string) (map[string]any, error) {
	if ownerID == "" || defCode == "" {
		return nil, fmt.Errorf("missing owner-id/def-code positional arguments")
	}
	if quantity <= 0 {
		return nil, fmt.Errorf("--quantity is required (positive)")
	}
	req := map[string]any{"ownerId": ownerID, "defCode": defCode, "quantity": quantity}
	if idempotencyKey != "" {
		req["idempotencyKey"] = idempotencyKey
	}
	if expiresAt != "" {
		req["expiresAt"] = tsJSON(expiresAt)
	}
	if level != 0 {
		req["level"] = level
	}
	if metadata != "" {
		m, err := jsonObject(metadata, "--metadata")
		if err != nil {
			return nil, err
		}
		req["metadata"] = m
	}
	if refType != "" {
		req["refType"] = refType
	}
	if refID != "" {
		req["refId"] = refID
	}
	return req, nil
}

func newAssetsConsumeCmd(g *GlobalFlags) *verb {
	var quantity int64
	var idempotencyKey, refType, refID string
	return newVerb(g, "consume", "consume (deduct) assets from an owner (assets.write, admin; audit-logged)", "assets consume --quantity <n> [--idempotency-key <k>] [--ref-type <t>] [--ref-id <id>] <owner-id> <def-code>",
		func(fs *flag.FlagSet) {
			fs.Int64Var(&quantity, "quantity", 0, "amount to consume (required; must be positive)")
			idempotencyFlag(fs, &idempotencyKey)
			refFlags(fs, &refType, &refID)
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			req, err := buildConsumeReq(args[0], args[1], quantity, idempotencyKey, refType, refID)
			if err != nil {
				return err
			}
			return call(g, env, methodAssetsConsume, req)
		})
}

// buildConsumeReq 构造 ConsumeRequest。
func buildConsumeReq(ownerID, defCode string, quantity int64, idempotencyKey, refType, refID string) (map[string]any, error) {
	if ownerID == "" || defCode == "" {
		return nil, fmt.Errorf("missing owner-id/def-code positional arguments")
	}
	if quantity <= 0 {
		return nil, fmt.Errorf("--quantity is required (positive)")
	}
	req := map[string]any{"ownerId": ownerID, "defCode": defCode, "quantity": quantity}
	if idempotencyKey != "" {
		req["idempotencyKey"] = idempotencyKey
	}
	if refType != "" {
		req["refType"] = refType
	}
	if refID != "" {
		req["refId"] = refID
	}
	return req, nil
}

func newAssetsTransferCmd(g *GlobalFlags) *verb {
	var quantity int64
	var idempotencyKey, refType, refID string
	return newVerb(g, "transfer", "transfer assets between owners (assets.write, admin; def must be tradable)", "assets transfer --quantity <n> [--idempotency-key <k>] [--ref-type <t>] [--ref-id <id>] <from-owner-id> <to-owner-id> <def-code>",
		func(fs *flag.FlagSet) {
			fs.Int64Var(&quantity, "quantity", 0, "amount to transfer (required; must be positive)")
			idempotencyFlag(fs, &idempotencyKey)
			refFlags(fs, &refType, &refID)
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 3); err != nil {
				return err
			}
			req, err := buildTransferReq(args[0], args[1], args[2], quantity, idempotencyKey, refType, refID)
			if err != nil {
				return err
			}
			return call(g, env, methodAssetsTransfer, req)
		})
}

// buildTransferReq 构造 TransferRequest。
func buildTransferReq(fromOwnerID, toOwnerID, defCode string, quantity int64, idempotencyKey, refType, refID string) (map[string]any, error) {
	if fromOwnerID == "" || toOwnerID == "" || defCode == "" {
		return nil, fmt.Errorf("missing from-owner-id/to-owner-id/def-code positional arguments")
	}
	if quantity <= 0 {
		return nil, fmt.Errorf("--quantity is required (positive)")
	}
	req := map[string]any{"fromOwnerId": fromOwnerID, "toOwnerId": toOwnerID, "defCode": defCode, "quantity": quantity}
	if idempotencyKey != "" {
		req["idempotencyKey"] = idempotencyKey
	}
	if refType != "" {
		req["refType"] = refType
	}
	if refID != "" {
		req["refId"] = refID
	}
	return req, nil
}

func newAssetsMutateCmd(g *GlobalFlags) *verb {
	var idempotencyKey, expiresAt, metadata, refType, refID string
	var level int
	return newVerb(g, "mutate", "mutate a holding in place (level / expiry / metadata; assets.write, admin)", "assets mutate [--idempotency-key <k>] [--level <n>] [--expires-at <ts>] [--metadata '<json>'] [--ref-type <t>] [--ref-id <id>] <holding-id>",
		func(fs *flag.FlagSet) {
			idempotencyFlag(fs, &idempotencyKey)
			fs.IntVar(&level, "level", 0, "new level (upgradeable defs)")
			fs.StringVar(&expiresAt, "expires-at", "", "new expiry (RFC3339, or YYYY-MM-DD = UTC midnight)")
			fs.StringVar(&metadata, "metadata", "", "JSON object (replaces the holding metadata)")
			refFlags(fs, &refType, &refID)
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildMutateReq(v, args[0], idempotencyKey, level, expiresAt, metadata, refType, refID)
			if err != nil {
				return err
			}
			return call(g, env, methodAssetsMutate, req)
		})
}

// buildMutateReq 构造 MutateRequest：至少要求一个修改目标（level / expiry /
// metadata），否则空操作没有意义。
func buildMutateReq(v *verb, holdingID, idempotencyKey string, level int, expiresAt, metadata, refType, refID string) (map[string]any, error) {
	if holdingID == "" {
		return nil, fmt.Errorf("missing holding-id positional argument")
	}
	if !v.changed("level") && !v.changed("expires-at") && !v.changed("metadata") {
		return nil, fmt.Errorf("nothing to mutate: pass at least one of --level, --expires-at, --metadata")
	}
	req := map[string]any{"holdingId": holdingID}
	if idempotencyKey != "" {
		req["idempotencyKey"] = idempotencyKey
	}
	setChanged(v, "level", req, "level", level)
	if v.changed("expires-at") {
		req["expiresAt"] = tsJSON(expiresAt)
	}
	if v.changed("metadata") {
		m, err := jsonObject(metadata, "--metadata")
		if err != nil {
			return nil, err
		}
		req["metadata"] = m
	}
	if refType != "" {
		req["refType"] = refType
	}
	if refID != "" {
		req["refId"] = refID
	}
	return req, nil
}

func newAssetsExpireCmd(g *GlobalFlags) *verb {
	var idempotencyKey string
	return newVerb(g, "expire", "expire a holding immediately (assets.write, admin)", "assets expire [--idempotency-key <k>] <holding-id>",
		func(fs *flag.FlagSet) {
			idempotencyFlag(fs, &idempotencyKey)
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			if args[0] == "" {
				return fmt.Errorf("missing holding-id positional argument")
			}
			req := map[string]any{"holdingId": args[0]}
			if idempotencyKey != "" {
				req["idempotencyKey"] = idempotencyKey
			}
			return call(g, env, methodAssetsExpire, req)
		})
}

func newAssetsReconcileCmd(g *GlobalFlags) *verb {
	return newVerb(g, "reconcile", "trigger a reconciliation run (ledger replay = holdings snapshot; reports drift)", "assets reconcile", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			return call(g, env, methodAssetsReconcile, map[string]any{})
		})
}

func newAssetsHoldingsCmd(g *GlobalFlags) *verb {
	var pageSize int
	var pageToken string
	return newVerb(g, "holdings", "list an owner's asset holdings", "assets holdings [--page-size <n>] [--page-token <t>] <owner-id>",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default when omitted)")
			fs.StringVar(&pageToken, "page-token", "", "next page token from the previous response")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req := listJSON(pageSize, pageToken)
			req["ownerId"] = args[0]
			return call(g, env, methodAssetsHoldings, req)
		})
}

func newAssetsLedgerCmd(g *GlobalFlags) *verb {
	var pageSize int
	var pageToken, defCode string
	var ascending bool
	return newVerb(g, "ledger", "list an owner's ledger entries (audit trail)", "assets ledger [--def-code <c>] [--ascending] [--page-size <n>] [--page-token <t>] <owner-id>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&defCode, "def-code", "", "filter by asset code")
			fs.BoolVar(&ascending, "ascending", false, "sort by time ascending (oldest first); default is descending (newest first)")
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default when omitted)")
			fs.StringVar(&pageToken, "page-token", "", "next page token from the previous response")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req := listJSON(pageSize, pageToken)
			req["ownerId"] = args[0]
			if defCode != "" {
				req["defCode"] = defCode
			}
			if ascending {
				req["ascending"] = true
			}
			return call(g, env, methodAssetsUserLedger, req)
		})
}
