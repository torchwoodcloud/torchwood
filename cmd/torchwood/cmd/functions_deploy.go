package cmd

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lynx-go/commands"
)

// functions deploy（docs/design/functions-v3.md §5.2 部署摩擦配套）：目录
// 内存打 zip（排除 node_modules/.git——对齐 E 切片的构建期拒收语义提前
// 规避）→ CreateDeployment（base64 gRPC 通道）→ --watch 轮询构建状态直到
// ready/failed（失败输出 error 尾部但**不退出**，继续 watch 目录变更重部署，
// 修一行存盘即重新构建）→ Ctrl-C 退出。
//
// 纯 CLI 包装，无服务端改动（§5.2）。
const (
	deployMaxCodeBytes = 1 << 20                // proto CreateDeploymentRequest 契约：>1MiB 走 multipart API
	deployPollEvery    = 1 * time.Second        // 构建状态轮询间隔（§5.2）
	deployPollTimeout  = 5 * time.Minute        // 单次构建轮询上限
	deployWatchEvery   = 500 * time.Millisecond // watch 模式目录变更轮询间隔（与 dev 同款零依赖轮询）
)

// newFunctionsDeployCmd 一条命令部署：zip → CreateDeployment → 状态回显。
func newFunctionsDeployCmd(g *globalFlags) *verb {
	var dir, functionID string
	var watch bool
	return newVerb(g, "deploy", "deploy a function directory: zip in memory, create deployment, optionally watch the build", "functions deploy --function-id <id> [--dir .] [--watch]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&functionID, "function-id", "", "function ID (required)")
			fs.StringVar(&dir, "dir", ".", "function directory to deploy (index.js expected; node_modules/.git excluded)")
			fs.BoolVar(&watch, "watch", false, "poll the build status until ready/failed; on failure keep watching the directory and redeploy on change (Ctrl-C to stop)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			if functionID == "" {
				return fmt.Errorf("--function-id is required")
			}
			return runDeploy(g, env.Stdout, deployOptions{
				functionID:  functionID,
				dir:         dir,
				watch:       watch,
				pollEvery:   deployPollEvery,
				pollTimeout: deployPollTimeout,
				watchEvery:  deployWatchEvery,
			})
		})
}

// deployOptions 是 functions deploy 的参数载体（测试直接构造）。
type deployOptions struct {
	functionID  string
	dir         string
	watch       bool
	pollEvery   time.Duration
	pollTimeout time.Duration
	watchEvery  time.Duration
}

// zipFunctionDir 把函数目录打进内存 zip：排除 node_modules / .git（任意
// 层级；E 切片将在构建期拒收 node_modules，CLI 提前剔除避免无效上传）与
// 常见 VCS 噪声。空目录（无任何文件）报错。
func zipFunctionDir(dir string) ([]byte, error) {
	// 先 Walk 收集文件清单、后读取：文件读取不落在 WalkDir 回调内（G122
	// symlink TOCTOU 语义下回调内 fs 操作是告警点；收集与读取分离语义等价）。
	type fileEntry struct{ abs, rel string }
	var files []fileEntry
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // 符号链接等非常规文件不入包（部署包语义只收源码）
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files = append(files, fileEntry{abs: path, rel: rel})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to walk --dir: %v", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("--dir %q has no files to deploy", dir)
	}

	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, f := range files {
		// f.abs 来自上面对 --dir 的 WalkDir 限定遍历（用户显式指定的本地
		// 函数目录；部署包语义即读取本地源码），非任意文件包含面（G304）。
		data, err := os.ReadFile(f.abs) // #nosec G304
		if err != nil {
			return nil, err
		}
		entry, err := w.Create(strings.ReplaceAll(f.rel, string(filepath.Separator), "/"))
		if err != nil {
			return nil, err
		}
		if _, err := entry.Write(data); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("failed to build zip: %v", err)
	}
	return buf.Bytes(), nil
}

// buildCreateDeploymentReq 一次性完成目录 → zip → 请求 map 的装配：>1MiB
// 显式报错并提示 multipart 通道（proto CreateDeploymentRequest 契约；gRPC
// 通道虽到 8MiB，代码包契约上限为 1MiB）。
func buildCreateDeploymentReqFromDir(functionID, dir string) (map[string]any, error) {
	code, err := zipFunctionDir(dir)
	if err != nil {
		return nil, err
	}
	if len(code) > deployMaxCodeBytes {
		return nil, fmt.Errorf("zipped code is %d bytes, exceeding the 1MiB gRPC channel contract; use the multipart upload API (POST /v1/server/functions/%s/deployments/code, 50MiB max)", len(code), functionID)
	}
	return map[string]any{"functionId": functionID, "code": base64.StdEncoding.EncodeToString(code)}, nil
}

// deploymentView 是 GetDeployment 响应的轮询字段视图。
type deploymentView struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

// parseDeploymentView 解析 GetDeployment 响应 JSON。
func parseDeploymentView(data []byte) (deploymentView, error) {
	var v deploymentView
	if err := json.Unmarshal(data, &v); err != nil {
		return deploymentView{}, fmt.Errorf("failed to parse deployment response: %v", err)
	}
	return v, nil
}

// pollDeployment 轮询构建状态直到 ready/failed（1s 间隔，上限 pollTimeout）。
// 返回终态视图。
func pollDeployment(g *globalFlags, functionID, deploymentID string, pollEvery, pollTimeout time.Duration) (deploymentView, error) {
	deadline := time.Now().Add(pollTimeout)
	for {
		resp, err := invoke(g, methodFunctionsGetDeployment, map[string]any{"functionId": functionID, "deploymentId": deploymentID})
		if err != nil {
			return deploymentView{}, err
		}
		view, err := parseDeploymentView(resp)
		if err != nil {
			return deploymentView{}, err
		}
		switch view.Status {
		case "ready", "failed":
			return view, nil
		}
		if time.Now().After(deadline) {
			return view, fmt.Errorf("build still %q after %s (deployment %s); check the server logs", view.Status, pollTimeout, deploymentID)
		}
		time.Sleep(pollEvery)
	}
}

// deployOnce 完成「zip → CreateDeployment →（watch）轮询构建状态」一整轮，
// 返回人类可读结果行。构建失败输出 Error 字段（§5.2：失败输出 error 尾部）。
// 进度输出失败不致命（stdout 仅为提示面），显式弃错满足 errcheck。
func deployOnce(g *globalFlags, stdout io.Writer, opts deployOptions) error {
	say := func(format string, a ...any) {
		_, _ = fmt.Fprintf(stdout, format, a...)
	}
	req, err := buildCreateDeploymentReqFromDir(opts.functionID, opts.dir)
	if err != nil {
		return err
	}
	say("uploading %s ...\n", opts.dir)
	resp, err := invoke(g, methodFunctionsCreateDeployment, req)
	if err != nil {
		return err
	}
	created, err := parseDeploymentView(resp)
	if err != nil {
		return err
	}
	say("deployment %s created, status=%s\n", created.ID, created.Status)

	view, err := pollDeployment(g, opts.functionID, created.ID, opts.pollEvery, opts.pollTimeout)
	if err != nil {
		return err
	}
	if view.Status == "failed" {
		// §5.2：失败输出 error 尾部（GetDeployment 的 Error 字段）。
		say("deployment %s failed: %s\n", view.ID, view.Error)
		return fmt.Errorf("build failed (deployment %s)", view.ID)
	}
	say("deployment %s ready\n", view.ID)
	return nil
}

// runDeploy 主流程：非 watch = 单轮部署（含构建状态回显）；watch = 首轮 +
// 目录变更重部署循环（构建失败不退出，继续 watch——修完存盘即重部署）。
func runDeploy(g *globalFlags, stdout io.Writer, opts deployOptions) error {
	if opts.pollEvery <= 0 {
		opts.pollEvery = deployPollEvery
	}
	if opts.pollTimeout <= 0 {
		opts.pollTimeout = deployPollTimeout
	}
	if opts.watchEvery <= 0 {
		opts.watchEvery = deployWatchEvery
	}
	if err := validateFunctionDir(opts.dir); err != nil {
		return err
	}
	if err := deployOnce(g, stdout, opts); err != nil {
		// watch 模式构建失败不退出：报错后继续 watch（§5.2 DX 语义）。
		if !opts.watch {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "watching %s for changes (build failed; fix the code and it will redeploy, Ctrl-C to stop)\n", opts.dir)
	}
	if !opts.watch {
		return nil
	}
	return watchAndRedeploy(g, stdout, opts)
}

// watchAndRedeploy 目录 mtime 轮询（零依赖）：任一入包文件变更即重部署，
// 直到 Ctrl-C。
func watchAndRedeploy(g *globalFlags, stdout io.Writer, opts deployOptions) error {
	snapshot := map[string]time.Time{}
	scan := func() error {
		next := map[string]time.Time{}
		err := filepath.WalkDir(opts.dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "node_modules", ".git":
					return filepath.SkipDir
				}
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			next[path] = info.ModTime()
			return nil
		})
		if err != nil {
			return err
		}
		snapshot = next
		return nil
	}
	if err := scan(); err != nil {
		return err
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	_, _ = fmt.Fprintf(stdout, "watching %s (change → redeploy; Ctrl-C to stop)\n", opts.dir)
	ticker := time.NewTicker(opts.watchEvery)
	defer ticker.Stop()
	for {
		select {
		case <-sigs:
			_, _ = fmt.Fprintln(stdout, "\nstopping.")
			return nil
		case <-ticker.C:
			prev := snapshot
			if err := scan(); err != nil {
				continue // 瞬态（编辑器原子替换窗口）跳过本轮
			}
			if sameFileStates(prev, snapshot) {
				continue
			}
			_, _ = fmt.Fprintln(stdout, "change detected, redeploying...")
			if err := deployOnce(g, stdout, opts); err != nil {
				// 构建失败不退出（--watch 语义）：报错后继续 watch。
				_, _ = fmt.Fprintf(stdout, "deploy failed: %v (keep watching)\n", err)
			}
		}
	}
}

// sameFileStates 比较两次目录快照（路径 → mtime）是否一致。
func sameFileStates(a, b map[string]time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || !v.Equal(w) {
			return false
		}
	}
	return true
}
