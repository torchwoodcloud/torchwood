#!/bin/sh
# in-e2e-run.sh — dind 内第二阶段：构建 driver 镜像 → task-group 网络 ensure →
# 以任务形态运行 e2e 编排器（dispatcher 真实代码路径）→ 收集任务日志。
# 前置：in-e2e-boot.sh 已跑（swarm active + fleetlyd live）；/tmp/token.txt
# 由宿主暂存（机具令牌，scope tasks,build；不 echo、不进镜像构建上下文）。
set -u

FCLI='/opt/e2e/fleetly'
ADDR='127.0.0.1:8421'
REF='pe2e'
DRIVER_IMAGE='tw-e2e/driver:1'
ALPINE='alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc'

TOKEN=$(cat /tmp/token.txt)
[ -n "$TOKEN" ] || {
    echo 'missing token'
    exit 1
}

# dind 宿主网关（容器出网/回访宿主 fleetlyd 的地址）。
GW=$(docker network inspect docker_gwbridge -f '{{(index .IPAM.Config 0).Gateway}}')
[ -n "$GW" ] || {
    echo 'cannot resolve docker_gwbridge gateway'
    exit 1
}
echo "container->host gateway: $GW"

echo '=== build driver image (local to dind; platform resolves via local inspect) ==='
cd /opt/e2e || exit 1
docker build -q -t "$DRIVER_IMAGE" -f /opt/e2e/Dockerfile . || exit 1
docker image inspect "$DRIVER_IMAGE" >/dev/null || exit 1

echo '=== ensure task-group network ==='
$FCLI tasks network ensure --addr "$ADDR" --token "$TOKEN" "$REF" || exit 1

echo '=== run e2e orchestrator as a task on the task-group network ==='
RUN_JSON=$(mktemp)
$FCLI tasks run --addr "$ADDR" --token "$TOKEN" \
    --image "$DRIVER_IMAGE" \
    --scope-kind task-group --scope-ref "$REF" \
    --name t23-e2e-orchestrator --ttl 20m \
    --command /opt/e2e/e2e.test \
    --arg -test.run=TestE2EFleetlyTaskLifecycle \
    --arg -test.v \
    --env "TW_E2E_FLEETLY_ENDPOINT=$GW:8421" \
    --env "TW_E2E_FLEETLY_TOKEN=$TOKEN" \
    --env "TW_E2E_FUNCTION_IMAGE=$DRIVER_IMAGE" \
    --env "TW_E2E_TASK_GROUP_REF=$REF" \
    --json >"$RUN_JSON" || {
    echo 'tasks run failed'
    cat "$RUN_JSON"
    exit 1
}
cat "$RUN_JSON"
TASK_ID=$(grep -oE '"id": ?"[^"]*"' "$RUN_JSON" | head -1 | cut -d'"' -f4)
[ -n "$TASK_ID" ] || {
    echo 'cannot parse task id'
    exit 1
}
SERVICE="fleetly-task-$TASK_ID"
echo "orchestrator task: $TASK_ID (service $SERVICE)"

echo '=== wait for orchestrator terminal state (<=10m) ==='
# API 台账为真值：引擎在任务容器完成后即移除底座服务（stopping→stopped 落账），
# docker service ps/logs 不再可依赖——状态经 tasks ls，日志经 tasks logs。
i=0
while :; do
    LINE=$($FCLI tasks ls --all --addr "$ADDR" --token "$TOKEN" --limit 20 |
        grep "^task $TASK_ID " | head -1)
    STATUS=$(printf '%s' "$LINE" | grep -oE 'status=[a-z]+' | head -1 | cut -d= -f2)
    echo "t=$i status=${STATUS:-unknown}"
    case "$STATUS" in
    stopped | failed) break ;;
    esac
    i=$((i + 5))
    [ "$i" -ge 600 ] && {
        echo 'orchestrator did not reach terminal state in 10m'
        break
    }
    sleep 5
done

echo '=== orchestrator logs (platform VictoriaLogs; task-labelled stream) ==='
# VL 检索在冷启动 dind 上可能短暂 degraded（后端未应答即返回
# E_LOGS_BACKEND_UNAVAILABLE，检索会自动恢复）——有界重试取日志。
i=0
while :; do
    $FCLI tasks logs --addr "$ADDR" --token "$TOKEN" --limit 200 "$TASK_ID" > /tmp/e2e-orchestrator.log 2>&1
    if [ -s /tmp/e2e-orchestrator.log ] &&
        ! grep -q 'E_LOGS_BACKEND_UNAVAILABLE' /tmp/e2e-orchestrator.log; then
        break
    fi
    i=$((i + 5))
    [ "$i" -ge 180 ] && break
    sleep 5
done
if [ ! -s /tmp/e2e-orchestrator.log ]; then
    # 平台日志库缺失时的兜底腿（老引擎服务仍在场的形态）。
    docker service logs "$SERVICE" > /tmp/e2e-orchestrator.log 2>&1 || true
fi
cat /tmp/e2e-orchestrator.log

if grep -q 'E2E PASS' /tmp/e2e-orchestrator.log; then
    echo 'E2E-RESULT: PASS'
    exit 0
fi
echo 'E2E-RESULT: FAIL'
echo '--- fleetlyd log tail ---'
tail -n 80 /tmp/fleetlyd.log
exit 1
