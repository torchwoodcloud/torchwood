package runbook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// runbook 引擎（docs/design/runbook.md §2.3 引擎算法，D8/D9/D13/D14/D16/D18/
// D21）：状态拉取与对账、up/down 主循环（含 CAS 收敛）、status 报告与
// forgive 逃生门。
//
// 调用层注入：引擎不绑定任何具体 RPC 通道，一切 RPC 走 Caller 抽象——生产
// 路径由命令方（cli 包）把 InvokeJSON 适配成 Caller，测试注入假实现即可覆盖
// 决策层。

// ---------------------------------------------------------------------------
// RPC 通道抽象与错误分类
// ---------------------------------------------------------------------------

// Caller 是引擎看到的 RPC 通道：protojson 请求 map 进、响应 map 出。
// 错误约定：可分类的 RPC 错误须以 *CallError 形态返回（携带 gRPC code
// 名），引擎据此走 NotFound/AlreadyExists/FailedPrecondition 收敛分支。
type Caller func(method string, req map[string]any) (map[string]any, error)

// gRPC code 名（server.ErrorCode(err).String() 的产出；调用方不直接 import
// grpc，code 名经 SDK 的错误分类函数取回）。codes.Code.String() 对这三个码
// 恰好输出这些 CamelCase 名，契约由 runbook_engine_test.go 钉死。
const (
	runbookCodeNotFound           = "NotFound"
	runbookCodeAlreadyExists      = "AlreadyExists"
	runbookCodeFailedPrecondition = "FailedPrecondition"
)

// NewCallError 构造携带 gRPC code 名的调用层错误（供生产 Caller 适配器把
// SDK 错误分类成引擎可收敛的形态；code 传空 = 未分类，引擎按普通错误上抛）。
func NewCallError(code string, err error) error {
	return &CallError{code: code, err: err}
}

// CallError 携带 gRPC code 名的调用层错误。Unwrap 链保留原始错误，调用方的
// 退出码映射（2=40x/3=5xx/4=429）不受影响。
type CallError struct {
	code string // gRPC code 名；空 = 未分类（引擎按普通错误上抛）
	err  error
}

func (e *CallError) Error() string { return e.err.Error() }
func (e *CallError) Unwrap() error { return e.err }

// isRunbookCode 报告错误是否为指定 code 的调用层错误。
func isRunbookCode(err error, code string) bool {
	var ce *CallError
	if errors.As(err, &ce) {
		return ce.code == code
	}
	return false
}

// ---------------------------------------------------------------------------
// 状态面
// ---------------------------------------------------------------------------

const (
	// runbookDefaultName 是唯一迁移线名（§7：表留 runbook 列，CLI 不暴露分线）。
	runbookDefaultName = "default"

	runbookMethodGetState = "/torchwood.server.v1.RunbookService/GetRunbookState"
	runbookMethodRecord   = "/torchwood.server.v1.RunbookService/RecordRunbookStep"
	runbookMethodDelete   = "/torchwood.server.v1.RunbookService/DeleteRunbookStep"
)

// runbookStateStep 是服务端已应用记录的引擎侧投影（appliedAt 不参与任何决策）。
type runbookStateStep struct {
	Version  int64
	Name     string
	Checksum string
}

// fetchRunbookState 拉取升序 step 全集（空 = 未应用）。
func fetchRunbookState(c Caller, runbook string) ([]runbookStateStep, error) {
	resp, err := c(runbookMethodGetState, map[string]any{"runbook": runbook})
	if err != nil {
		return nil, fmt.Errorf("fetch runbook state: %w", err)
	}
	raw := runbookAnyList(resp["steps"])
	steps := make([]runbookStateStep, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("fetch runbook state: malformed step entry %v", item)
		}
		var version int64
		switch t := m["version"].(type) {
		case json.Number:
			version, _ = t.Int64()
		case float64:
			version = int64(t)
		case int64:
			version = t
		case int:
			version = int64(t)
		case string:
			// protojson 把 int64 序列化为字符串（"version": "3"）——生产
			// gRPC 链路（InvokeJSON 响应）走的正是这个形状；其余形状留给
			// 测试 fake 与宽容解码器。
			v, perr := strconv.ParseInt(t, 10, 64)
			if perr != nil {
				return nil, fmt.Errorf("fetch runbook state: step version is not a number (%q)", t)
			}
			version = v
		default:
			return nil, fmt.Errorf("fetch runbook state: step version is not a number (%T)", m["version"])
		}
		steps = append(steps, runbookStateStep{
			Version:  version,
			Name:     runbookGotStr(m, "name"),
			Checksum: runbookGotStr(m, "checksum"),
		})
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].Version < steps[j].Version })
	return steps, nil
}

// runbookTopVersion 返回状态顶版（空状态 = 0）。
func runbookTopVersion(steps []runbookStateStep) int64 {
	if len(steps) == 0 {
		return 0
	}
	return steps[len(steps)-1].Version
}

// reconcileRunbookHistory 是共同前置的对账（§2.3）：服务端每个已应用版本必须
// 在本地存在且 checksum 相等，否则 fail（文件被删 = orphan / 被改历史 =
// modified），指引 git 恢复 / forgive / 写新 step（D9/D21）。
func reconcileRunbookHistory(files []runbookFile, steps []runbookStateStep) error {
	local := make(map[int64]*runbookFile, len(files))
	for i := range files {
		local[files[i].Version] = &files[i]
	}
	for _, s := range steps {
		f, ok := local[s.Version]
		if !ok {
			return fmt.Errorf("server has step %06d (%s) applied but no local file exists (orphan) — restore the file from git, or acknowledge with `runbook forgive %d` and re-run up", s.Version, s.Name, s.Version)
		}
		if f.Checksum != s.Checksum {
			return fmt.Errorf("runbook file %s was modified after version %06d was applied (checksum mismatch) — restore it via git, acknowledge the change with `runbook forgive %d`, or express the change as a new step", f.FileName, s.Version, s.Version)
		}
	}
	return nil
}

// loadRunbookEngineDir = 文件层加载 + 阶段 C 动词 schema 校验（up/down/status/
// forgive 的统一入口；D13：坏输入直接 fail）。
func loadRunbookEngineDir(dir string) ([]runbookFile, error) {
	files, err := loadRunbookDir(dir)
	if err != nil {
		return nil, err
	}
	if err := validateRunbookVerbs(files); err != nil {
		return nil, err
	}
	return files, nil
}

// ---------------------------------------------------------------------------
// up / down / status / forgive
// ---------------------------------------------------------------------------

// RunOptions 汇聚四个引擎命令的公共旗标。
type RunOptions struct {
	Dir    string
	To     int64
	ToSet  bool // --to 显式给出（0 在 down 里是合法目标值，须用 presence 区分）
	All    bool // down --all = --to 0
	DryRun bool
	Quiet  bool
}

// UsageError 标记「越界的目标版本」类错误（D14）：命令层转成 UsageError
// （带 usage 行、退出码 1）。
type UsageError struct{ Err error }

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }

// StepReport 是 summary 里一个 step 的形态。
type StepReport struct {
	Version int64          `json:"version"`
	Name    string         `json:"name"`
	Actions []ActionReport `json:"actions"`
}

// UpSummary 是 up 的 stdout 契约（§2.1：单个 JSON summary）。dry-run 下
// applied 列出的是计划内容（result=planned），WouldFail 携带中止原因（退出码
// 仍为 0，§5 退出码契约「--dry-run 一律 0」）。
type UpSummary struct {
	Runbook        string       `json:"runbook"`
	CurrentVersion int64        `json:"current_version"`
	Applied        []StepReport `json:"applied"`
	DurationMs     int64        `json:"duration_ms"`
	DryRun         bool         `json:"dry_run"`
	WouldFail      string       `json:"would_fail,omitempty"`
}

// DownSummary 与 up 同构；键名用 reverted（down 的 applied 是误导）。
type DownSummary struct {
	Runbook        string       `json:"runbook"`
	CurrentVersion int64        `json:"current_version"`
	Reverted       []StepReport `json:"reverted"`
	DurationMs     int64        `json:"duration_ms"`
	DryRun         bool         `json:"dry_run"`
	WouldFail      string       `json:"would_fail,omitempty"`
}

// StatusOut 是 status 的输出契约（D16）。
type StatusOut struct {
	Runbook        string       `json:"runbook"`
	CurrentVersion int64        `json:"current_version"`
	Clean          bool         `json:"clean"`
	Files          []StatusFile `json:"files"`
}

type StatusFile struct {
	Version      int64  `json:"version"`
	Name         string `json:"name"`
	State        string `json:"state"` // applied | pending | modified | orphan
	Irreversible bool   `json:"irreversible"`
}

// ForgiveSummary 是 forgive 的摘要输出（D21）。
type ForgiveSummary struct {
	Runbook          string  `json:"runbook"`
	RequestedVersion int64   `json:"requested_version"`
	DeletedVersions  []int64 `json:"deleted_versions"`
	CurrentVersion   int64   `json:"current_version"`
}

// runbookProgressLine 输出一条进度行（stderr；quiet 时静默）。
func runbookProgressLine(stderr io.Writer, quiet bool, format string, args ...any) {
	if quiet || stderr == nil {
		return
	}
	_, _ = fmt.Fprintf(stderr, "[runbook] "+format+"\n", args...)
}

// RunUp 应用到最新/指定版（§2.3 up 算法）：
// 加载+对账 → 从 state.max+1 顺序应用到 target（禁跳版）→ 逐动作 reconcile →
// RecordRunbookStep（CAS 撞 → 重拉校验 checksum：相等继续 / 不等 fatal）。
func RunUp(c Caller, opts RunOptions, stdout, stderr io.Writer) error {
	files, err := loadRunbookEngineDir(opts.Dir)
	if err != nil {
		return err
	}
	steps, err := fetchRunbookState(c, runbookDefaultName)
	if err != nil {
		return err
	}
	if err := reconcileRunbookHistory(files, steps); err != nil {
		return err
	}
	stateMax := runbookTopVersion(steps)
	localMax := files[len(files)-1].Version
	target := localMax
	if opts.ToSet {
		if opts.To < 1 {
			return &UsageError{Err: fmt.Errorf("--to must be a version number >= 1, got %d", opts.To)}
		}
		if opts.To < stateMax {
			return &UsageError{Err: fmt.Errorf("cannot up to version %06d: it is at or below the current version %06d (up only moves forward; use `runbook down`)", opts.To, stateMax)}
		}
		if opts.To > localMax {
			return &UsageError{Err: fmt.Errorf("cannot up to version %06d: no local runbook file for it (latest local is %06d)", opts.To, localMax)}
		}
		target = opts.To
	}

	rec := &runbookReconciler{caller: c, dryRun: opts.DryRun}
	start := time.Now()
	applied := []StepReport{}
	summary := UpSummary{Runbook: runbookDefaultName, CurrentVersion: stateMax, DryRun: opts.DryRun}

	for v := stateMax + 1; v <= target; v++ {
		f := &files[v-1] // 版本链已验证连续从 1 起：index = version-1
		step := StepReport{Version: v, Name: f.Name, Actions: []ActionReport{}}
		if len(f.Up) == 0 {
			runbookProgressLine(stderr, opts.Quiet, "%06d_%s: no-op step (empty up section)", v, f.Name)
		}
		var stepErr error
		for _, a := range f.Up {
			outcome, err := rec.action(a)
			if err != nil {
				stepErr = fmt.Errorf("%06d_%s: %s: %w", v, f.Name, a.Verb, err)
				break
			}
			step.Actions = append(step.Actions, outcome.Report)
			for _, ev := range outcome.Events {
				runbookProgressLine(stderr, opts.Quiet, "applying %06d_%s: %s(%s) ... %s", v, f.Name, ev.Verb, ev.Target, ev.Result)
			}
		}
		if stepErr != nil {
			if opts.DryRun {
				// dry-run 如实预报失败但不算执行失败（§5：--dry-run 一律 0）。
				summary.WouldFail = stepErr.Error()
				break
			}
			return stepErr
		}
		if !opts.DryRun {
			if err := recordRunbookStep(c, v, f); err != nil {
				if isRunbookCode(err, runbookCodeFailedPrecondition) || isRunbookCode(err, runbookCodeAlreadyExists) {
					// CAS 撞（D8 并发双跑；AlreadyExists = 唯一约束兜底漏网）：
					// 重拉校验该版 checksum——相等 = 对方做了同样的事，继续；
					// 不等 = 文件分叉，fatal。
					if cerr := convergeAfterRecordCollision(c, v, f); cerr != nil {
						return cerr
					}
					runbookProgressLine(stderr, opts.Quiet, "%06d_%s: already recorded by a concurrent engine (checksum matches) - continuing", v, f.Name)
				} else {
					return fmt.Errorf("record step %06d_%s: %w", v, f.Name, err)
				}
			}
		}
		applied = append(applied, step)
		summary.CurrentVersion = v
	}

	summary.Applied = applied
	summary.DurationMs = time.Since(start).Milliseconds()
	return writeIndentJSON(stdout, summary)
}

// recordRunbookStep 记录一个已应用 step。expect_prev_version 恒 = version-1
// （0 = 首步断言），CAS 语义全在服务端。
func recordRunbookStep(c Caller, version int64, f *runbookFile) error {
	_, err := c(runbookMethodRecord, map[string]any{
		"runbook":             runbookDefaultName,
		"version":             version,
		"name":                f.Name,
		"checksum":            f.Checksum,
		"expect_prev_version": version - 1,
	})
	return err
}

// convergeAfterRecordCollision 处理 Record CAS 撞后的收敛判定：重拉状态找该
// 版记录，checksum 相等 = 收敛成功（nil）；不等 / 缺席 = fatal。
func convergeAfterRecordCollision(c Caller, version int64, f *runbookFile) error {
	steps, err := fetchRunbookState(c, runbookDefaultName)
	if err != nil {
		return err
	}
	for _, s := range steps {
		if s.Version == version {
			if s.Checksum == f.Checksum {
				return nil
			}
			return fmt.Errorf("version %06d diverges: server checksum %s != local %s (a different %06d_*.yaml was applied elsewhere) — restore the file, or `runbook forgive %d` to re-record", version, s.Checksum, f.Checksum, version, version)
		}
	}
	return fmt.Errorf("version %06d is missing from server state after a CAS collision (state changed underneath; re-run)", version)
}

// RunDown 回退到指定版（§2.3 down 算法）：默认回退一步，--all → 0；
// down 段空 → fail 不可逆；DeleteRunbookStep 顶版撞 → 重拉状态重评估（down
// 语义幂等自然继续）。
func RunDown(c Caller, opts RunOptions, stdout, stderr io.Writer) error {
	if opts.All && opts.ToSet {
		return &UsageError{Err: fmt.Errorf("use either --to or --all, not both")}
	}
	files, err := loadRunbookEngineDir(opts.Dir)
	if err != nil {
		return err
	}
	steps, err := fetchRunbookState(c, runbookDefaultName)
	if err != nil {
		return err
	}
	if err := reconcileRunbookHistory(files, steps); err != nil {
		return err
	}
	current := runbookTopVersion(steps)
	// 未应用任何 step 时 down 是 no-op（current-1 = -1 钳到 0，循环自然跳过）。
	target := current - 1
	if target < 0 {
		target = 0
	}
	if opts.All {
		target = 0
	} else if opts.ToSet {
		if opts.To < 0 {
			return &UsageError{Err: fmt.Errorf("--to must be a version number >= 0, got %d", opts.To)}
		}
		if opts.To >= current {
			return &UsageError{Err: fmt.Errorf("cannot down to version %06d: it is not below the current version %06d (down only moves backward)", opts.To, current)}
		}
		target = opts.To
	}

	rec := &runbookReconciler{caller: c, dryRun: opts.DryRun}
	start := time.Now()
	reverted := []StepReport{}
	summary := DownSummary{Runbook: runbookDefaultName, CurrentVersion: current, DryRun: opts.DryRun}

	for current > target {
		f := &files[current-1]
		if len(f.Down) == 0 {
			stepErr := fmt.Errorf("version %06d (%s) is irreversible: its down section is empty (D4)", current, f.Name)
			if opts.DryRun {
				summary.WouldFail = stepErr.Error()
				break
			}
			return stepErr
		}
		step := StepReport{Version: current, Name: f.Name, Actions: []ActionReport{}}
		var stepErr error
		for _, a := range f.Down {
			outcome, err := rec.action(a)
			if err != nil {
				stepErr = fmt.Errorf("%06d_%s: %s: %w", current, f.Name, a.Verb, err)
				break
			}
			step.Actions = append(step.Actions, outcome.Report)
			for _, ev := range outcome.Events {
				runbookProgressLine(stderr, opts.Quiet, "reverting %06d_%s: %s(%s) ... %s", current, f.Name, ev.Verb, ev.Target, ev.Result)
			}
		}
		if stepErr != nil {
			if opts.DryRun {
				summary.WouldFail = stepErr.Error()
				break
			}
			return stepErr
		}
		if !opts.DryRun {
			if _, err := c(runbookMethodDelete, map[string]any{"runbook": runbookDefaultName, "version": current}); err != nil {
				if isRunbookCode(err, runbookCodeFailedPrecondition) || isRunbookCode(err, runbookCodeNotFound) {
					// 顶版撞（并发方已摘）→ 重拉重评估；本步动作已由我们执行，
					// 仍计入 reverted（报告的是本次回退的工作量）。
					fresh, ferr := fetchRunbookState(c, runbookDefaultName)
					if ferr != nil {
						return ferr
					}
					if newTop := runbookTopVersion(fresh); newTop < current {
						runbookProgressLine(stderr, opts.Quiet, "%06d_%s: step already removed by a concurrent engine - continuing", current, f.Name)
						reverted = append(reverted, step)
						current = newTop
						summary.CurrentVersion = current
						continue
					}
				}
				return fmt.Errorf("delete step %06d: %w", current, err)
			}
		}
		reverted = append(reverted, step)
		current--
		summary.CurrentVersion = current
	}

	summary.Reverted = reverted
	summary.DurationMs = time.Since(start).Milliseconds()
	return writeIndentJSON(stdout, summary)
}

// RunStatus 对账报告（D16）：本地文件 vs 服务端状态，恒退 0（能产出报告
// 就不算失败；加载/目录错误仍按 D13 退出 1，由调用方错误返回承担）。
func RunStatus(c Caller, dir string, stdout io.Writer) error {
	files, err := loadRunbookEngineDir(dir)
	if err != nil {
		return err
	}
	steps, err := fetchRunbookState(c, runbookDefaultName)
	if err != nil {
		return err
	}
	server := make(map[int64]runbookStateStep, len(steps))
	for _, s := range steps {
		server[s.Version] = s
	}
	local := make(map[int64]bool, len(files))
	out := StatusOut{
		Runbook:        runbookDefaultName,
		CurrentVersion: runbookTopVersion(steps),
		Clean:          true,
		Files:          []StatusFile{},
	}
	for i := range files {
		f := &files[i]
		local[f.Version] = true
		state := "pending"
		if s, ok := server[f.Version]; ok {
			if s.Checksum == f.Checksum {
				state = "applied"
			} else {
				state = "modified"
				out.Clean = false
			}
		}
		out.Files = append(out.Files, StatusFile{
			Version:      f.Version,
			Name:         f.Name,
			State:        state,
			Irreversible: len(f.Down) == 0,
		})
	}
	for _, s := range steps {
		if !local[s.Version] {
			out.Files = append(out.Files, StatusFile{
				Version: s.Version,
				Name:    s.Name,
				State:   "orphan",
			})
			out.Clean = false
		}
	}
	return writeIndentJSON(stdout, out)
}

// RunForgive 是 D21 逃生门：从顶版起逐个 DeleteRunbookStep 摘到 ≥N 全清，
// 不执行任何资源动作；随后用户重跑 up（动作幂等 skip + Record 新 checksum）。
// 对账前置仅要求本地存在该版本文件（checksum 此刻就是被质疑的对象，不比较）。
func RunForgive(c Caller, dir string, version int64, stdout io.Writer) error {
	if version < 1 {
		return fmt.Errorf("version must be >= 1, got %d", version)
	}
	files, err := loadRunbookEngineDir(dir)
	if err != nil {
		return err
	}
	localMax := files[len(files)-1].Version
	if version > localMax {
		return fmt.Errorf("no local runbook file for version %06d (latest local is %06d) - forgive only acknowledges versions whose files exist", version, localMax)
	}
	steps, err := fetchRunbookState(c, runbookDefaultName)
	if err != nil {
		return err
	}
	current := runbookTopVersion(steps)
	if version > current {
		return fmt.Errorf("version %06d is not applied (current top is %06d) - nothing to forgive", version, current)
	}
	deleted := []int64{}
	for current >= version {
		if _, err := c(runbookMethodDelete, map[string]any{"runbook": runbookDefaultName, "version": current}); err != nil {
			if isRunbookCode(err, runbookCodeFailedPrecondition) || isRunbookCode(err, runbookCodeNotFound) {
				// 并发方已摘：重拉重评估（版本链单调保证只向下走）。
				fresh, ferr := fetchRunbookState(c, runbookDefaultName)
				if ferr != nil {
					return ferr
				}
				if newTop := runbookTopVersion(fresh); newTop < current {
					current = newTop
					continue
				}
			}
			return fmt.Errorf("delete step %06d: %w", current, err)
		}
		deleted = append(deleted, current)
		current--
	}
	return writeIndentJSON(stdout, ForgiveSummary{
		Runbook:          runbookDefaultName,
		RequestedVersion: version,
		DeletedVersions:  deleted,
		CurrentVersion:   current,
	})
}

// shortRunbookMethod 把全方法名缩为 Service/Method（错误信息可读性）。
func shortRunbookMethod(method string) string {
	parts := strings.Split(strings.TrimPrefix(method, "/"), "/")
	if len(parts) == 2 {
		return parts[0][strings.LastIndex(parts[0], ".")+1:] + "/" + parts[1]
	}
	return method
}

// writeIndentJSON 以缩进 JSON 输出 summary（引擎自带输出，不依赖调用方的
// 表现层助手）。
func writeIndentJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}
