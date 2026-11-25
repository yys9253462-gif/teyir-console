#!/usr/bin/env bash
# 部署入口（服务器根目录下的 ./deploy.sh）。
#
# 这里故意不复制 update.sh 的内容：一旦复制，两份脚本会随着仓库更新而分叉，
# 服务器上那份会悄悄变旧。转发到 deploy/update.sh，逻辑永远只有一份。
set -euo pipefail
exec "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/deploy/update.sh" "$@"
