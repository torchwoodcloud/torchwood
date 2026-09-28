#!/bin/sh
# docker-driver-e2e.sh — IMPL-T2-5 本地 dind docker 底座端到端（守卫②的
# docker 形态等价集成；编排形态沿 fleetly-dind.sh 先例，fleetlyd/swarm/令牌
# 三段不再需要——docker 底座直接对 dind 的 docker.sock 操作）。
#
# 形态：
#   宿主交叉编译 dockerdriver 的 e2e 测试二进制 → 单特权 dind → dind 内构建
#   driver 镜像（alpine + e2e.test）→ `docker run` 把编排器作为容器跑起来
#   （挂载 dind 的 docker.sock + 自 attach 项目网络）→ 收集容器日志断言
#   E2E PASS。
#
# 用法（Git Bash，torchwood 仓库根目录）：
#   sh dispatcher/testdata/e2e/docker-driver-e2e.sh
#
# 前置：本机 Docker 29+（dind 权限）；出网（dind 拉 alpine 基础镜像）。
# 产物落 dispatcher/testdata/e2e/artifacts/<run-id>/（gitignore 覆盖）。
set -u
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

SELF=$(printf '%s' "$0" | tr '\\' '/')
ROOT=$(CDPATH= cd -- "$(dirname -- "$SELF")/../../.." && pwd)
RUN_ID=$(date -u +%Y%m%d-%H%M%S)
RUN_DIR="$ROOT/dispatcher/testdata/e2e/artifacts/docker-$RUN_ID"
DIND=tw-e2e-dind-docker
DIND_IMAGE='docker:29.8.1-dind@sha256:3f3c01aaaebf7cce837356b688b7c059a4749f10bd7660dec7c58fc454a283f0'
DRIVER_IMAGE='tw-e2e/driver:1'

say() { printf '%s\n' "$*" | tee -a "$RUN_DIR/summary.txt"; }
die() { say "FATAL: $*"; exit 1; }

mkdir -p "$RUN_DIR" || exit 1
say "run dir: $RUN_DIR"

command -v docker >/dev/null 2>&1 || die 'docker not found'

TMP=$(mktemp -d) || die 'mktemp'
case "$TMP" in
/*)
    if command -v cygpath >/dev/null 2>&1; then
        TMP=$(cygpath -m "$TMP") || die 'cygpath -m'
    fi
    ;;
esac

cleanup() {
    docker rm -f "$DIND" >/dev/null 2>&1 || true
    rm -rf "$TMP" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

say '=== build torchwood dockerdriver e2e test binary (linux/amd64) ==='
(
    cd "$ROOT" || exit 1
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o "$TMP/e2e.test" ./dispatcher/dockerdriver
) >>"$RUN_DIR/summary.txt" 2>&1 || die 'torchwood e2e test build failed'

say '=== start dind ==='
docker rm -f "$DIND" >/dev/null 2>&1 || true
docker run -d --name "$DIND" --privileged "$DIND_IMAGE" >>"$RUN_DIR/summary.txt" 2>&1 ||
    die 'start dind failed'
i=0
while :; do
    if docker exec "$DIND" docker info >/dev/null 2>&1; then break; fi
    i=$((i + 2))
    [ "$i" -ge 90 ] && die 'dind docker daemon not ready within 90s'
    sleep 2
done
say 'dind docker daemon ready'

say '=== stage files ==='
docker exec "$DIND" mkdir -p /opt/e2e || die 'mkdir stage'
stage() { # <local> <remote>
    docker exec -i "$DIND" sh -c "cat > '$2'" <"$1" || die "stage $1 -> $2"
}
stage "$TMP/e2e.test" /opt/e2e/e2e.test
stage "$ROOT/dispatcher/testdata/e2e/Dockerfile.driver-docker" /opt/e2e/Dockerfile
docker exec "$DIND" chmod +x /opt/e2e/e2e.test || die 'chmod stage'

say '=== build driver image (local to dind; same image doubles as the function runner) ==='
docker exec "$DIND" sh -c "cd /opt/e2e && docker build -q -t '$DRIVER_IMAGE' -f Dockerfile ." >>"$RUN_DIR/summary.txt" 2>&1 ||
    die 'driver image build failed'
docker exec "$DIND" docker image inspect "$DRIVER_IMAGE" >/dev/null 2>&1 || die 'driver image missing'
say "driver image ready: $DRIVER_IMAGE"

say '=== run e2e orchestrator as a container (docker.sock mounted) ==='
# 编排器容器：挂载 dind 的 docker.sock（docker 底座唯一特权面）；网络/
# 容器操作全部经 sock——编排器自 attach tw-func-e2e 后按容器 IP 分发。
ORCH_LOG=$(docker exec "$DIND" docker run --rm \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -e TW_E2E_DOCKER=1 \
    -e TW_E2E_FUNCTION_IMAGE="$DRIVER_IMAGE" \
    --name tw-e2e-orchestrator \
    --entrypoint /opt/e2e/e2e.test \
    "$DRIVER_IMAGE" -test.run=TestE2EDockerDriverLifecycle -test.v 2>&1)
printf '%s\n' "$ORCH_LOG" | tee "$RUN_DIR/orchestrator.log" | tail -n 40 | tee -a "$RUN_DIR/summary.txt"

if printf '%s' "$ORCH_LOG" | grep -q 'E2E PASS'; then
    say '=== E2E DONE ==='
    exit 0
fi
say '=== FAIL: docker driver e2e failed ==='
exit 1
