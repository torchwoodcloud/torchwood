package dispatcher

// 本文件是双执行底座共享的镜像构建上下文编排（纯文件编排，零平台/零
// docker 依赖）：zip 解压校验 → runtime 对账 → 模板渲染 → build context
// tar 流。两种驱动（fleetly BuildFromUpload / docker ImageBuild）消费同一
// 实现——构建语义单点，行为分叉即镜像分叉。

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	infrafunctions "github.com/torchwoodcloud/torchwood/internal/infra/functions"
	"github.com/torchwoodcloud/torchwood/internal/infra/functions/runner"
)

// PrepareBuildContext 在 buildDir 准备镜像构建上下文（纯文件编排，单测以
// 临时 zip 直接驱动）。顺序敏感（runtime 对账先于模板渲染）：
//
//  1. 解压 zip 并探测部署源（ExtractZipRelaxed）；
//  2. runtime 一致性对账（D7'）：探测产出语言族（family），版本轴来自
//     声明——opts.Runtime 非空且其 family ≠ 探测 family → InvalidArgument
//     （错误信息含声明 ID 与探测 family）；未知 runtime ID 同样拒绝
//     （fail-closed）；
//  3. node 分支写 .tw-runner.js；go 分支零平台注入（五期 5b：用户 zip 根 =
//     package main + SDK，构建 = go build .，无 bootstrap 生成/入口探测）；
//     最后渲染 Dockerfile（缺 go.sum 等拒收错误在此冒出）。
func PrepareBuildContext(buildDir string, opts BuildImageOptions) error {
	tmpZip, err := os.CreateTemp("", "torchwood-dispatch-src-*.zip")
	if err != nil {
		return fmt.Errorf("stage zip: %w", err)
	}
	defer func() { _ = os.Remove(tmpZip.Name()) }()
	if _, err := tmpZip.Write(opts.Zip); err != nil {
		_ = tmpZip.Close()
		return fmt.Errorf("stage zip: %w", err)
	}
	_ = tmpZip.Close()

	// 解压 + 探测：复用 v1 防炸弹/路径穿越预算，条目预算用放宽版（二期
	// 阶段 3，设计 §2 条目维链条）——BuildImage 无法区分 zip/git 源（同
	// base64 内联通道），统一放宽到 packer 物化口径（条目 5000；单条
	// 100MiB / 总量 200MiB 解压预算维持），防 git 源合法 zip 被默认 1000
	// 条目预算击毙。SourceContents 附带部署源探测结果（family 标记），
	// 逐字段映射为 runner 包的模板载体（runner 保持叶子资产包，不 import
	// infra/functions 根包）。
	contents, err := infrafunctions.ExtractZipRelaxed(tmpZip.Name(), buildDir)
	if err != nil {
		return err
	}

	// runtime 一致性对账（D7'，functions-runtime-selection.md §2）：探测产出
	// 语言族，版本轴来自声明——声明的 runtime ID 的 family 必须与探测
	// family 一致，否则构建期 InvalidArgument。opts.Runtime 为空 = 遗留
	// 调用方（跳过对账）：渲染基准取探测 family 的首个 active 表项（平台
	// 缺省 runtime），不再隐含历史 node:18 耦合。
	renderRuntime := opts.Runtime
	if opts.Runtime != "" {
		declaredFamily := domainfunctions.FamilyOf(opts.Runtime)
		if declaredFamily == "" {
			return status.Errorf(codes.InvalidArgument, "unsupported runtime %q", opts.Runtime)
		}
		if declaredFamily != contents.Family {
			return status.Errorf(codes.InvalidArgument,
				"runtime mismatch: function declares %q (family %q) but source probes as %q family (redeploy with a runtime whose family matches the source)",
				opts.Runtime, declaredFamily, contents.Family)
		}
	} else {
		def, ok := domainfunctions.DefaultRuntimeForFamily(contents.Family)
		if !ok {
			return status.Errorf(codes.InvalidArgument, "source probes as %q family but no runtime is available for platform builds", contents.Family)
		}
		renderRuntime = def.ID
	}

	// engines.node 校验（functions-runtime-selection.md §5）：Node 生态正规
	// 声明位与所选 runtime 的 major 相交性——「本地 node 22、线上 node 18」
	// 类漂移在部署期显式化。与 D11 正交（纯读取 + 比较，零新执行面）。
	if err := checkNodeEngines(contents.NodeEngines, renderRuntime); err != nil {
		return err
	}

	dockerfile, err := runner.DockerfileFor(runner.SourceContents{
		Runtime:       renderRuntime,
		NodeDeps:      contents.NodeDeps,
		HasLockfile:   contents.HasLockfile,
		GoModulePath:  contents.GoModulePath,
		GoHasRequires: contents.GoHasRequires,
		GoHasSum:      contents.GoHasSum,
		HasVendor:     contents.HasVendor,
	})
	if err != nil {
		return err
	}
	// node 分支：runner 脚本随 COPY . . 进镜像（go 分支写入该文件无害——
	// go 模板不引用它）。构建产物经 COPY 进入镜像后须被镜像内 USER 读取
	// ——权限必须保持 world-readable（G306 误报：非机密，且收紧曾致容器
	// 秒退、健康握手永不 ready，CI e2e 实证）。
	if err := os.WriteFile(filepath.Join(buildDir, runner.RunnerFileName), runner.NodeRunnerJS(), 0o644); err != nil { // #nosec G306 -- 镜像内 USER 须可读
		return fmt.Errorf("write runner: %w", err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil { // #nosec G306 -- 镜像内 USER 须可读
		return fmt.Errorf("write dockerfile: %w", err)
	}
	return nil
}

// TarDir 将目录流式打包为 build context tar（失败路径调用方须 Close 读端，
// 唤醒阻塞在 pipe 写侧的打包 goroutine）。
//
// 流式化（P2 S13）：原实现把整棵 tar 缓冲进 bytes.Buffer——50MiB zip 场景
// （解压预算 200MiB）构建峰值内存 ≈ tar 缓冲 + 解压目录 + base64 载荷
// ≈ 800MB 量级。现改 io.Pipe + goroutine 边 WalkDir 边写，调用方
// （BuildFromUpload 接受 io.Reader）直接消费读端，峰值内存降为单文件拷贝
// 缓冲。错误经 pipe 传播（CloseWithError，读侧以读错误收场——遍历目录是
// 刚由 PrepareBuildContext 写出的自有文件，出错概率极低，接受错误文案
// 不经「tar build context」包装）。调用方在 BuildFromUpload 失败路径须
// Close 读端，唤醒阻塞中的写侧（防 goroutine 悬挂）。
//
// umask 归一化语义（EACCES 秒退事故的坏档修复）原样保留：文件恒 0644、
// 目录恒 0755、属主归零——见循环内注释。
func TarDir(dir string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(writeBuildContext(pw, dir))
	}()
	return pr
}

// writeBuildContext 是 TarDir 的写侧：遍历 dir 逐条目写入 tar 流（与流式化
// 前的字节语义一致）。
func writeBuildContext(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		// 权限坏档修复（EACCES 秒退事故）：镜像内文件 mode 不得依赖 dispatcher
		// 进程状态。上游 os.WriteFile/OpenFile 声明的 0644 会先被进程 umask 掩蔽
		// （0644 & ~umask，umask 0077 时落盘 0600），而 FileInfoHeader 忠实保留
		// 磁盘实际 mode，经 COPY . . 原样进镜像——模板 USER node 读 .tw-runner.js
		// 即 EACCES、容器秒退（同构建代码先后产出坏/好镜像 = 进程 umask 随栈
		// redeploy 漂移的状态依赖）。在此单一收口点归一化：文件恒 0644、目录恒
		// 0755（x 位不可省，子目录用户代码要靠它遍历）、属主归零，镜像权限与
		// dispatcher 以何用户/何 umask 运行彻底解耦。不用模板 COPY --chmod：
		// 它对文件与目录只能给同一个 mode，顾此失彼。
		if d.IsDir() {
			hdr.Mode = 0o755
			hdr.Name += "/"
		} else {
			hdr.Mode = 0o644
		}
		hdr.Uid = 0
		hdr.Gid = 0
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !d.IsDir() {
			f, err := os.Open(path) // #nosec G304,G122 -- path 由 WalkDir 从自有构建目录枚举（非用户输入；目录为本包 PrepareBuildContext 刚写出的临时目录，无符号链接注入面）
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tw, f)
			_ = f.Close()
			if copyErr != nil {
				return copyErr
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}
