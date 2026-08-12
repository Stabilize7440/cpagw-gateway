#!/usr/bin/env bash
# 编译 cpagw-gateway 插件（.so）
# 注意：CPA 容器是 glibc（Debian），必须用 bookworm 镜像编译，不能用 alpine(musl)
set -euo pipefail
cd "$(dirname "$0")"

docker run --rm -v "$(pwd):/src" -w /src golang:1.24-bookworm \
  sh -c "apt-get update >/dev/null 2>&1 && apt-get install -y gcc >/dev/null 2>&1 && \
         CGO_ENABLED=1 go build -buildmode=c-shared -o cpagw-gateway.so ."

echo "OK: cpagw-gateway.so"
ls -la cpagw-gateway.so
