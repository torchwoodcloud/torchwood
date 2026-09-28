#!/bin/sh
# fleetly-dind.sh — IMPL-T2-3 本地 dind fleetly 端到端（守卫①的本地等价集成）。
#
# 形态（沿 fleetly 仓 spike/t20/s4 的 dind 编排先例）：
#   宿主交叉编译 fleetlyd+fleetly（FLEETLY_REPO）与 torchwood dispatcher 的
#   e2e 测试二进制 → 单特权 dind → swarm init → fleetlyd 起服 → 宿主注册
#   founder + 铸机具令牌（scope tasks,build，经 127.0.0.1:18420 发布端口）→
#   dind 内构建 driver 镜像 → `fleetly tasks network ensure` + `tasks run` 把
#   e2e 编排器作为任务跑在任务网络内 → 收集任务日志断言 E2E PASS。
#
# 用法（Git Bash，torchwood 仓库根目录）：
#   FLEETLY_REPO=/d/Codes/qiulin/fleetly sh dispatcher/testdata/e2e/fleetly-dind.sh
#
# 前置：本机 Docker 29+（dind 权限）；出网（dind 拉 alpine 基础镜像）；
# fleetly 仓可交叉编译。产物落 dispatcher/testdata/e2e/artifacts/<run-id>/
# （gitignore 覆盖）。令牌只进本 run 目录的 token.txt 与任务 env，不进日志。
set -u
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

SELF=$(printf '%s' "$0" | tr '\\' '/')
ROOT=$(CDPATH= cd -- "$(dirname -- "$SELF")/../../.." && pwd)
FLEETLY_REPO=${FLEETLY_REPO:-$(CDPATH= cd -- "$ROOT/../fleetly" && pwd)}
RUN_ID=$(date -u +%Y%m%d-%H%M%S)
RUN_DIR="$ROOT/dispatcher/testdata/e2e/artifacts/$RUN_ID"
DIND=tw-e2e-dind
DIND_IMAGE='docker:29.8.1-dind@sha256:3f3c01aaaebf7cce837356b688b7c059a4749f10bd7660dec7c58fc454a283f0'
HOST_REST_PORT=18420
E2E_VERSION=v0.0.0-t23e2e

say() { printf '%s\n' "$*" | tee -a "$RUN_DIR/summary.txt"; }
die() { say "FATAL: $*"; exit 1; }

mkdir -p "$RUN_DIR" || exit 1
say "run dir: $RUN_DIR"
say "fleetly repo: $FLEETLY_REPO"

command -v docker >/dev/null 2>&1 || die 'docker not found'
command -v curl >/dev/null 2>&1 || die 'curl not found (needed for founder register + token mint on the published port)'
[ -d "$FLEETLY_REPO/cmd/fleetlyd" ] || die "FLEETLY_REPO does not look like the fleetly repo: $FLEETLY_REPO"

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

say '=== build fleetlyd + fleetly (linux/amd64) ==='
(
    cd "$FLEETLY_REPO" || exit 1
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$E2E_VERSION" -o "$TMP/fleetlyd" ./cmd/fleetlyd &&
        GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$E2E_VERSION" -o "$TMP/fleetly" ./cmd/fleetly
) >>"$RUN_DIR/summary.txt" 2>&1 || die 'fleetly go build failed'

say '=== build torchwood dispatcher e2e test binary (linux/amd64) ==='
(
    cd "$ROOT" || exit 1
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o "$TMP/e2e.test" ./dispatcher
) >>"$RUN_DIR/summary.txt" 2>&1 || die 'torchwood e2e test build failed'

say '=== start dind ==='
docker rm -f "$DIND" >/dev/null 2>&1 || true
docker run -d --name "$DIND" --privileged -p "127.0.0.1:$HOST_REST_PORT:8420" "$DIND_IMAGE" >>"$RUN_DIR/summary.txt" 2>&1 ||
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
stage "$TMP/fleetlyd" /opt/e2e/fleetlyd
stage "$TMP/fleetly" /opt/e2e/fleetly
stage "$TMP/e2e.test" /opt/e2e/e2e.test
stage "$ROOT/dispatcher/testdata/e2e/in-e2e-boot.sh" /opt/e2e/in-e2e-boot.sh
stage "$ROOT/dispatcher/testdata/e2e/in-e2e-run.sh" /opt/e2e/in-e2e-run.sh
stage "$ROOT/dispatcher/testdata/e2e/Dockerfile.driver" /opt/e2e/Dockerfile
docker exec "$DIND" chmod +x /opt/e2e/fleetlyd /opt/e2e/fleetly /opt/e2e/in-e2e-boot.sh /opt/e2e/in-e2e-run.sh /opt/e2e/e2e.test ||
    die 'chmod stage'

say '=== boot fleetlyd + swarm init ==='
docker exec "$DIND" sh /opt/e2e/in-e2e-boot.sh >"$RUN_DIR/boot.log" 2>&1 || {
    tail -n 30 "$RUN_DIR/boot.log" | tee -a "$RUN_DIR/summary.txt"
    die 'boot script failed'
}

say '=== register founder + mint machine token (host -> published REST port) ==='
# --noproxy '*'：宿主环境常带 HTTP_PROXY/HTTPS_PROXY（本机事实），发布端口在
# 回环，代理会截断——显式绕过。cookie jar 的 -c 参数是原生程序路径参数，
# 在 MSYS_NO_PATHCONV=1 下须经 cygpath 转 Windows 形态（响应体走 shell 重定向，
# 不经过参数转换）。
JAR="$RUN_DIR/cookies.txt"
JAR_WIN=$(cygpath -w "$JAR") || die 'cygpath -w cookies'
curl -s --noproxy '*' -c "$JAR_WIN" -X POST "http://127.0.0.1:$HOST_REST_PORT/v1/auth/register" \
    -H 'Content-Type: application/json' \
    -d '{"email":"founder@t23e2e.test","password":"t23-e2e-founder-pass","display_name":"T23 E2E Founder"}' \
    >"$RUN_DIR/register.json" || die 'founder register failed'
TOKEN=$(curl -s --noproxy '*' -b "$JAR_WIN" -X POST "http://127.0.0.1:$HOST_REST_PORT/v1/tokens" \
    -H 'Content-Type: application/json' \
    -d '{"machine":true,"note":"t23 e2e machine token","scopes":["tasks","build","read"]}' |
    grep -oE '"token": ?"[^"]*"' | head -1 | cut -d'"' -f4)
[ -n "$TOKEN" ] || die 'machine token mint failed'
printf '%s' "$TOKEN" >"$RUN_DIR/token.txt"
say 'founder registered; machine token minted (stored in run dir, not echoed)'

say '=== stage token + run inner e2e ==='
docker exec -i "$DIND" sh -c 'cat > /tmp/token.txt' <"$RUN_DIR/token.txt" || die 'stage token failed'
docker exec "$DIND" sh /opt/e2e/in-e2e-run.sh >"$RUN_DIR/inner.log" 2>&1
INNER_RC=$?
say "inner rc=$INNER_RC (log: $RUN_DIR/inner.log)"
tail -n 60 "$RUN_DIR/inner.log" | tee -a "$RUN_DIR/summary.txt"

# 原始产物收集（best-effort；dind 随后被清理）。
docker exec "$DIND" cat /tmp/e2e-orchestrator.log >"$RUN_DIR/orchestrator.log" 2>&1 || true
docker exec "$DIND" cat /tmp/fleetlyd.log >"$RUN_DIR/fleetlyd.log" 2>&1 || true

if [ "$INNER_RC" -ne 0 ]; then
    say '=== FAIL: inner e2e failed ==='
    exit 1
fi
say '=== E2E DONE ==='
