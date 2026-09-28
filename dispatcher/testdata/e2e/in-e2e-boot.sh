#!/bin/sh
# in-e2e-boot.sh — dind 内第一阶段：swarm init + fleetlyd 起服（0.0.0.0 REST/gRPC）。
# 由宿主 fleetly-dind.sh 暂存后执行；令牌不在此脚本内出现。
set -u

ALPINE='alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc'

echo '=== pre-pull base image ==='
docker pull -q "$ALPINE" || exit 1

echo '=== swarm init ==='
docker swarm init --advertise-addr eth0 || exit 1

echo '=== fleetlyd config ==='
mkdir -p /var/lib/fleetly
cat >/opt/e2e/config.yaml <<'EOF'
addr: "0.0.0.0:8420"
grpc:
  addr: "0.0.0.0:8421"
state:
  db_path: "/var/lib/fleetly/fleetly.db"
secrets:
  key_path: "/var/lib/fleetly/fleetly.key"
build:
  cache_dir: "/var/lib/fleetly/build-cache"
  artifacts_dir: "/var/lib/fleetly/build-artifacts"
logs:
  dir: "/var/lib/fleetly/fleetly-logs"
ingress:
  token_file: "/var/lib/fleetly/fleetly-ingress.token"
  cert_dir: "/var/lib/fleetly/fleetly-certs"
  acme:
    enabled: false
engine:
  deploy_timeout_seconds: 60
  observe_seconds: 5
  replicas_below_seconds: 5
  poll_seconds: 1
  drift_interval_seconds: 3600
git:
  enabled: false
logging:
  level: info
EOF

echo '=== boot fleetlyd ==='
cd /var/lib/fleetly || exit 1
nohup /opt/e2e/fleetlyd -c /opt/e2e/config.yaml >/tmp/fleetlyd.log 2>&1 &
echo $! >/tmp/fleetlyd.pid

i=0
while :; do
    if wget -q -T 3 -O /dev/null http://127.0.0.1:8420/healthz/liveness; then break; fi
    i=$((i + 2))
    [ "$i" -ge 90 ] && {
        echo 'fleetlyd not live within 90s; log tail:'
        tail -n 40 /tmp/fleetlyd.log
        exit 1
    }
    sleep 2
done
echo 'fleetlyd live (liveness 200)'
