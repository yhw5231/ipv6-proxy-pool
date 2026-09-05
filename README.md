# Go IPv6 代理池

这是一个使用 Go 实现的 IPv6 SOCKS5 代理池，支持常驻备用租约池（保底数量）、弹性超额分配、IPv6 租约分配、一端口一 IPv6、客户端申请/更换/回收/销毁代理、空闲自动回收、Web 管理、管理令牌、SSH 管理菜单和 Docker 部署。

项目保留原有的 `ipv6-incognito` 目录。Go 代理池不会修改或调用其中的 Windows 地址管理功能。

## 功能

- **常驻备用池**：启动即预生成 `min_leases` 个备用代理（端口 + IPv6 就绪），客户端申请秒级分配
- **弹性超额**：备用用完仍可继续申请，直到 `max_leases` 总上限
- **保底空闲回收**：仅当租约总数超过 `min_leases` 时才按空闲超时回收，常驻部分永不回收
- **释放自动补充**：任何释放/回收都会把旧租约转为全新备用（新 IPv6），池子自动回填
- 从配置的 IPv6 CIDR 前缀中生成租约地址
- 按时间/请求数自动轮换 IPv6（均可关闭）
- SOCKS5 `CONNECT` 转发
- 使用 `net.Dialer.LocalAddr` 绑定租约 IPv6 作为出站源地址
- `multiplex` 单端口复用模式
- `per_ipv6` 一端口一 IPv6 模式（客户端一个端口 + 一个 IPv6）
- 指定常开端口
- 通过管理 API 动态创建、更换（`rotate`）、回收换新（`recycle`）、释放租约及监听器
- 客户端 CLI 子命令：申请代理 / 更换 IP / 回收换新 / 销毁代理 / 列表 / 状态
- 可选的管理令牌（`admin.token`），保护所有 `/v1/*` 请求
- Web 可视化管理
- SSH 管理菜单：一键安装运行、停止、重启、卸载（`control.sh`）
- JSON 配置校验和原子保存
- Docker 多阶段构建与 Compose 部署

## 常驻备用与弹性分配模型

池子分两类租约：

| 角色 | 说明 |
|---|---|
| `standby`（常驻备用） | 启动时预生成、持有端口和未使用的 IPv6，等待被认领；默认列表不展示 |
| `client`（客户端租约） | 被客户端认领的备用或新建租约，真正承载代理流量 |

行为规则：

1. **申请**：优先认领一个备用（端口、IPv6 立即可用）；备用耗尽后新建租约，直到 `max_leases` 总上限，超限返回 `409`
2. **空闲回收**：只回收 `client` 租约，且只在**总数 > `min_leases`** 时执行——常驻保底部分即使空闲也不会被回收
3. **释放/回收**：旧租约转为全新备用（新 IPv6、端口保留）直到备用补满 `min_leases`，超出部分才真正销毁
4. **持久租约**（`persistent: true`，含 `always_on_ports`）：完全豁免空闲回收

## 快速开始（SSH 管理菜单）

在目标 Linux 主机上（拥有 tunnelbroker.net 分配的 IPv6 /64 网段）：

```bash
# 1. 从 Git 仓库拉取代码（推荐，便于后续在线更新）
git clone https://github.com/yhw5231/ipv6-proxy-pool.git
cd ipv6-proxy-pool

# 也可以从其他机器上传已有目录（此时如需在线更新，建议补一条 remote）：
#   git init && git remote add origin https://github.com/yhw5231/ipv6-proxy-pool.git

# 2. 进入项目目录，运行安装菜单
sudo bash control.sh
```

交互菜单提供：**一键安装并启动 / 启动 / 停止 / 重启 / 状态 / 日志 / 检查 IPv6 环境 / 编辑配置 / 新建客户端代理 / 更换客户端 IP / 销毁客户端代理 / 租约列表 / 释放空闲 / 卸载**。

也可以直接用单条命令（适合写入脚本或远程执行）：

```bash
sudo bash control.sh install        # 一键安装并启动（自动检测 /64、生成令牌、安装 systemd 服务）
sudo bash control.sh start|stop|restart|status|logs|uninstall
sudo bash control.sh update         # 在线更新：从仓库拉取最新代码、重建二进制并重启
sudo bash control.sh client-new client-a        # 为客户端分配代理（返回端口和 IPv6）
sudo bash control.sh client-rotate client-a     # 更换该客户端的 IPv6（端口不变）
sudo bash control.sh client-recycle client-a    # 释放并立即重新获取（新端口 + 新 IPv6）
sudo bash control.sh client-delete client-a     # 销毁代理并回收端口/IPv6
sudo bash control.sh list                       # 列出所有租约
```

安装脚本会：

- 自动检测网卡上的全局 IPv6 地址并推导 `/64` 前缀（可手动修改）
- 用 Go 构建二进制（或使用 `bin/ipv6-proxy-pool` 中已有的二进制）
- 生成 `per_ipv6` 模式配置和随机管理令牌（默认常驻 1024 / 上限 2048）
- 开启 `net.ipv6.ip_nonlocal_bind = 1` 以便绑定路由网段中的地址
- 安装并启动 systemd 服务 `ipv6-proxy-pool.service`
- 安装结束时打印管理令牌，请立即保存

## 代理模式

### multiplex 单端口复用

所有客户端连接同一个 SOCKS5 端口。客户端通过 SOCKS5 用户名、密码或租约标识携带租约 ID，服务端根据该标识选择或创建租约，并使用租约对应的 IPv6 发起出站连接。

这种模式可以让多个 IPv6 代理共用同一个端口，适合能够设置 SOCKS5 凭据的客户端。为了获得稳定、可重复使用的 IPv6，建议显式使用租约 ID。

### per_ipv6 一端口一 IPv6

每个监听端口对应一个固定租约和 IPv6。客户端只需连接指定端口，不需要通过 SOCKS5 凭据选择租约。

启动时只为 `socks.always_on_ports` 中指定的端口创建持久租约和监听器，不会根据 `max_leases` 启动整个端口范围。常驻备用租约会预留端口和 IPv6，但**不会**预开监听器——监听器在备用被认领时才启动，空闲回收/释放时关闭，文件描述符占用始终与实际客户端数量成正比。

管理 API 创建新租约时，系统会从配置的端口范围中分配端口并动态启动监听器。删除租约时，对应监听器会被关闭，端口会返回可用端口池（并优先转为常驻备用）。

## 配置

复制示例配置：

```powershell
Copy-Item .\config.example.json .\config.json
```

主要字段：

- `ipv6_prefix`：用于生成租约地址的 IPv6 CIDR 前缀
- `min_leases`：常驻备用租约数，启动时预生成并在释放后自动补充，低于该数的租约不会被空闲回收
- `max_leases`：常驻备用 + 客户端租约的总上限，必须不小于 `min_leases`
- `idle_timeout`：空闲回收时间（Go `time.Duration` 纳秒数），仅当总数超过 `min_leases` 时生效
- `rotate_after`：按时间轮换 IPv6 的间隔，设为 `0` 可关闭
- `rotate_requests`：达到指定请求数后轮换 IPv6，设为 `0` 可关闭
- `socks.mode`：`multiplex` 或 `per_ipv6`
- `socks.listen_address`：SOCKS5 监听地址；在 `per_ipv6` 模式中使用其中的主机地址
- `socks.port_start`：动态端口范围起点
- `socks.port_end`：动态端口范围终点
- `socks.always_on_ports`：启动时保持监听的端口数组，仅适用于 `per_ipv6`
- `admin.listen_address`：管理 API 和 Web 界面监听地址
- `admin.token`：可选管理令牌，设置后所有 `/v1/*` 请求都需要 `Authorization: Bearer <token>`（至少 8 位）
- `admin.webui.username`：Web 控制台登录用户名，默认 `admin`
- `admin.webui.password`：Web 控制台登录密码，默认 `admin`；打开控制台必须先登录，建议尽快改成强密码

示例（默认值：常驻 1024、上限 2048、空闲 60 分钟回收）：

```json
{
  "ipv6_prefix": "2001:db8::/64",
  "min_leases": 1024,
  "max_leases": 2048,
  "idle_timeout": 3600000000000,
  "rotate_after": 0,
  "rotate_requests": 0,
  "socks": {
    "mode": "per_ipv6",
    "listen_address": "[::]:10080",
    "port_start": 20000,
    "port_end": 22047,
    "always_on_ports": [20000, 20001, 20002]
  },
  "admin": {
    "listen_address": "[::]:10070",
    "token": "换成至少8位的随机令牌",
    "webui": {
      "username": "admin",
      "password": "换成强密码"
    }
  }
}
```

`2001:db8::/64` 是文档示例前缀，不能作为真实公网出口。部署时必须替换为真实分配并路由到宿主机的 IPv6 前缀。`per_ipv6` 模式要求端口范围大小不小于 `max_leases`（示例 20000–22047 共 2048 个端口）。

> **从旧版本升级**：旧配置没有 `min_leases` 字段，加载时会取默认值 1024。若原 `max_leases` 小于 1024，启动会报错 `min_leases must not exceed max_leases`——请把配置中的 `max_leases` 调大（例如 2048），或显式把 `min_leases` 设为 `0`（关闭常驻保底，回到旧的纯弹性模型）。
>
> **端口变更**：自本版本起，SOCKS 基础监听端口默认 `10080`，Web 管理/管理 API 端口默认 `10070`（原为 `1080` / `8080`）。升级后如仍想使用旧端口，请修改配置文件中的 `socks.listen_address` 和 `admin.listen_address`；否则请同步更新客户端的 `-admin` 地址、防火墙放行规则和反向代理配置。

## 本地运行

生成默认配置：

```powershell
go run ./cmd/ipv6-proxy-pool -config config.json -write-default-config
```

启动服务：

```powershell
go run ./cmd/ipv6-proxy-pool -config config.json
```

Web 管理界面默认地址：

```text
http://127.0.0.1:10070/
```

Web 控制台分四个页签，全部管理操作均可在浏览器完成：

- **运行状态**：服务健康、运行时间、客户端/备用租约数、池容量进度条、累计请求数、活跃监听器，以及常开端口监听表和服务参数一览
- **代理租约**：新建（弹窗表单）、换 IP、回收换新、删除（带确认）、切换持久保护、按 ID/端口/IPv6 筛选、任意列排序、复制 SOCKS5 地址；可切换显示池内备用租约
- **客户端接入**：客户端对接参数（管理端 URL、令牌、租约 ID、SOCKS5 地址）一键复制，并附内置 CLI 的完整示例命令
- **配置**：代理模式、IPv6 前缀、常驻/上限、空闲回收、轮换策略、端口范围、常开端口、监听地址、管理令牌与 Web 登录账号密码的在线编辑，保存前校验、修改后显示"未保存"徽标，保存成功提示重启生效

打开控制台会先进入登录页，使用 `admin.webui` 配置的账号密码登录（默认 `admin` / `admin`）后才能看到面板；右上角「退出」可注销会话，服务重启会使所有登录会话失效。`admin.token` 仍按原样保护 REST API 供客户端程序调用；「自动刷新」开关默认每 5 秒拉取一次状态，页签支持 `#leases` 形式的直链。

Web 页面保存的配置写入 config.json，需要重启服务后，新的代理模式、前缀和监听器配置才会完整生效。

## REST API

完整的接口文档（供其他程序调用）见：

- [`docs/API.md`](docs/API.md) — 中文接口文档：数据对象、每个端点的请求/响应、错误码、调用示例
- [`docs/openapi.yaml`](docs/openapi.yaml) — OpenAPI 3.0 规范（机器可读），可直接导入 Postman / Swagger UI / Insomnia，或用于生成 SDK

接口总览：

```text
GET    /healthz
POST   /v1/auth/login
POST   /v1/auth/logout
GET    /v1/auth/session
GET    /v1/status
GET    /v1/config
PUT    /v1/config
GET    /v1/leases
POST   /v1/leases
GET    /v1/leases/{id}
PATCH  /v1/leases/{id}
DELETE /v1/leases/{id}
POST   /v1/leases/{id}/rotate
POST   /v1/leases/{id}/recycle
POST   /v1/leases:release-idle
```

`/v1/auth/login`、`/v1/auth/logout`、`/v1/auth/session` 是 Web 控制台的登录会话接口（`GET /v1/auth/session` 无需认证，返回 `{authenticated, username}`）。设置 `admin.token` 后，除 `/healthz`、静态资源和认证接口外，所有 `/v1/*` 请求必须携带请求头或有效的控制台会话 Cookie：

```text
Authorization: Bearer <token>
```

创建租约示例（返回的 `port` 就是该客户端的 SOCKS5 代理端口，`ipv6` 就是出口地址；备用池就绪时分配是即时的）：

```powershell
Invoke-RestMethod -Method Post `
  -Uri http://127.0.0.1:10070/v1/leases `
  -ContentType application/json `
  -Body '{"id":"client-a","persistent":false}'
```

在 `per_ipv6` 模式下，创建租约会同时启动对应端口的监听器。监听器启动失败时，新建租约会回滚。删除租约会同步关闭对应监听器。

### 更换 IP（rotate）

`POST /v1/leases/{id}/rotate` 为租约分配一个新的 IPv6 出口地址，**端口保持不变**，因此客户端只需更新任何基于 IP 的授权，无需修改代理配置。空闲回收时不区分是否被 rotate 过。

### 回收换新（recycle）

`POST /v1/leases/{id}/recycle` 释放当前租约并立即以**同一 ID** 重新获取：旧租约回到备用池，客户端拿到（通常相同的）端口和一个全新 IPv6。相当于"释放后自动重新获取"，适合需要彻底重置端口和 IP 的场景。持久租约不支持 recycle（请用 rotate）。

### 空闲自动回收

- **只在租约总数超过 `min_leases`（常驻保底）时执行**；总数不超常驻数时，即使空闲也不会回收
- 仅回收非持久（`persistent: false`）的客户端租约；备用与持久租约始终豁免
- 服务后台定时扫描（间隔约为 `idle_timeout/3`，5 秒到 2 分钟），不依赖新连接触发
- 回收/释放的资源优先转为全新备用（新 IPv6）补足常驻池，备用已满时才彻底销毁
- `per_ipv6` 模式下释放租约会同步停止对应端口的监听器
- 也可以随时调用 `POST /v1/leases:release-idle` 立即回收（同样受常驻保底约束）

## 客户端 CLI

二进制内置 `client` 子命令，客户端可以通过管理 API 自助申请、更换、回收和销毁代理，无需 SSH 或 curl：

```bash
# 申请代理：返回 SOCKS5 端口和出口 IPv6
ipv6-proxy-pool client create -name client-a -admin http://服务器:10070 -token <令牌> -server 服务器公网IP

# 更换出口 IPv6（端口不变）
ipv6-proxy-pool client rotate -name client-a -admin http://服务器:10070 -token <令牌>

# 释放并立即重新获取（同 ID，新端口 + 新 IPv6）
ipv6-proxy-pool client recycle -name client-a -admin http://服务器:10070 -token <令牌>

# 销毁代理并回收端口 / IPv6
ipv6-proxy-pool client delete -name client-a -admin http://服务器:10070 -token <令牌>

# 查看所有租约 / 服务状态
ipv6-proxy-pool client list   -admin http://服务器:10070 -token <令牌>
ipv6-proxy-pool client status -admin http://服务器:10070 -token <令牌>
```

令牌也可以通过环境变量 `IPV6_PROXY_POOL_TOKEN` 提供，避免出现在命令行历史中。`-server` 参数用于拼装 `host:port` 形式的 SOCKS5 地址，省略时取 `-admin` 的主机部分。

客户端拿到端口后，把 SOCKS5 代理指向 `服务器:端口` 即可，所有请求都会从该租约对应的 IPv6 地址出站。

## Docker 部署

从仓库拉取代码并部署：

```bash
git clone https://github.com/yhw5231/ipv6-proxy-pool.git
cd ipv6-proxy-pool
```

构建镜像：

```bash
docker build -t ipv6-proxy-pool .
```

使用 Compose：

```bash
cp config.example.json config.json   # 首次部署必须：config.json 不在 Git 里
docker compose up -d --build
```

Compose 使用 Linux `network_mode: host`，使容器可以直接使用宿主机网络命名空间中的 IPv6 路由和监听端口。配置中包含 `NET_ADMIN` capability，但程序本身不会自动向网卡添加 IPv6 地址。

### 容器在线更新

已通过 Compose 部署后，使用仓库自带的更新脚本即可一条命令完成"拉取最新代码 → 重建镜像 → 滚动重启容器"：

```bash
chmod +x docker-update.sh       # 首次使用前赋予执行权限
./docker-update.sh              # 拉取最新代码并更新容器（推荐）
./docker-update.sh --skip-pull  # 跳过 git pull，仅用当前代码重建容器
```

说明：

- 脚本只重建镜像和重启容器，**不会改动 `config.json`**，令牌、租约配置全部保留；更新期间代理会短暂中断，请选择合适的时机执行
- 默认拉取 `origin` 远程仓库的当前分支；仓库地址可用环境变量 `IPV6_PROXY_POOL_REPO` 覆盖，管理端口可用 `IPV6_PROXY_POOL_ADMIN_PORT` 覆盖（用于更新后的健康检查）
- 更新完成后脚本会自动调用 `/healthz` 健康检查，并清理悬空的旧镜像
- 也可以手动执行等价操作：`git pull && docker compose up -d --build`

推荐在原生 Linux 宿主机上部署。Windows Docker Desktop 的 host 网络运行在 Linux 虚拟机中，不等价于原生 Linux host 网络，也不会自动把 Windows 网卡获得的完整 IPv6 前缀路由给容器。

### 常见问题：mount a directory onto a file

启动报错 `error mounting ... /app/config.json ... Are you trying to mount a directory onto a file (or vice-versa)?`，是因为首次部署时宿主机上没有 `config.json`（该文件不进 Git），Docker Compose 把挂载源自动创建成了**空目录**，随后无法把目录挂载到容器内的文件上。修复：

```bash
rm -rf config.json                       # 只在它确实是目录时执行
cp config.example.json config.json
docker compose up -d
```

`docker-update.sh` 已内置该检查：缺失时自动从 `config.example.json` 复制，误建为目录时自动修复。

## 真实 IPv6 网络前提

代码中的租约地址生成和源地址绑定并不代表该地址一定能够访问公网。真实部署必须满足：

- `ipv6_prefix` 是真实分配给宿主机或明确路由到宿主机的前缀
- 租约 IPv6 已配置到宿主机接口，或者 Linux 已配置 `net.ipv6.ip_nonlocal_bind = 1` 允许绑定非本地 IPv6
- 宿主机存在有效的 IPv6 默认路由
- 上游路由器允许该前缀中的地址作为源地址出站
- 邻居发现、防火墙和相关路由配置正确
- 容器能够访问宿主机的 IPv6 网络和路由

程序不会执行 `ip addr add`、`netsh` 或其他网卡地址配置命令，也不会自动把生成的 IPv6 添加到网卡。地址和路由必须由宿主机管理员单独配置（`control.sh` 安装时会提示并可选地添加一个地址）。

### tunnelbroker.net 部署要点

假设 tunnelbroker 分配了 `2001:470:xxxx:yyyy::/64` 路由网段：

1. 建立隧道后，隧道接口通常已获得一个全局 IPv6 地址（例如 `2001:470:zzzz::2/64`）
2. 把路由网段 `2001:470:xxxx:yyyy::1/64` 配置到 LAN 接口或本机回环，并确保该网段路由指向本机：

   ```bash
   ip -6 addr add 2001:470:xxxx:yyyy::1/64 dev eth0
   ```

3. 允许绑定该网段中未直接配置的地址（路由网段的其余地址）：

   ```bash
   sysctl -w net.ipv6.ip_nonlocal_bind=1
   # 持久化：写入 /etc/sysctl.d/99-ipv6-proxy-pool.conf
   ```

4. 确认 IPv6 默认路由存在且可 ping 通公网 IPv6
5. 在配置中把 `ipv6_prefix` 设为 `2001:470:xxxx:yyyy::/64`，并放行防火墙中 SOCKS 端口范围和 `admin.listen_address` 端口
6. `control.sh` 的 `install` 会自动完成第 3 步并提示第 2 步

> 注意：内核绑定非本地地址需要 `ip_nonlocal_bind=1`；如果每个出站源地址都需要能被上游路由，请确认 HE 的 `/64` 已完整路由到该主机。

## 验证

格式化、测试和构建：

```powershell
gofmt -w .
go test -timeout 30s ./...
go build ./...
```

检查前端 JavaScript：

```powershell
node --check .\web\app.js
```

检查 Compose 配置：

```powershell
docker compose config --quiet
```

真实公网出口验证必须在具备可路由 IPv6 前缀的目标主机上执行。应通过租约对应的 SOCKS5 代理访问 IPv6 地址检测服务，并确认返回的出口地址与租约 IPv6 一致。本项目的本地单元测试和构建验证不等同于真实公网 IPv6 出口验证。

## 安全建议

- Web 控制台默认账号密码为 `admin` / `admin`，部署后请立即在「配置」页修改 `admin.webui`，并限制管理端口访问来源
- 管理 API 在未设置 `admin.token` 时没有认证保护（登录页只挡住浏览器面板，拦不住直接调用 API）；建议设置 `admin.token` 保护 `/v1/*`
- 为远程客户端分配代理时，务必设置令牌并妥善保管（`install` 会自动生成）
- 使用防火墙限制 SOCKS5 和管理端口的访问来源
- 可在管理 API 前部署带认证的反向代理
- 确认你有权使用配置的 IPv6 前缀
- 修改配置后通过受控方式重启服务

