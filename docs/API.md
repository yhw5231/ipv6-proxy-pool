# IPv6 代理池 HTTP 接口文档

本文档描述 `ipv6-proxy-pool` 管理服务对外提供的 HTTP REST 接口，供其他程序（客户端程序、脚本、Web 应用）调用。

机器可读的 OpenAPI 3.0 规范见 [`openapi.yaml`](./openapi.yaml)，可直接导入 Postman / Swagger UI / Insomnia，或用于生成 SDK。

---

## 1. 基本约定

### 1.1 基础地址

管理服务监听地址由配置文件 `admin.listen_address` 决定，默认 `[::]:10070`：

```
服务基地址 = http://<服务器IP>:<admin端口>
```

### 1.2 数据格式

所有请求与响应均为 `application/json; charset=utf-8`（`DELETE` 成功返回 `204` 无响应体）。

请求体大小上限 **1 MB**。请求体中出现**未定义的字段**会被拒绝（`400`）。

### 1.3 认证

- 若配置了 `admin.token`（推荐），**所有 `/v1/*` 请求**都必须携带请求头：

  ```
  Authorization: Bearer <token>
  ```

- 未携带或令牌错误时返回 `401`：

  ```json
  { "error": "missing or invalid admin token" }
  ```

- `/healthz` 与 Web 静态资源**不需要**令牌，始终开放。

### 1.4 错误格式

所有非 `2xx` 响应均为：

```json
{ "error": "错误描述，人类可读" }
```

### 1.5 通用状态码

| 状态码 | 含义 |
|---|---|
| `200` | 成功，响应体为 JSON |
| `201` | 创建成功（创建租约） |
| `204` | 成功，无响应体（删除租约） |
| `400` | 请求体非法 / 参数缺失 / 未定义字段 |
| `401` | 缺少或错误的令牌 |
| `404` | 资源不存在（租约 ID 不存在） |
| `409` | 冲突（租约容量已满 / 端口不可用） |
| `500` | 服务内部错误（监听器启动失败等） |
| `503` | 配置持久化未启用（`PUT /v1/config`） |

---

## 2. 数据对象

### 2.0 常驻备用与弹性分配模型

池子始终保有 `min_leases`（常驻保底）个**备用租约**（`role: "standby"`），已分配给客户端的租约 `role` 为 `"client"`：

- **申请**（`POST /v1/leases`）：优先认领一个备用租约（端口 + IPv6 即刻可用）；备用耗尽后新建租约，总数最多 `max_leases`
- **空闲回收**：仅当租约总数 > `min_leases` 时执行，且只回收 `client` 租约——常驻保底部分不会被回收
- **释放/回收**：旧租约转为全新备用（新 IPv6）补足常驻池，备用已满时才真正销毁

### 2.1 Lease（租约）

一个租约 = 一个客户端标识 + 一个出口 IPv6（`per_ipv6` 模式下还绑定一个 SOCKS5 端口）。

```json
{
  "id": "client-a",
  "ipv6": "2001:470:xxxx:yyyy::a1b2",
  "port": 20010,
  "persistent": false,
  "role": "client",
  "created_at": "2026-09-01T12:00:00.123456789+08:00",
  "last_used_at": "2026-09-01T12:05:00.987654321+08:00",
  "requests": 0
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | string | 租约标识（客户端名）。创建后不可修改；备用租约为 `pool-<n>` 形式 |
| `ipv6` | string | 该租约的出站源 IPv6；`rotate` / `recycle` 后变化 |
| `port` | int | `per_ipv6` 模式下该客户端的 SOCKS5 端口；`multiplex` 模式下为 `0` 且不输出 |
| `persistent` | bool | 是否常驻。`true` 时不受空闲回收影响，且不可 `recycle` |
| `role` | string | `client`（客户端租约）或 `standby`（常驻备用） |
| `created_at` | string | 创建时间（RFC3339 / Go `time.Time`） |
| `last_used_at` | string | 最近一次被 SOCKS5 连接使用的时间 |
| `requests` | uint64 | 累计请求数；`rotate` / `recycle` 后重置为 `0` |

### 2.2 Status（服务状态，`GET /v1/status`）

```json
{
  "status": "ok",
  "uptime_seconds": 86400,
  "lease_count": 12,
  "persistent_count": 3,
  "standby_count": 1024,
  "total_requests": 45678,
  "listener_count": 12,
  "listeners": [
    { "id": "port-20000", "address": "[::]:20000" }
  ],
  "min_leases": 1024,
  "max_leases": 2048,
  "ipv6_prefix": "2001:470:xxxx:yyyy::/64",
  "socks_mode": "per_ipv6",
  "socks_listen_address": "[::]:10080",
  "port_start": 20000,
  "port_end": 22047,
  "always_on_ports": [20000, 20001],
  "idle_timeout_seconds": 3600,
  "rotate_after_seconds": 0,
  "rotate_requests": 0,
  "token_required": true
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `status` | string | 固定 `"ok"` |
| `uptime_seconds` | int | 服务已运行秒数 |
| `lease_count` | int | 当前**客户端**租约数（不含备用；含 `always_on_ports` 持久租约） |
| `persistent_count` | int | 持久租约数（含 `always_on_ports`） |
| `standby_count` | int | 常驻备用租约数（当前待分配数量） |
| `total_requests` | uint64 | 全部租约累计成功转发的请求数 |
| `listener_count` | int | per_ipv6 模式当前活跃的动态监听器数量（multiplex 恒为 `0`） |
| `listeners` | array | 活跃监听器列表，元素为 `{ "id": 租约ID, "address": 监听地址 }` |
| `min_leases` | int | 配置的常驻保底数量 |
| `max_leases` | int | 租约总量上限（备用 + 客户端） |
| `ipv6_prefix` | string | 配置的 IPv6 前缀 |
| `socks_mode` | string | `multiplex` 或 `per_ipv6` |
| `socks_listen_address` | string | SOCKS5 基础监听地址 |
| `port_start` / `port_end` | int | 动态端口范围 |
| `always_on_ports` | int[] | 配置的常开端口 |
| `idle_timeout_seconds` | int | 空闲回收超时（秒，来自配置的纳秒换算） |
| `rotate_after_seconds` | int | 按时间轮换间隔（秒，`0` 表示关闭） |
| `rotate_requests` | uint64 | 按请求数轮换阈值（`0` 表示关闭） |
| `token_required` | bool | 管理 API 是否启用了令牌保护 |

### 2.3 Config（配置，`GET/PUT /v1/config`）

`idle_timeout`、`rotate_after` 单位为**纳秒**（Go `time.Duration`），`0` 表示关闭对应功能。

```json
{
  "ipv6_prefix": "2001:470:xxxx:yyyy::/64",
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
    "always_on_ports": []
  },
  "admin": {
    "listen_address": "[::]:10070",
    "token": "可选，文档用途下不建议回传明文"
  }
}
```

> 注意：`admin.token` 仅本机管理员需要；普通客户端程序**不要**调用配置接口。`multiplex` 模式下 `socks.port_start/port_end/always_on_ports` 不生效。校验要求 `0 ≤ min_leases ≤ max_leases`，且 `per_ipv6` 模式下端口范围大小 ≥ `max_leases`。

---

## 3. 接口列表

### 3.1 GET /healthz — 健康检查（无需令牌）

**响应 `200`**

```json
{ "status": "ok" }
```

---

### 3.2 GET /v1/status — 服务状态（需要令牌）

**响应 `200`**：见 [2.2 Status](#22-statusget-v1status)。

客户端程序建议先调用本接口确认：模式、前缀、容量、服务是否在线。

---

### 3.3 GET /v1/leases — 租约列表

**查询参数**

| 参数 | 说明 |
|---|---|
| `include_standby=true` | 同时返回常驻备用租约（默认只返回 `client` 租约） |

**响应 `200`**：`Lease[]` 数组（空数组 `[]` 表示无租约）：

```json
[
  { "id": "client-a", "ipv6": "2001:470:xxxx:yyyy::1", "port": 20000, "persistent": false, "role": "client", "created_at": "...", "last_used_at": "...", "requests": 5 },
  { "id": "client-b", "ipv6": "2001:470:xxxx:yyyy::2", "port": 20001, "persistent": true, "role": "client", "created_at": "...", "last_used_at": "...", "requests": 0 }
]
```

---

### 3.4 POST /v1/leases — 创建租约（客户端申请 IP/端口）

**请求体**

```json
{ "id": "client-a", "persistent": false }
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `id` | string | 是 | 客户端唯一标识（可作用户名）；首尾空白会被去除，不能为空 |
| `persistent` | bool | 否 | 默认 `false`。`true` 表示常驻，不受空闲回收影响 |

**行为**

- 若 `id` 已存在，直接返回当前租约（幂等，仍为 `201`）
- **常驻备用就绪时立即认领一个备用**（端口 + 未使用的 IPv6 即刻可用）；备用耗尽则新建租约，总数最多 `max_leases`
- `per_ipv6` 模式：为认领/新建的租约启动对应端口监听器
- 监听器启动失败时新建租约会回滚（`500`，且不残留租约）

**响应 `201`**：`Lease` 对象。客户端程序取 `port` 作为 SOCKS5 端口、`ipv6` 作为出口地址：

```json
{
  "id": "client-a",
  "ipv6": "2001:470:xxxx:yyyy::a1",
  "port": 20010,
  "persistent": false,
  "role": "client",
  "created_at": "...",
  "last_used_at": "...",
  "requests": 0
}
```

**错误**

| 状态码 | 场景 |
|---|---|
| `400` | `id` 为空、JSON 非法、含未定义字段 |
| `409` | 租约容量已满（`lease pool capacity reached`）或端口不可用 |

> `multiplex` 模式下响应中的 `port` 为 `0`，客户端改用 SOCKS5 用户名 `user:<id>` 携带身份连接单端口池。

---

### 3.5 GET /v1/leases/{id} — 查询租约

**响应 `200`**：`Lease` 对象。

**错误**：`404` `{ "error": "lease not found" }`。

---

### 3.6 PATCH /v1/leases/{id} — 修改租约属性

**请求体**

```json
{ "persistent": true }
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `persistent` | bool | 是 | 缺省时报 `400`；`true` 免于空闲回收，`false` 恢复可回收 |

**响应 `200`**：更新后的 `Lease` 对象。

**错误**：`400`（缺 `persistent` / JSON 非法）、`404`（租约不存在）。

---

### 3.7 DELETE /v1/leases/{id} — 销毁租约（申请释放 IP/端口）

**行为**

- 停止监听器（`per_ipv6` 模式）、释放终端端口和 IPv6
- **资源优先转为常驻备用**：备用未满 `min_leases` 时，旧租约变为全新备用租约（新 IPv6、端口保留）；备用已满时端口才真正回到空闲端口池
- 之后该端口可被其他客户端复用

**响应 `204`**：无响应体。

**错误**：`404`（租约不存在）、`500`（监听器停止失败）。

---

### 3.8 POST /v1/leases/{id}/rotate — 更换出口 IP

**行为**

- 为该租约分配一个新的随机 IPv6 出口地址
- **`per_ipv6` 模式下端口保持不变**，因此客户端无需修改 SOCKS5 配置
- `created_at` / `last_used_at` 重置为当前时间，`requests` 清零；`persistent` 不变
- 新 IP 从当前 SOCKS5 连接起生效

**响应 `200`**：`Lease` 对象（`ipv6` 为新地址）。

**错误**：`404`（租约不存在）。

---

### 3.9 POST /v1/leases/{id}/recycle — 回收换新（释放并立即重新获取）

**行为**

- 释放当前租约（旧租约按释放规则转为备用或销毁，`per_ipv6` 模式下同步停止旧监听器）
- 立即以**同一 `id`** 重新申请：认领备用或新建租约，拿到全新 IPv6（`per_ipv6` 模式下通常还有全新端口），并启动新监听器
- 相当于「释放后自动重新获取」，效果类似 `rotate`（换 IP），但会彻底重建租约与监听器
- `persistent: true` 的租约不支持 recycle（`400`，请改用 rotate）

**响应 `200`**：`Lease` 对象（`ipv6` 为新地址，`port` 为新端口）。

**错误**

| 状态码 | 场景 |
|---|---|
| `400` | 持久租约不能 recycle |
| `404` | 租约不存在 |
| `409` | 容量已满（回收后重新申请失败，理论上备用就绪时不会发生） |
| `500` | 监听器启动失败 |

---

### 3.10 POST /v1/leases:release-idle — 立即释放所有空闲租约

**行为**：释放所有 `persistent=false` 且空闲超过 `idle_timeout` 的**客户端**租约（等价于手动触发后台自动回收），并同步停止对应监听器。与自动回收相同：**仅当租约总数超过 `min_leases` 时执行**，且释放的资源优先转为常驻备用。

**请求体**：无（可为空）。

**响应 `200`**

```json
{ "released": 3 }
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `released` | int | 本次实际释放的租约数 |

---

### 3.11 GET /v1/config — 读取当前配置（管理员）

**响应 `200`**：见 [2.3 Config](#23-configgetput-v1config)。`admin.token` 会回显实际值，请勿泄露给普通客户端。

### 3.12 PUT /v1/config — 覆盖保存配置（管理员）

**请求体**：完整 `Config` 对象（见 2.3），`token` 留空字符串表示清除令牌。

**响应 `200`**

```json
{ "saved": true, "restart_required": true, "config": { "..." } }
```

**错误**：`503`（未启用配置持久化）、`400`（校验失败，例如前缀无效、端口范围小于租约数、令牌短于 8 位等）。

> 配置保存后**需要重启服务**才完整生效（`restart_required: true`）。

---

## 4. 完整调用示例（客户端程序）

```bash
BASE=http://<服务器IP>:10070
TOKEN=<管理令牌>

# 1) 确认服务在线
curl -s $BASE/healthz

# 2) 申请代理：拿到端口 + 出口 IPv6（常驻备用就绪时瞬时返回）
curl -s -X POST $BASE/v1/leases \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"id":"client-a","persistent":false}'
# => {"id":"client-a","ipv6":"2001:470:...:a1","port":20010,...}

# 3) 更换出口 IP（端口不变）
curl -s -X POST $BASE/v1/leases/client-a/rotate \
  -H "Authorization: Bearer $TOKEN"

# 3b) 回收换新：释放并立即重新获取（同 ID，新端口 + 新 IPv6）
curl -s -X POST $BASE/v1/leases/client-a/recycle \
  -H "Authorization: Bearer $TOKEN"

# 4) 查询我的租约
curl -s $BASE/v1/leases/client-a -H "Authorization: Bearer $TOKEN"

# 5) 销毁代理（归还端口 + IP；备用未满时转为常驻备用）
curl -s -X DELETE $BASE/v1/leases/client-a -H "Authorization: Bearer $TOKEN" -w "%{http_code}\n"   # 204

# 6) 管理端强制回收空闲租约（总数超过 min_leases 时才生效）
curl -s -X POST $BASE/v1/leases:release-idle -H "Authorization: Bearer $TOKEN"
```

## 5. SOCKS5 使用说明

拿到 `port` 后，客户端把 SOCKS5 代理指向：

```
host = <服务器IP>
port = <返回的 port>
```

- `per_ipv6` 模式：无需认证字段，出口固定为该租约的 IPv6
- `multiplex` 模式（无独立端口，`port` 为 0）：所有客户端连接同一个 SOCKS5 端口，用**用户名** `user:<id>`（密码忽略）区分租约

空闲回收提醒：只有当租约总数超过 `min_leases`（常驻保底）时，`persistent=false` 的租约才会在空闲超过 `idle_timeout` 后被自动回收，届时对应端口停止服务；总数不超常驻数时租约不会被回收。需要长期固定的客户端可设置 `persistent=true`，或在断线后重新调用 `POST /v1/leases` 申请新租约。

## 6. 其他程序的集成建议

1. 启动时调用 `GET /v1/status` 校验连通性与模式（`standby_count` 可确认常驻备用是否就绪）
2. 用 `POST /v1/leases` 申请 → 保存返回的 `id` / `port` / `ipv6`
3. 出口 IP 被目标网站封禁时，调用 `POST /v1/leases/{id}/rotate` 换 IP（端口不变），或 `POST /v1/leases/{id}/recycle` 彻底重建
4. 任务结束调用 `DELETE /v1/leases/{id}` 归还资源（自动补充常驻备用）
5. 程序退出/崩溃前，可用 `POST /v1/leases:release-idle` 兜底清理（服务本身也会后台自动回收）