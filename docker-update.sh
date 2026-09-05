#!/usr/bin/env bash
# ============================================================================
#  IPv6 代理池 Docker 在线更新脚本
# ----------------------------------------------------------------------------
#  从 Git 仓库拉取最新代码，重新构建镜像并滚动重启容器。
#  配置文件（config.json）与数据目录在更新过程中保持不变，客户端租约由
#  服务自身的持久化机制恢复；更新期间代理会短暂中断，请选择合适的时机执行。
#
#  用法：
#    ./docker-update.sh            拉取最新代码并更新容器
#    ./docker-update.sh --skip-pull 跳过 git pull，仅用当前代码重建容器
# ============================================================================
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_URL="${IPV6_PROXY_POOL_REPO:-https://github.com/yhw5231/ipv6-proxy-pool.git}"
COMPOSE_FILE="${SCRIPT_DIR}/docker-compose.yml"

cd "$SCRIPT_DIR"

info() { printf '\033[32m%s\033[0m\n' "$*"; }
warn() { printf '\033[33m%s\033[0m\n' "$*"; }
error() { printf '\033[31m%s\033[0m\n' "$*" >&2; }

# ---------------------------------------------------------------------------
# 前置检查
# ---------------------------------------------------------------------------
command -v docker >/dev/null 2>&1 || { error "未找到 docker，请先安装 Docker Engine。"; exit 1; }
if docker compose version >/dev/null 2>&1; then
  COMPOSE="docker compose"
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE="docker-compose"
else
  error "未找到 Docker Compose（docker compose 或 docker-compose 均不可用）。"
  exit 1
fi

SKIP_PULL=0
for arg in "$@"; do
  case "$arg" in
    --skip-pull) SKIP_PULL=1 ;;
    -h|--help) sed -n '2,14p' "$0"; exit 0 ;;
    *) error "未知参数: $arg"; exit 1 ;;
  esac
done

# ---------------------------------------------------------------------------
# 拉取最新代码
# ---------------------------------------------------------------------------
if [ "$SKIP_PULL" -eq 0 ]; then
  if [ -d "$SCRIPT_DIR/.git" ]; then
    if ! command -v git >/dev/null 2>&1; then
      error "未找到 git，无法在线拉取代码。请先安装 git，或使用 --skip-pull 跳过。"
      exit 1
    fi
    # remote 缺失或指向其他仓库时，补/改 remote
    if ! git -C "$SCRIPT_DIR" remote get-url origin >/dev/null 2>&1; then
      info "添加远程仓库 origin: $REPO_URL"
      git -C "$SCRIPT_DIR" remote add origin "$REPO_URL"
    fi
    BRANCH=$(git -C "$SCRIPT_DIR" rev-parse --abbrev-ref HEAD)
    info "拉取最新代码（分支 ${BRANCH}）..."
    if ! git -C "$SCRIPT_DIR" pull --ff-only origin "$BRANCH"; then
      error "git pull 失败（本地有改动或历史分叉）。"
      echo "  - 查看本地改动: git status"
      echo "  - 丢弃本地改动: git checkout -- . && git clean -fd"
      echo "  - 或改用: git reset --hard origin/${BRANCH}（会丢弃所有本地修改）"
      exit 1
    fi
  else
    warn "目录不是 Git 仓库，跳过拉取，直接使用当前代码构建。"
  fi
fi

# ---------------------------------------------------------------------------
# 重建镜像并滚动重启（配置文件不改动）
# ---------------------------------------------------------------------------
info "重建镜像并更新容器..."
$COMPOSE -f "$COMPOSE_FILE" up -d --build

info "清理悬空镜像..."
docker image prune -f >/dev/null 2>&1 || true

sleep 1
$COMPOSE -f "$COMPOSE_FILE" ps || true

# 等待管理接口就绪（admin.listen_address 默认端口 10070，host 网络直接本机探测）
ADMIN_PORT=${IPV6_PROXY_POOL_ADMIN_PORT:-10070}
if command -v curl >/dev/null 2>&1; then
  ok=0
  for _ in $(seq 1 15); do
    if curl -fsS --max-time 2 "http://127.0.0.1:${ADMIN_PORT}/healthz" >/dev/null 2>&1; then
      info "健康检查通过: /healthz OK"
      ok=1
      break
    fi
    sleep 1
  done
  [ "$ok" -eq 1 ] || warn "健康检查未通过，请查看: $COMPOSE -f $COMPOSE_FILE logs --tail 50"
fi

info "在线更新完成。Web 管理界面: http://127.0.0.1:${ADMIN_PORT}/"
