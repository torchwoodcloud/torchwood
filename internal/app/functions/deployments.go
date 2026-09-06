package functions

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	appshared "github.com/torchwooddev/torchwood/internal/app/shared"
	domainfunctions "github.com/torchwooddev/torchwood/internal/domain/functions"
	"github.com/torchwooddev/torchwood/pkg/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// maxDeploymentCodeBytes 是 zip 代码包上限（multipart 路径限流）。
const maxDeploymentCodeBytes = 50 << 20 // 50 MiB

// zipDir 是本地 zip 代码包根目录（单机部署假设：server 与 worker 共享文件系统）。
const zipDir = "torchwood-functions"

type CreateDeploymentCommand struct {
	ProjectID  string
	FunctionID string
	Code       []byte // zip 字节流
}

func (f *Functions) CreateDeployment(ctx context.Context, cmd CreateDeploymentCommand) (*domainfunctions.Deployment, error) {
	// 纵深防御（G2-1/R06-P0，G12 调整）：部署写操作允许 admin 会话与 API key。
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	if len(cmd.Code) == 0 {
		return nil, status.Error(codes.InvalidArgument, "code is required")
	}
	if len(cmd.Code) > maxDeploymentCodeBytes {
		return nil, status.Errorf(codes.InvalidArgument, "code exceeds maximum size of %d bytes", maxDeploymentCodeBytes)
	}
	if !isZip(cmd.Code) {
		return nil, status.Error(codes.InvalidArgument, "invalid zip file: missing PK zip signature")
	}
	fn, err := f.repo.GetFunction(ctx, cmd.ProjectID, cmd.FunctionID)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, status.Error(codes.NotFound, "function not found")
	}

	now := time.Now()
	dep := &domainfunctions.Deployment{
		ID:         idgen.UUID().String(),
		FunctionID: cmd.FunctionID,
		ProjectID:  cmd.ProjectID,
		Size:       int64(len(cmd.Code)),
		Status:     domainfunctions.DeploymentStatusPending,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := f.repo.CreateDeployment(ctx, dep); err != nil {
		return nil, err
	}

	path := zipPath(cmd.ProjectID, cmd.FunctionID, dep.ID)
	if err := writeZip(path, cmd.Code); err != nil {
		return nil, fmt.Errorf("write code package: %w", err)
	}

	// 同步构建（MVP 定案：不在独立构建队列，请求内完成）。
	if err := f.buildDeployment(ctx, dep, path); err != nil {
		// 信号量满或状态写回失败：删除 deployment 行与本地 zip，避免残留 pending 行。
		_ = f.repo.DeleteDeployment(ctx, cmd.ProjectID, cmd.FunctionID, dep.ID)
		_ = removeZip(cmd.ProjectID, cmd.FunctionID, dep.ID)
		return nil, err
	}
	return dep, nil
}

// buildDeployment 占用构建信号量并同步构建镜像；结果写入 dep 状态并落库。
// 信号量满仅返回 ResourceExhausted——是否清理 deployment 行与 zip 是调用方
// 的决策（CreateDeployment 清理本次刚建的 pending 行；worker 补构建路径
// 保留既有 deployment，靠队列重试在信号量释放后重建）。
func (f *Functions) buildDeployment(ctx context.Context, dep *domainfunctions.Deployment, path string) error {
	ok, release, err := f.getBuildSemaphore().TryAcquire(ctx)
	if err != nil {
		return status.Errorf(codes.Internal, "acquire build semaphore: %v", err)
	}
	if !ok {
		return status.Error(codes.ResourceExhausted, "too many concurrent builds")
	}
	defer release()

	dep.Status = domainfunctions.DeploymentStatusBuilding
	dep.UpdatedAt = time.Now()
	if err := f.repo.UpdateDeployment(ctx, dep); err != nil {
		return err
	}

	err = f.executor.Build(ctx, dep.FunctionID, dep.ID, path)
	dep.UpdatedAt = time.Now()
	if err != nil {
		dep.Status = domainfunctions.DeploymentStatusFailed
		dep.Error = truncate(err.Error(), maxOutputBytes)
		_ = f.repo.UpdateDeployment(ctx, dep)
		// 清理本地 zip 与可能残留的镜像（幂等）。
		_ = removeZip(dep.ProjectID, dep.FunctionID, dep.ID)
		_ = f.executor.RemoveImage(ctx, dep.FunctionID, dep.ID)
		return nil
	}
	dep.Status = domainfunctions.DeploymentStatusReady
	dep.Error = ""
	if err := f.repo.UpdateDeployment(ctx, dep); err != nil {
		return err
	}
	return nil
}

func (f *Functions) ListDeployments(ctx context.Context, projectID, functionID string) ([]domainfunctions.Deployment, error) {
	if _, err := f.repo.GetFunction(ctx, projectID, functionID); err != nil {
		return nil, err
	}
	return f.repo.ListDeployments(ctx, projectID, functionID)
}

func (f *Functions) GetDeployment(ctx context.Context, projectID, functionID, deploymentID string) (*domainfunctions.Deployment, error) {
	fn, err := f.repo.GetFunction(ctx, projectID, functionID)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, status.Error(codes.NotFound, "function not found")
	}
	dep, err := f.repo.GetDeployment(ctx, projectID, functionID, deploymentID)
	if err != nil {
		return nil, err
	}
	if dep == nil {
		return nil, status.Error(codes.NotFound, "deployment not found")
	}
	return dep, nil
}

// DeleteDeployment 删除顺序：先 DB 级联删除 → 再 docker image rm → 最后删本地 zip
// （全部幂等，失败仅记日志），避免进行中构建/执行读到半删除状态。
func (f *Functions) DeleteDeployment(ctx context.Context, projectID, functionID, deploymentID string) error {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	fn, err := f.repo.GetFunction(ctx, projectID, functionID)
	if err != nil {
		return err
	}
	if fn == nil {
		return status.Error(codes.NotFound, "function not found")
	}
	dep, err := f.repo.GetDeployment(ctx, projectID, functionID, deploymentID)
	if err != nil {
		return err
	}
	if dep == nil {
		return status.Error(codes.NotFound, "deployment not found")
	}
	if err := f.repo.DeleteDeployment(ctx, projectID, functionID, deploymentID); err != nil {
		return err
	}
	_ = f.executor.RemoveImage(ctx, functionID, deploymentID)
	_ = removeZip(projectID, functionID, deploymentID)
	return nil
}

// zipRoot 返回本地 zip 代码包根目录（单机部署假设）。
func zipRoot() string {
	return filepath.Join(os.TempDir(), zipDir)
}

// zipPath 返回本地 zip 代码包路径；functionID 等组件先 filepath.Base 消毒，
// 防止 `../../` 等路径穿越逃逸 zip 根目录。
func zipPath(projectID, functionID, deploymentID string) string {
	return filepath.Join(zipRoot(), filepath.Base(projectID), filepath.Base(functionID), filepath.Base(deploymentID)+".zip")
}

// assertZipDir 断言 path 的父目录位于 zip 根目录前缀内（纵深防御）。
func assertZipDir(path string) error {
	root := filepath.Clean(zipRoot())
	dir := filepath.Clean(filepath.Dir(path))
	if dir != root && !strings.HasPrefix(dir, root+string(os.PathSeparator)) {
		return fmt.Errorf("zip path escapes functions root: %q", path)
	}
	return nil
}

func writeZip(path string, code []byte) error {
	if err := assertZipDir(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, code, 0o600)
}

func removeZip(projectID, functionID, deploymentID string) error {
	path := zipPath(projectID, functionID, deploymentID)
	if err := assertZipDir(path); err != nil {
		return err
	}
	return os.Remove(path)
}

// isZip 校验 zip 魔数 PK\x03\x04（空 zip 为 PK\x05\x06，一并拒绝）。
func isZip(code []byte) bool {
	return len(code) >= 4 && bytes.Equal(code[:4], []byte{0x50, 0x4B, 0x03, 0x04})
}
