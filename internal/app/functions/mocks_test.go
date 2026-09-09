package functions

import (
	"context"
	"sync"
	"time"

	domainfunctions "github.com/torchwooddev/torchwood/internal/domain/functions"
)

// mockRepo 是 FunctionRepo 的内存实现（测试用）。
type mockRepo struct {
	mu            sync.Mutex
	functions     map[string]*domainfunctions.Function
	deployments   map[string]*domainfunctions.Deployment
	variables     map[string]map[string]string
	executions    map[string]*domainfunctions.ExecutionRecord
	recoverEach   int
	recoverCalls  []string
	recoverLimits []int
}

func newMockRepo() *mockRepo {
	return &mockRepo{
		functions:   map[string]*domainfunctions.Function{},
		deployments: map[string]*domainfunctions.Deployment{},
		variables:   map[string]map[string]string{},
		executions:  map[string]*domainfunctions.ExecutionRecord{},
	}
}

func (r *mockRepo) CreateFunction(_ context.Context, fn *domainfunctions.Function) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.functions[fn.ID] = fn
	return nil
}

func (r *mockRepo) GetFunction(_ context.Context, projectID, functionID string) (*domainfunctions.Function, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn := r.functions[functionID]
	if fn == nil || fn.ProjectID != projectID {
		return nil, nil
	}
	return fn, nil
}

func (r *mockRepo) ListFunctions(_ context.Context, projectID string) ([]domainfunctions.Function, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domainfunctions.Function
	for _, fn := range r.functions {
		if fn.ProjectID == projectID {
			out = append(out, *fn)
		}
	}
	return out, nil
}

func (r *mockRepo) UpdateFunction(_ context.Context, fn *domainfunctions.Function) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.functions[fn.ID] = fn
	return nil
}

func (r *mockRepo) DeleteFunction(_ context.Context, projectID, functionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fn := r.functions[functionID]; fn != nil && fn.ProjectID == projectID {
		delete(r.functions, functionID)
	}
	return nil
}

func (r *mockRepo) CreateDeployment(_ context.Context, d *domainfunctions.Deployment) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deployments[d.ID] = d
	return nil
}

func (r *mockRepo) GetDeployment(_ context.Context, projectID, functionID, deploymentID string) (*domainfunctions.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.deployments[deploymentID]
	if d == nil || d.FunctionID != functionID || d.ProjectID != projectID {
		return nil, nil
	}
	return d, nil
}

func (r *mockRepo) ListDeployments(_ context.Context, projectID, functionID string) ([]domainfunctions.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domainfunctions.Deployment
	for _, d := range r.deployments {
		if d.FunctionID == functionID && d.ProjectID == projectID {
			out = append(out, *d)
		}
	}
	return out, nil
}

func (r *mockRepo) UpdateDeployment(_ context.Context, d *domainfunctions.Deployment) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deployments[d.ID] = d
	return nil
}

func (r *mockRepo) ActivateDeployment(_ context.Context, d *domainfunctions.Deployment) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d2 := *d
	d2.Status = domainfunctions.DeploymentStatusReady
	r.deployments[d.ID] = &d2
	if fn := r.functions[d.FunctionID]; fn != nil && fn.ProjectID == d.ProjectID {
		fn.LatestReadyDeploymentID = d.ID
	}
	return nil
}

func (r *mockRepo) DeleteDeployment(_ context.Context, projectID, functionID, deploymentID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.deployments[deploymentID]
	// projectID 断言（R08-P3-5）：跨项目/跨函数同 id 部署不得被删除。
	if d != nil && (d.FunctionID != functionID || d.ProjectID != projectID) {
		return nil
	}
	delete(r.deployments, deploymentID)
	return nil
}

func (r *mockRepo) SetVariables(_ context.Context, projectID, functionID string, vars map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.variables[functionID] = vars
	return nil
}

func (r *mockRepo) GetVariables(_ context.Context, projectID, functionID string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	for k, v := range r.variables[functionID] {
		out[k] = v
	}
	return out, nil
}

func (r *mockRepo) CreateExecution(_ context.Context, e *domainfunctions.ExecutionRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.executions[e.ID] = e
	return nil
}

func (r *mockRepo) GetExecution(_ context.Context, projectID, functionID, executionID string) (*domainfunctions.ExecutionRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.executions[executionID]
	if e == nil || e.FunctionID != functionID || e.ProjectID != projectID {
		return nil, nil
	}
	return e, nil
}

func (r *mockRepo) ListExecutions(_ context.Context, projectID, functionID string, limit int) ([]domainfunctions.ExecutionRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domainfunctions.ExecutionRecord
	for _, e := range r.executions {
		if e.FunctionID == functionID && e.ProjectID == projectID {
			out = append(out, *e)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *mockRepo) UpdateExecution(_ context.Context, e *domainfunctions.ExecutionRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.executions[e.ID] = e
	return nil
}

func (r *mockRepo) TransitionExecutionStatus(_ context.Context, projectID, functionID, executionID, from, to string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.executions[executionID]
	if e == nil || e.FunctionID != functionID || e.ProjectID != projectID || e.Status != from {
		return false, nil
	}
	e.Status = to
	e.UpdatedAt = time.Now()
	return true, nil
}

func (r *mockRepo) FailExecutionIfActive(_ context.Context, projectID, functionID, executionID, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.executions[executionID]
	if e == nil || e.FunctionID != functionID || e.ProjectID != projectID {
		return nil
	}
	switch e.Status {
	case domainfunctions.ExecutionStatusQueued,
		domainfunctions.ExecutionStatusBuilding,
		domainfunctions.ExecutionStatusRunning:
		e.Status = domainfunctions.ExecutionStatusFailed
		e.Error = reason
		e.UpdatedAt = time.Now()
	}
	return nil
}

func (r *mockRepo) RecoverOrphanExecutionsInProject(_ context.Context, projectID string, olderThan time.Time, limit int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recoverCalls = append(r.recoverCalls, projectID)
	r.recoverLimits = append(r.recoverLimits, limit)
	if r.recoverEach <= 0 {
		return 0, nil
	}
	n := r.recoverEach
	if n > limit {
		n = limit
	}
	return int64(n), nil
}

func (r *mockRepo) PruneOldExecutionsInProject(_ context.Context, projectID, functionID string, keepRecent int) error {
	return nil
}

func (r *mockRepo) PruneTriggerExecutionsInProject(_ context.Context, projectID, functionID string, olderThan time.Time) error {
	return nil
}

func (r *mockRepo) GetExecutionByIdempotencyKey(_ context.Context, projectID, functionID, userID, key string) (*domainfunctions.ExecutionRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.executions {
		if e.ProjectID == projectID && e.FunctionID == functionID && e.InvokingUserID == userID && e.ClientIdempotencyKey == key {
			cp := *e
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *mockRepo) CountClientInvocations(_ context.Context, projectID, functionID, userID string, since time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.executions {
		if e.ProjectID == projectID && e.FunctionID == functionID && e.InvokingUserID == userID &&
			e.TriggerSource == domainfunctions.TriggerSourceClient && !e.CreatedAt.Before(since) {
			n++
		}
	}
	return n, nil
}

// mockQueue 是 shared.Queue 的内存实现（测试用）。
type mockQueue struct {
	enqueued [][]byte
	err      error
}

func newMockQueue() *mockQueue {
	return &mockQueue{}
}

func (q *mockQueue) Trim(context.Context, string, int64) error { return nil }

func (q *mockQueue) Enqueue(_ context.Context, _ string, payload []byte) error {
	if q.err != nil {
		return q.err
	}
	q.enqueued = append(q.enqueued, payload)
	return nil
}

func (q *mockQueue) Dequeue(_ context.Context, _ string, _ time.Duration) ([]byte, string, error) {
	if len(q.enqueued) == 0 {
		return nil, "", nil
	}
	p := q.enqueued[0]
	q.enqueued = q.enqueued[1:]
	return p, "mock-ack", nil
}

func (q *mockQueue) Ack(_ context.Context, _ string, _ string) error { return nil }

// seedReadyFunction 构造函数 + ready 部署。
func seedReadyFunction(repo *mockRepo, projectID, functionID string, enabled bool, timeout int) *domainfunctions.Function {
	fn := &domainfunctions.Function{
		ID:             functionID,
		ProjectID:      projectID,
		Name:           "fn",
		Runtime:        "node-18.0",
		Entrypoint:     "index.main",
		TimeoutSeconds: timeout,
		Spec:           "shared-1x",
		Enabled:        enabled,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	_ = repo.CreateFunction(context.Background(), fn)
	dep := &domainfunctions.Deployment{
		ID:         "dep_ready",
		FunctionID: functionID,
		ProjectID:  projectID,
		Size:       100,
		Status:     domainfunctions.DeploymentStatusReady,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	_ = repo.CreateDeployment(context.Background(), dep)
	return fn
}
