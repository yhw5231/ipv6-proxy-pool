#!/usr/bin/env bash
# ============================================================================
#  IPv6 代理池 管理菜单 / SSH 控制脚本
# ----------------------------------------------------------------------------
#  适用于拥有 tunnelbroker.net 分配的 IPv6 /64 网段的 Linux 主机。
#  交互菜单：直接运行 ./control.sh
#  单条指令：./control.sh <命令>  例如 ./control.sh install / start / stop /
#            restart / status / logs / uninstall / client-new <名称> /
#            client-rotate <名称> / client-delete <名称> / list / help
# ============================================================================

set -u

# ---------------------------------------------------------------------------
# 配置常量
# ---------------------------------------------------------------------------
SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
INSTALL_DIR="/opt/ipv6-proxy-pool"
SERVICE_NAME="ipv6-proxy-pool"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
CONFIG_FILE="${INSTALL_DIR}/config.json"
BINARY_NAME="ipv6-proxy-pool"

# 默认参数（安装时可修改）
MIN_LEASES=1024                   # 常驻备用租约数（池子始终保有的待分配代理数）
MAX_LEASES=2048                   # 常驻备用 + 客户端租约的总上限
PORT_START=20000
PORT_END=22047
SOCKS_BASE_LISTEN="[::]:10080"    # 仅用于提取绑定主机，实际端口按租约动态分配
ADMIN_LISTEN="[::]:10070"
IDLE_TIMEOUT_MIN=60               # 客户端空闲多长时间后自动回收（分钟，仅当总数超过常驻数时生效）

# 颜色
C_RESET=$'\033[0m'
C_BOLD=$'\033[1m'
C_GREEN=$'\033[32m'
C_YELLOW=$'\033[33m'
C_RED=$'\033[31m'
C_CYAN=$'\033[36m'
[ -t 1 ] || { C_RESET=""; C_BOLD=""; C_GREEN=""; C_YELLOW=""; C_RED=""; C_CYAN=""; }

info()  { printf '%s%s%s\n' "$C_GREEN" "$*" "$C_RESET"; }
warn()  { printf '%s%s%s\n' "$C_YELLOW" "$*" "$C_RESET"; }
error() { printf '%s%s%s\n' "$C_RED" "$*" "$C_RESET" >&2; }

usage() {
  cat <<EOF
用法: $(basename "$0") [命令]

不带参数进入交互式菜单；也可直接使用以下命令：

  install          一键安装并启动（检测 IPv6 /64、安装 systemd 服务）
  start            启动服务
  stop             停止服务
  restart          重启服务
  status           查看服务状态
  info             显示客户端连接信息（管理端 URL、Token、SOCKS5 等）
  logs [行数]      查看实时日志（默认 100 行）
  config           编辑配置文件（编辑后需 restart）
  env              检查 IPv6 网络环境
  release-idle     立即释放所有空闲租约
  list             列出当前所有客户端租约
  client-new <名称>        为客户端创建代理（分配端口 + IPv6）
  client-rotate <名称>     为客户端更换 IPv6（端口不变）
  client-recycle <名称>    释放并立即重新获取（新端口 + 新 IPv6）
  client-delete <名称>     释放并销毁客户端代理
  uninstall        停止并卸载（配置自动备份到当前用户主目录）
  update           在线更新：拉取仓库最新代码，重建二进制并重启服务
  help             显示本帮助
EOF
}

# ---------------------------------------------------------------------------
# 工具函数
# ---------------------------------------------------------------------------
need_root() {
  if [ "$(id -u)" -ne 0 ]; then
    error "该操作需要 root 权限，请使用 sudo 运行。"
    exit 1
  fi
}

service_is_active() {
  systemctl is-active --quiet "${SERVICE_NAME}.service" 2>/dev/null
}

resolve_binary() {
  if [ -x "${INSTALL_DIR}/${BINARY_NAME}" ]; then
    printf '%s' "${INSTALL_DIR}/${BINARY_NAME}"
    return 0
  fi
  if [ -x "${SCRIPT_DIR}/bin/${BINARY_NAME}" ]; then
    printf '%s' "${SCRIPT_DIR}/bin/${BINARY_NAME}"
    return 0
  fi
  # sudo 的 secure_path 会重置 PATH，补上手动安装的 Go 常见位置
  export PATH="$PATH:/usr/local/go/bin:/usr/lib/go/bin"
  if command -v go >/dev/null 2>&1; then
    info "使用 Go: $(command -v go) ($({ go version; } 2>/dev/null || echo '未知版本'))"
    if (cd "$SCRIPT_DIR" && go build -trimpath -ldflags="-s -w" -o "bin/${BINARY_NAME}" "./cmd/${BINARY_NAME}"); then
      printf '%s' "${SCRIPT_DIR}/bin/${BINARY_NAME}"
      return 0
    fi
    error "go build 失败，请查看上方错误（最常见原因：Go 版本低于 go.mod 声明的版本）。"
    return 1
  fi
  return 1
}

# 强制用当前源码构建二进制（update 专用；resolve_binary 会优先复用已安装的旧版本）
build_fresh_binary() {
  export PATH="$PATH:/usr/local/go/bin:/usr/lib/go/bin"
  if command -v go >/dev/null 2>&1; then
    info "使用 Go: $(command -v go) ($({ go version; } 2>/dev/null || echo '未知版本'))"
    if (cd "$SCRIPT_DIR" && go build -trimpath -ldflags="-s -w" -o "bin/${BINARY_NAME}" "./cmd/${BINARY_NAME}"); then
      printf '%s' "${SCRIPT_DIR}/bin/${BINARY_NAME}"
      return 0
    fi
    error "go build 失败，请查看上方错误。"
    return 1
  fi
  if [ -x "${SCRIPT_DIR}/bin/${BINARY_NAME}" ]; then
    warn "未检测到 Go，使用 ${SCRIPT_DIR}/bin/ 中现有的二进制（可能不是最新代码构建的）。"
    printf '%s' "${SCRIPT_DIR}/bin/${BINARY_NAME}"
    return 0
  fi
  return 1
}

# 从 /64 之前的 IPv6 地址推导 /64 前缀
detect_prefix() {
  local raw=""
  raw=$(ip -6 addr show scope global 2>/dev/null | awk '/inet6 / && $2 !~ /^fe80/ {print $2; exit}')
  if [ -z "$raw" ]; then
    raw=$(ip -6 addr show 2>/dev/null | awk '/inet6 / && $2 !~ /^fe80/ && $2 !~ /^::1/ {print $2; exit}')
  fi
  [ -z "$raw" ] && return 1
  raw=${raw%%/*}
  if command -v python3 >/dev/null 2>&1; then
    local computed=""
    computed=$(python3 -c "
import ipaddress, sys
try:
    a = ipaddress.ip_address('${raw}')
    print(ipaddress.ip_network(str(a) + '/64', strict=False))
except Exception:
    sys.exit(1)
" 2>/dev/null)
    [ -n "$computed" ] && { printf '%s' "$computed"; return 0; }
  fi
  # bash 兜底：取前 4 个 hextet 并补齐
  local IFS=':' parts part hextets=()
  IFS=':' read -ra parts <<< "$raw"
  for part in "${parts[@]}"; do
    [ -n "$part" ] && hextets+=("$part")
  done
  while [ "${#hextets[@]}" -lt 4 ]; do hextets+=("0"); done
  printf '%s:%s:%s:%s::/64' "${hextets[0]}" "${hextets[1]}" "${hextets[2]}" "${hextets[3]}"
}

validate_prefix() {
  local prefix=$1
  if command -v python3 >/dev/null 2>&1; then
    python3 -c "
import ipaddress, sys
try:
    ipaddress.ip_network(sys.argv[1], strict=False)
except Exception:
    sys.exit(1)
" "$prefix" 2>/dev/null || { error "IPv6 前缀格式无效: $prefix"; return 1; }
  fi
  return 0
}

gen_token() {
  if [ -r /dev/urandom ]; then
    od -An -N16 -tx1 /dev/urandom | tr -d ' \n' | cut -c1-32
  else
    printf 'ipv6pxy%s' "$(date +%s%N)"
  fi
}

# 从配置文件读取用于客户端命令的管理地址和令牌
admin_url() {
  local addr="" port url
  if command -v python3 >/dev/null 2>&1 && [ -f "$CONFIG_FILE" ]; then
    addr=$(python3 -c "
import json, sys
try:
    print(json.load(open('${CONFIG_FILE}')).get('admin', {}).get('listen_address', ''))
except Exception:
    pass
" 2>/dev/null)
  fi
  [ -z "$addr" ] && addr="127.0.0.1:10070"
  port=${addr##*:}
  case "$port" in
    ''|*[!0-9]*) port=10070 ;;
  esac
  printf 'http://127.0.0.1:%s' "$port"
}

read_token() {
  local token=""
  if command -v python3 >/dev/null 2>&1 && [ -f "$CONFIG_FILE" ]; then
    token=$(python3 -c "
import json, sys
try:
    print(json.load(open('${CONFIG_FILE}')).get('admin', {}).get('token', ''))
except Exception:
    pass
" 2>/dev/null)
  fi
  if [ -z "$token" ]; then
    token=$(grep -o '"token"[[:space:]]*:[[:space:]]*"[^"]*"' "$CONFIG_FILE" 2>/dev/null | head -n1 | cut -d'"' -f4)
  fi
  printf '%s' "$token"
}

# 检测服务器公网 IPv4（出方向探测失败时回退到本机路由源地址）
detect_public_ip() {
  local ip=""
  if command -v curl >/dev/null 2>&1; then
    ip=$(curl -4 -s --max-time 3 https://api.ipify.org 2>/dev/null)
    [ -z "$ip" ] && ip=$(curl -4 -s --max-time 3 https://ifconfig.me 2>/dev/null)
  fi
  case "$ip" in
    ''|*[!0-9.]*) ip="" ;;
  esac
  if [ -z "$ip" ]; then
    ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')
  fi
  printf '%s' "$ip"
}

# 一次性读出展示连接信息所需的运行配置（R_ 前缀变量）
load_runtime_info() {
  R_PREFIX=""; R_MODE=""; R_TOKEN=""; R_ADMIN_ADDR=""
  R_SOCKS_ADDR=""; R_PORT_START=""; R_PORT_END=""
  R_ROTATE_REQUESTS=0; R_ROTATE_AFTER_NS=0
  if command -v python3 >/dev/null 2>&1 && [ -f "$CONFIG_FILE" ]; then
    eval "$(python3 - "$CONFIG_FILE" <<'PY'
import json, sys
try:
    cfg = json.load(open(sys.argv[1]))
except Exception:
    cfg = {}
def q(value):
    return str(value).replace("'", "'\\''")
socks = cfg.get('socks') or {}
admin = cfg.get('admin') or {}
print("R_PREFIX='%s'" % q(cfg.get('ipv6_prefix', '')))
print("R_MODE='%s'" % q(socks.get('mode', '')))
print("R_TOKEN='%s'" % q(admin.get('token', '')))
print("R_ADMIN_ADDR='%s'" % q(admin.get('listen_address', '')))
print("R_SOCKS_ADDR='%s'" % q(socks.get('listen_address', '')))
print("R_PORT_START='%s'" % q(socks.get('port_start', '')))
print("R_PORT_END='%s'" % q(socks.get('port_end', '')))
print("R_ROTATE_REQUESTS=%d" % int(cfg.get('rotate_requests') or 0))
print("R_ROTATE_AFTER_NS=%d" % int(cfg.get('rotate_after') or 0))
PY
)" 2>/dev/null || true
  else
    # 无 python3 时的 grep 兜底：按出现顺序提取 "key": value（socks 段在 admin 段之前）
    local line key value
    while IFS='|' read -r key value; do
      case "$key" in
        ipv6_prefix)    R_PREFIX=$value ;;
        mode)           R_MODE=$value ;;
        token)          R_TOKEN=$value ;;
        listen_address) if [ -z "$R_SOCKS_ADDR" ]; then R_SOCKS_ADDR=$value; elif [ -z "$R_ADMIN_ADDR" ]; then R_ADMIN_ADDR=$value; fi ;;
        port_start)     R_PORT_START=$value ;;
        port_end)       R_PORT_END=$value ;;
        rotate_requests) R_ROTATE_REQUESTS=$value ;;
        rotate_after)   R_ROTATE_AFTER_NS=$value ;;
      esac
    done <<< "$(grep -oE '"[a-z0-9_]+"[[:space:]]*:[[:space:]]*("[^"]*"|[0-9]+)' "$CONFIG_FILE" 2>/dev/null | sed -E 's/^"([a-z0-9_]+)"[[:space:]]*:[[:space:]]*/\1|/; s/"//g')"
  fi
  case "$R_ADMIN_ADDR" in *:*) ;; *) R_ADMIN_ADDR="127.0.0.1:10070" ;; esac
  case "$R_SOCKS_ADDR" in *:*) ;; *) R_SOCKS_ADDR="[::]:10080" ;; esac
  case "$R_PORT_START" in ''|*[!0-9]*) R_PORT_START=20000 ;; esac
  case "$R_PORT_END" in ''|*[!0-9]*) R_PORT_END=22047 ;; esac
}

# 列出现有租约 ID（供客户端填「租约 ID」参考）
list_lease_ids() {
  local url token ids=""
  url=$(admin_url)
  token=$(read_token)
  if command -v curl >/dev/null 2>&1; then
    local auth=()
    [ -n "$token" ] && auth=(-H "Authorization: Bearer ${token}")
    ids=$(curl -s --max-time 3 "${auth[@]}" "$url/v1/leases" 2>/dev/null | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | paste -sd ', ' -)
  fi
  printf '%s' "$ids"
}

# 展示客户端接入所需的完整连接信息（对应客户端配置表单字段）
show_connection_info() {
  if ! [ -f "$CONFIG_FILE" ]; then
    error "尚未安装（找不到 $CONFIG_FILE），请先执行: $0 install"
    return 1
  fi
  load_runtime_info
  local pub_ip admin_port rotate_min
  pub_ip=$(detect_public_ip)
  [ -z "$pub_ip" ] && pub_ip="<服务器公网IP>"
  admin_port=${R_ADMIN_ADDR##*:}
  case "$admin_port" in
    ''|*[!0-9]*) admin_port=10070 ;;
  esac
  rotate_min=$((R_ROTATE_AFTER_NS / 60000000000))

  echo
  echo "${C_BOLD}=== 客户端连接信息（填写到客户端配置） ===${C_RESET}"
  printf '  %-24s %s\n' "池管理端 URL" "http://${pub_ip}:${admin_port}"
  printf '  %-24s %s\n' "池 Token（可空）" "${R_TOKEN:-(未启用令牌，可留空)}"
  local lease_ids
  lease_ids=$(list_lease_ids)
  printf '  %-24s %s\n' "租约 ID（空=自动）" "${lease_ids:-(暂无租约；留空由客户端自动创建)}"
  local socks_host=${R_SOCKS_ADDR%:*}
  socks_host=${socks_host#[}
  socks_host=${socks_host%]}
  case "$socks_host" in
    '::'|'0.0.0.0'|'') socks_host="${pub_ip}" ;;
  esac
  printf '  %-24s %s\n' "SOCKS5 地址" "${socks_host}（客户端端口 ${R_PORT_START}-${R_PORT_END}，每个租约一个端口）"
  printf '  %-24s %s\n' "换IP状态码" "由客户端自行配置（如 403,429），代理池服务端不涉及"
  printf '  %-24s %s\n' "每 N 次请求换IP" "${R_ROTATE_REQUESTS}（0=关闭）"
  printf '  %-24s %s\n' "按时间换IP（分钟）" "${rotate_min}（0=关闭）"
  printf '  %-24s %s\n' "IPv6 前缀 / 模式" "${R_PREFIX} / ${R_MODE:-per_ipv6}"
  echo
  warn "SOCKS5 实际端口 = 租约分配的端口，用「./control.sh client-new <名称>」创建后查看。"
  echo "  远程客户端参考命令: ipv6-proxy-pool client create -name <名称> -admin $(admin_url) -token <令牌> -server ${pub_ip}"
}

run_client_cmd() {
  local binary=""
  binary=$(resolve_binary) || { error "找不到二进制，请先执行 install 或在有 Go 环境下构建。"; return 1; }
  local url token
  url=$(admin_url)
  token=$(read_token)
  if [ -n "$token" ]; then
    "$binary" client "$@" -admin "$url" -token "$token"
  else
    "$binary" client "$@" -admin "$url"
  fi
}

# ---------------------------------------------------------------------------
# 网络环境
# ---------------------------------------------------------------------------
check_env() {
  echo "=== IPv6 网络环境 ==="
  local prefix=""
  if prefix=$(detect_prefix); then
    info "检测到 IPv6 /64 前缀: $prefix"
  else
    warn "未在网卡上检测到全局 IPv6 地址。请先配置 tunnelbroker 隧道。"
  fi
  if ip -6 route show default >/dev/null 2>&1 && [ -n "$(ip -6 route show default 2>/dev/null)" ]; then
    info "IPv6 默认路由: $(ip -6 route show default | head -n1)"
  else
    warn "没有 IPv6 默认路由！"
  fi
  local probe=""
  if command -v ping >/dev/null 2>&1 && ping -6 -c1 -W2 2001:4860:4860::8888 >/dev/null 2>&1; then
    probe=ok
  elif command -v ping6 >/dev/null 2>&1 && ping6 -c1 -W2 2001:4860:4860::8888 >/dev/null 2>&1; then
    probe=ok
  elif command -v curl >/dev/null 2>&1 && curl -6 -s --max-time 3 -o /dev/null https://www.google.com >/dev/null 2>&1; then
    probe=ok
  fi
  if [ "$probe" = ok ]; then
    info "IPv6 公网连通: 正常"
  else
    warn "IPv6 公网连通: 无法确认（不影响服务启动，但出站代理需要公网 IPv6 路由）"
  fi
  local nonlocal
  nonlocal=$(cat /proc/sys/net/ipv6/ip_nonlocal_bind 2>/dev/null || echo "?")
  if [ "$nonlocal" = "1" ]; then
    info "net.ipv6.ip_nonlocal_bind = 1（允许绑定非本机 IPv6）"
  else
    warn "net.ipv6.ip_nonlocal_bind = $nonlocal（建议设为 1，否则代理无法绑定路由网段中的地址）"
  fi
}

ensure_nonlocal_bind() {
  if [ -w /proc/sys/net/ipv6/ip_nonlocal_bind ]; then
    echo 1 > /proc/sys/net/ipv6/ip_nonlocal_bind
  fi
  local sysctl_file=/etc/sysctl.d/99-ipv6-proxy-pool.conf
  if ! grep -qs 'ip_nonlocal_bind' "$sysctl_file" 2>/dev/null; then
    printf 'net.ipv6.ip_nonlocal_bind = 1\n' >> "$sysctl_file" && info "已持久化 net.ipv6.ip_nonlocal_bind = 1"
  fi
}

ensure_prefix_address() {
  local prefix=$1
  # "2001:470:xxxx:yyyy::/64" -> "2001:470:xxxx:yyyy"
  local subnet="${prefix%::*}"
  if [ -z "$subnet" ]; then
    subnet=${prefix%%/*}
  fi
  if ! ip -6 addr show 2>/dev/null | grep -qi "inet6 ${subnet}"; then
    echo
    warn "网卡上尚未配置该 /64 网段的地址。"
    echo "tunnelbroker 路由网段可以不配地址直接使用（需要 ip_nonlocal_bind），"
    echo "但建议在接口上添加一个地址以利于邻居发现。"
    read -rp "是否把 ${prefix}1/64 添加到默认路由接口？[y/N] " ans
    if [[ "$ans" =~ ^[Yy]$ ]]; then
      local dev=""
      dev=$(ip -6 route show default 2>/dev/null | awk '{print $5; exit}')
      [ -z "$dev" ] && dev=$(ip route show default 2>/dev/null | awk '{print $5; exit}')
      if [ -n "$dev" ]; then
        ip -6 addr add "${prefix}1/64" dev "$dev" && info "已添加 ${prefix}1/64 到 $dev"
      else
        warn "未找到默认路由接口，请手动配置。"
      fi
    fi
  else
    info "已在该网段内配置接口地址，无需额外添加。"
  fi
}

# ---------------------------------------------------------------------------
# 安装 / 启停 / 卸载
# ---------------------------------------------------------------------------
do_install() {
  need_root

  echo "=== 第 1 步：IPv6 前缀 ==="
  local prefix=""
  if prefix=$(detect_prefix); then
    info "自动检测到 /64 前缀: $prefix"
    read -rp "直接回车使用该前缀，或输入正确前缀: " answer
    [ -n "$answer" ] && prefix=$answer
  else
    warn "未能自动检测，请手动输入 tunnelbroker 分配的 /64 网段。"
    read -rp "请输入 IPv6 /64 前缀（例如 2001:470:xxxx:yyyy::/64）: " prefix
  fi
  if [ -z "$prefix" ] || ! validate_prefix "$prefix"; then
    error "未提供有效 IPv6 前缀，安装中止。"
    return 1
  fi

  echo
  echo "=== 第 2 步：构建/准备二进制 ==="
  mkdir -p "$INSTALL_DIR"
  local binary=""
  binary=$(resolve_binary) || {
    error "未找到可用的 Go 或现成二进制。请检查以下两项："
    error "  1) bin/${BINARY_NAME} 是否存在且有执行权限（ls -la ${SCRIPT_DIR}/bin/）"
    error "  2) sudo 环境下 go 是否可见（sudo command -v go）；手动安装的 Go 通常在 /usr/local/go/bin"
    error "也可在其他机器构建后把 ${BINARY_NAME} 放到 ${SCRIPT_DIR}/bin/，再重新执行 install。"
    return 1
  }
  cp "$binary" "$INSTALL_DIR/${BINARY_NAME}" && chmod 755 "$INSTALL_DIR/${BINARY_NAME}"
  info "二进制已就绪: $INSTALL_DIR/${BINARY_NAME}"

  echo
  echo "=== 第 3 步：生成配置 ==="
  echo "代理模式：per_ipv6（一个客户端 = 一个端口 + 一个 IPv6）"
  read -rp "常驻备用租约数（默认 $MIN_LEASES，池子始终保有的待分配代理）: " ans
  case "$ans" in
    ''|*[!0-9]*) ;;
    *) MIN_LEASES=$ans ;;
  esac
  read -rp "最大租约总数（默认 $MAX_LEASES，需不小于常驻数）: " ans
  case "$ans" in
    ''|*[!0-9]*) ;;
    *) MAX_LEASES=$ans ;;
  esac
  if [ "$MAX_LEASES" -lt "$MIN_LEASES" ]; then
    MAX_LEASES=$MIN_LEASES
    warn "最大租约数小于常驻数，已自动调整为 $MAX_LEASES。"
  fi
  PORT_END=$((PORT_START + MAX_LEASES - 1))
  read -rp "客户端空闲自动回收时间（分钟，默认 $IDLE_TIMEOUT_MIN，仅当总数超过常驻数时生效）: " ans
  case "$ans" in
    ''|*[!0-9]*) ;;
    *) IDLE_TIMEOUT_MIN=$ans ;;
  esac
  local token
  token=$(gen_token)
  cat > "$CONFIG_FILE" <<EOF
{
  "ipv6_prefix": "$prefix",
  "min_leases": $MIN_LEASES,
  "max_leases": $MAX_LEASES,
  "idle_timeout": $((IDLE_TIMEOUT_MIN * 60000000000)),
  "rotate_after": 0,
  "rotate_requests": 0,
  "socks": {
    "mode": "per_ipv6",
    "listen_address": "$SOCKS_BASE_LISTEN",
    "port_start": $PORT_START,
    "port_end": $PORT_END,
    "always_on_ports": []
  },
  "admin": {
    "listen_address": "$ADMIN_LISTEN",
    "token": "$token"
  }
}
EOF
  [ -d "$SCRIPT_DIR/web" ] && cp -r "$SCRIPT_DIR/web" "$INSTALL_DIR/"
  info "配置已写入: $CONFIG_FILE"

  echo
  echo "=== 第 4 步：IPv6 系统设置 ==="
  ensure_nonlocal_bind
  ensure_prefix_address "$prefix"

  echo
  echo "=== 第 5 步：安装 systemd 服务 ==="
  cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=IPv6 Proxy Pool (one port per client IPv6)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/${BINARY_NAME} -config $CONFIG_FILE
Restart=always
RestartSec=3
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable --now "${SERVICE_NAME}.service" >/dev/null 2>&1 || true
  sleep 1
  if systemctl is-active --quiet "${SERVICE_NAME}.service"; then
    info "服务已启动: ${SERVICE_NAME}"
  else
    error "服务启动失败，最近日志如下："
    journalctl -u "${SERVICE_NAME}.service" -n 30 --no-pager -o cat 2>/dev/null | tail -n 30 || true
    return 1
  fi

  echo
  echo "=== 安装完成 ==="
  echo "  IPv6 前缀 : $prefix"
  echo "  常驻备用  : $MIN_LEASES（启动即就绪，申请秒分配）"
  echo "  最大租约  : $MAX_LEASES（客户端端口 $PORT_START - $PORT_END）"
  echo "  空闲回收  : 超过常驻数的租约空闲 ${IDLE_TIMEOUT_MIN} 分钟后回收"
  echo "  管理地址  : $ADMIN_LISTEN"
  echo "  管理令牌  : $token   ← 请立即保存！客户端通过 -token 或环境变量 IPV6_PROXY_POOL_TOKEN 使用"
  echo
  show_connection_info
  echo
  echo "在服务器本地，现在就可以为客户端分配代理："
  echo "  sudo $0 client-new client-a"
  echo "远程客户端使用："
  echo "  ipv6-proxy-pool client create -name client-a -admin http://<服务器IP>:10070 -token <令牌> -server <服务器IP>"
  echo "或  ./control.sh client-new client-a 之后把输出的 127.0.0.1 换成服务器公网地址"
}

do_start() {
  need_root
  if ! [ -f "$SERVICE_FILE" ]; then
    error "尚未安装，请先执行: $0 install"
    exit 1
  fi
  systemctl start "${SERVICE_NAME}.service"
  sleep 1
  if service_is_active; then info "服务已启动。"; else
    error "启动失败，日志："; journalctl -u "${SERVICE_NAME}.service" -n 20 --no-pager -o cat 2>/dev/null | tail -n 20 || true
  fi
}

do_stop() {
  need_root
  systemctl stop "${SERVICE_NAME}.service" 2>/dev/null || true
  info "服务已停止。"
}

do_restart() {
  need_root
  if ! [ -f "$SERVICE_FILE" ]; then
    error "尚未安装，请先执行: $0 install"
    exit 1
  fi
  systemctl restart "${SERVICE_NAME}.service"
  sleep 1
  if service_is_active; then info "服务已重启。"; else
    error "重启失败，日志："; journalctl -u "${SERVICE_NAME}.service" -n 20 --no-pager -o cat 2>/dev/null | tail -n 20 || true
  fi
}

do_status() {
  if [ -f "$SERVICE_FILE" ] && service_is_active; then
    info "服务状态: 运行中"
    systemctl show -p ActiveEnterTimestamp "${SERVICE_NAME}.service" | sed 's/^ActiveEnterTimestamp=/启动时间: /'
  else
    warn "服务状态: 未运行"
  fi
  echo
  if service_is_active || [ -x "$INSTALL_DIR/$BINARY_NAME" ]; then
    run_client_cmd status || true
    echo
    run_client_cmd list || true
  fi
}

do_logs() {
  local lines=${1:-100}
  case "$lines" in
    ''|*[!0-9]*) lines=100 ;;
  esac
  if command -v journalctl >/dev/null 2>&1; then
    journalctl -u "${SERVICE_NAME}.service" -n "$lines" --no-pager -o cat -f
  else
    error "未找到 journalctl（非 systemd 环境），请直接查看服务输出。"
  fi
}

do_edit_config() {
  if ! [ -f "$CONFIG_FILE" ]; then
    error "配置文件不存在，请先执行 install。"
    return 1
  fi
  local editor=${EDITOR:-vi}
  command -v "$editor" >/dev/null 2>&1 || editor=vi
  "$editor" "$CONFIG_FILE"
  warn "配置已编辑。修改生效需要重启：sudo $0 restart"
}

do_release_idle() {
  if ! service_is_active; then
    warn "服务未运行，无法释放。"
    return 0
  fi
  local url token code
  url=$(admin_url)
  token=$(read_token)
  if [ -n "$token" ]; then
    code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer ${token}" "$url/v1/leases:release-idle")
  else
    code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$url/v1/leases:release-idle")
  fi
  if [ "$code" = "200" ]; then
    info "已请求释放所有空闲租约。"
  else
    warn "释放请求未成功（HTTP $code），请确认服务状态和令牌。"
  fi
}

do_uninstall() {
  need_root
  echo "将停止并删除: $INSTALL_DIR、$SERVICE_FILE"
  read -rp "确认卸载？输入大写 Y 继续: " ans
  if [[ "$ans" != "Y" ]]; then
    info "已取消卸载。"
    return 0
  fi
  systemctl stop "${SERVICE_NAME}.service" 2>/dev/null || true
  systemctl disable "${SERVICE_NAME}.service" 2>/dev/null || true
  rm -f "$SERVICE_FILE"
  systemctl daemon-reload
  if [ -f "$CONFIG_FILE" ]; then
    cp "$CONFIG_FILE" "$HOME/ipv6-proxy-pool.config.json.bak.$(date +%Y%m%d%H%M%S)"
    info "配置已备份到 $HOME/ipv6-proxy-pool.config.json.bak.*"
  fi
  rm -rf "$INSTALL_DIR"
  info "卸载完成。"
}

# ---------------------------------------------------------------------------
# 客户端管理（也对应 REST API 的使用方式）
# ---------------------------------------------------------------------------
client_new() {
  local name=${1:-}
  if [ -z "$name" ]; then
    read -rp "客户端名称（唯一标识，例如 client-a）: " name
  fi
  [ -z "$name" ] && { error "客户端名称不能为空。"; return 1; }
  if ! run_client_cmd create -name "$name"; then
    error "分配失败：请确认服务已启动（$0 status），令牌是否正确。"
    return 1
  fi
  echo
  warn "提示：客户端在远程时，请把上面打印的 127.0.0.1 换成服务器公网 IP/域名。"
}

client_rotate() {
  local name=${1:-}
  if [ -z "$name" ]; then
    read -rp "客户端名称: " name
  fi
  [ -z "$name" ] && { error "客户端名称不能为空。"; return 1; }
  run_client_cmd rotate -name "$name" || { error "更换 IP 失败。"; return 1; }
}

client_recycle() {
  local name=${1:-}
  if [ -z "$name" ]; then
    read -rp "客户端名称: " name
  fi
  [ -z "$name" ] && { error "客户端名称不能为空。"; return 1; }
  if run_client_cmd recycle -name "$name"; then
    info "已释放旧代理并重新获取（新端口 + 新 IPv6）。"
  else
    error "回收换新失败。"
    return 1
  fi
}

client_delete() {
  local name=${1:-}
  if [ -z "$name" ]; then
    read -rp "客户端名称: " name
  fi
  [ -z "$name" ] && { error "客户端名称不能为空。"; return 1; }
  if run_client_cmd delete -name "$name"; then
    info "已销毁代理并回收端口/IPv6。"
  else
    error "释放失败。"
    return 1
  fi
}

# ---------------------------------------------------------------------------
# 在线更新：git pull 后重建二进制并重启服务
# ---------------------------------------------------------------------------
do_update() {
  local repo_url=${IPV6_PROXY_POOL_REPO:-https://github.com/yhw5231/ipv6-proxy-pool.git}
  if [ -d "${SCRIPT_DIR}/.git" ]; then
    if ! command -v git >/dev/null 2>&1; then
      error "未找到 git，无法在线更新。请先安装 git。"
      return 1
    fi
    if ! git -C "$SCRIPT_DIR" remote get-url origin >/dev/null 2>&1; then
      git -C "$SCRIPT_DIR" remote add origin "$repo_url"
    fi
    local branch
    branch=$(git -C "$SCRIPT_DIR" rev-parse --abbrev-ref HEAD)
    info "拉取最新代码（分支 ${branch}）..."
    if ! git -C "$SCRIPT_DIR" pull --ff-only origin "$branch"; then
      error "git pull 失败（本地有改动或历史分叉）。"
      echo "  - 查看本地改动: git -C $SCRIPT_DIR status"
      echo "  - 丢弃本地改动: git -C $SCRIPT_DIR reset --hard origin/${branch}（会丢弃所有本地修改）"
      return 1
    fi
  else
    warn "目录不是 Git 仓库，跳过拉取，直接重建当前代码。"
  fi

  echo
  echo "=== 重建二进制 ==="
  local binary=""
  binary=$(build_fresh_binary) || {
    error "构建失败：未检测到 Go 且 ${SCRIPT_DIR}/bin/ 中没有二进制。"
    error "请安装 Go（sudo command -v go 验证）或把构建好的 ${BINARY_NAME} 放到 ${SCRIPT_DIR}/bin/。"
    return 1
  }

  echo
  echo "=== 更新文件 ==="
  mkdir -p "$INSTALL_DIR"
  cp "$binary" "$INSTALL_DIR/${BINARY_NAME}" && chmod 755 "$INSTALL_DIR/${BINARY_NAME}"
  info "二进制已更新: $INSTALL_DIR/${BINARY_NAME}"
  [ -d "$SCRIPT_DIR/web" ] && { rm -rf "$INSTALL_DIR/web"; cp -r "$SCRIPT_DIR/web" "$INSTALL_DIR/"; }

  if [ -f "$SERVICE_FILE" ]; then
    echo
    echo "=== 重启服务 ==="
    systemctl restart "${SERVICE_NAME}.service"
    sleep 1
    if service_is_active; then
      info "在线更新完成，服务已重启。"
    else
      error "服务重启失败，日志："
      journalctl -u "${SERVICE_NAME}.service" -n 20 --no-pager -o cat 2>/dev/null | tail -n 20 || true
      return 1
    fi
  else
    info "尚未通过 install 安装 systemd 服务，如需部署请执行: $0 install"
  fi
  local rev
  rev=$(git -C "$SCRIPT_DIR" rev-parse --short HEAD 2>/dev/null || echo "未知")
  echo "  当前版本: $rev"
}

# ---------------------------------------------------------------------------
# 交互菜单
# ---------------------------------------------------------------------------
menu() {
  while true; do
    clear >/dev/null 2>&1 || true
    echo
    echo "${C_BOLD}================ IPv6 代理池 管理菜单 ================${C_RESET}"
    service_is_active && state="${C_GREEN}运行中${C_RESET}" || state="${C_RED}未运行${C_RESET}"
    echo "  服务状态: $state"
    echo "${C_BOLD}------------------------------------------------------${C_RESET}"
    echo "  ${C_CYAN}1${C_RESET}) 一键安装并启动           ${C_CYAN}2${C_RESET}) 启动服务"
    echo "  ${C_CYAN}3${C_RESET}) 停止服务                 ${C_CYAN}4${C_RESET}) 重启服务"
    echo "  ${C_CYAN}5${C_RESET}) 服务状态                  ${C_CYAN}6${C_RESET}) 查看日志"
    echo "  ${C_CYAN}7${C_RESET}) 检查 IPv6 环境            ${C_CYAN}8${C_RESET}) 编辑配置"
    echo "  ${C_CYAN}9${C_RESET}) 新建客户端代理            ${C_CYAN}10${C_RESET}) 更换客户端 IP"
    echo "  ${C_CYAN}11${C_RESET}) 释放并换新（回收重建）    ${C_CYAN}12${C_RESET}) 租约列表"
    echo "  ${C_CYAN}13${C_RESET}) 释放空闲租约             ${C_CYAN}14${C_RESET}) 销毁客户端代理"
    echo "  ${C_CYAN}15${C_RESET}) 客户端连接信息           ${C_CYAN}16${C_RESET}) 在线更新"
    echo "  ${C_RED}0${C_RESET}) 卸载                    ${C_YELLOW}q${C_RESET}) 退出"
    echo "${C_BOLD}------------------------------------------------------${C_RESET}"
    read -rp "请选择操作: " choice
    case "$choice" in
      1) do_install ;;
      2) do_start ;;
      3) do_stop ;;
      4) do_restart ;;
      5) do_status ;;
      6) do_logs ;;
      7) check_env ;;
      8) do_edit_config ;;
      9) client_new ;;
      10) client_rotate ;;
      11) client_recycle ;;
      12) run_client_cmd list || warn "服务未运行或令牌错误。" ;;
      13) do_release_idle ;;
      14) client_delete ;;
      15) show_connection_info ;;
      16) do_update ;;
      0) do_uninstall ;;
      q|Q) echo "再见。"; exit 0 ;;
      *) warn "无效选项：$choice" ;;
    esac
    echo
    read -rp "按回车返回菜单..." _ || exit 0
  done
}

# ---------------------------------------------------------------------------
# 入口
# ---------------------------------------------------------------------------
if [ "${1:-}" = "" ]; then
  menu
  exit 0
fi

cmd=$1
shift || true
case "$cmd" in
  install)         do_install ;;
  start)           do_start ;;
  stop)            do_stop ;;
  restart)         do_restart ;;
  status)          do_status ;;
  info|conn-info|connection-info) show_connection_info ;;
  logs)            do_logs "${1:-100}" ;;
  config|edit)     do_edit_config ;;
  env|check)       check_env ;;
  release-idle)    do_release_idle ;;
  list|leases)     run_client_cmd list ;;
  client-new|client-create) client_new "${1:-}" ;;
  client-rotate)   client_rotate "${1:-}" ;;
  client-recycle)  client_recycle "${1:-}" ;;
  client-delete|client-remove) client_delete "${1:-}" ;;
  uninstall)       do_uninstall ;;
  update)          do_update ;;
  help|--help|-h)  usage ;;
  *) error "未知命令: $cmd"; usage; exit 1 ;;
esac