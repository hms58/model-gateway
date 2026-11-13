# model-gateway — 多产品模型网关

## 这是什么

**把多个「各家 AI 编程订阅账号变成 OpenAI 接口」的网关，再收敛成一个入口。**

`workbuddy2api` / `trae2api` / `qoder2api` 这类项目解决的是「**一个产品 → 一个 OpenAI 接口**」：
它们把你手上某个 AI 编程客户端（腾讯 CodeBuddy、TRAE SOLO、Qoder）的账号额度，
包装成标准的 `/v1/chat/completions`。

问题是它们都是**单产品**的。你有三家账号，就有三个 base_url、三套 key、三套模型命名，
三份各自独立的冷却与限流；客户端要接三家供应商，还得自己写「哪家挂了换哪家」的逻辑。

model-gateway 就架在它们之上：

| 能力 | 说明 |
| --- | --- |
| **一个入口** | 客户端只认一个 base_url（`http://127.0.0.1:8080/v1`）和一个虚拟 key（`mg-...`） |
| **跨产品组队** | 同一个模型组的成员可以来自不同上游，按**权重降序**自动降级（权重越大越优先，同权重按列表顺序） |
| **统一治理** | 软冷却、熔断半开、生效时间窗、会话粘性、请求日志、Token 授权都收在一处，不用在三家面板各配一遍 |
| **对外只露组名** | 客户端看到的是 `glm-5.3`、`gpt-5` 这类组名，不会知道背后是哪家的哪个账号池 |
| **观测** | 面板可查每请求的降级链（试了哪几个成员、各返回什么码）、TTFB、token 用量 |

它**不产生任何额度、也不做账号登录**：账号、签到、风控由各上游 `*2api` 自己管，
model-gateway 只负责「按组选路 + 降级 + 观测」。所以它的价值是**聚合与治理**，不是白嫖额度。

只依赖 Go 标准库（Go 1.21+），无第三方依赖、无数据库、无外部登录服务。

## 架构

```mermaid
graph TD
    Client["客户端 NextChat / Chatbox / Claude Code / Cline"]

    subgraph GW["model-gateway 监听 8080"]
        Token["虚拟 Token 鉴权 与 模型组授权"]
        Router["选路 权重降序 / 时间窗 / 软冷却 / 熔断半开 / 粘性会话"]
        Proxy["转发 改写 model 字段 逐块 flush"]
    end

    WB["workbuddy2api 监听 7863"]
    TR["trae2api-web 监听 7864"]
    QD["qoder2api-hub 监听 8790"]

    WBA["CodeBuddy 账号池"]
    TRA["TRAE SOLO 账号池"]
    QDA["Qoder 账号池 国内 国际"]

    Disk[("data 目录")]

    Client -->|"一个 base_url 加一个虚拟 key"| Token
    Token --> Router --> Proxy
    Proxy -->|"组内权重最高且可用"| WB
    Proxy -->|"降级"| TR
    Proxy -->|"降级"| QD
    WB --> WBA
    TR --> TRA
    QD --> QDA
    Proxy -.->|"运行态 模型缓存 请求日志"| Disk
```

一次「首选成员失败、自动换下一个」的完整链路：

```mermaid
sequenceDiagram
    participant C as 客户端
    participant G as model-gateway
    participant A as workbuddy2api
    participant B as trae2api-web

    C->>G: 请求 model 传组名 glm-5.3
    G->>G: 校验虚拟 Token 查组与权重
    G->>A: 转发 并改写为 global 域的真实模型名
    A-->>G: 返回 429 限流
    G->>G: 该成员记软冷却 换下一个成员
    G->>B: 转发 并改写为裸模型名
    B-->>G: 返回 200 流式响应
    G-->>C: 逐块 flush 原样回传
```

## 上游：三个 *2api

这三个项目都是「把某个 AI 编程客户端的账号额度转成 OpenAI 兼容接口」的网关，
各有自己的账号池、签到与风控；model-gateway 不替代它们，只在其之上做聚合。

| 上游 | 开源地址 | 默认端口 | 账号来源 | 对外协议 |
| --- | --- | --- | --- | --- |
| **workbuddy2api** | <https://github.com/misakano7545/workbuddy2api-panel> | `:7863` | 腾讯 CodeBuddy / WorkBuddy | `/v1/chat/completions`、`/v1/responses`、`/v1/messages` |
| **trae2api-web** | <https://github.com/hms58/trae2api-web> | `:7864` | TRAE SOLO | `/v1/chat/completions`、`/v1/models` |
| **qoder2api-hub** | <https://github.com/shuishuipingan/qoder2api-hub> | `:8790` | Qoder（国内 / 国际双区） | OpenAI 兼容 + Responses |

各自启动与鉴权（细节以上游 README 为准）：

| 上游 | 启动 | 鉴权 |
| --- | --- | --- |
| workbuddy2api | `docker compose up -d --build`，或 `.\wb2api.exe -config config.json` | `config.json` 的 `api_key`，`Authorization: Bearer <api_key>`；为空则不鉴权 |
| trae2api-web | `docker compose up -d --build`，或 `./trae2api-web` | 环境变量 `TW2A_API_KEY`，`Authorization: Bearer <key>` |
| qoder2api-hub | Windows 双击 `start-qoder-proxy.bat`；Linux/macOS `./start-qoder-proxy.sh` | 见上游 README；API 根地址 `http://127.0.0.1:8790` |

> 三个上游都**只提供明文 HTTP**，公网部署必须放在 HTTPS 反代之后；它们各自带管理面板，
> 面板里有账号登录、签到、模型与额度，**不要在公网直接暴露**。

## 快速开始

```bash
# 1. 复制示例配置，改成自己的上游
cp configs/modelgw.example.json configs/modelgw.json

# 2. 用 AdminToken 方式给管理口令（推荐：明文不落盘、不进配置文件）
export MODELGW_ADMIN_TOKEN='your-strong-token'

# 3. 启动（-c 必填）
go run ./cmd/model-gateway -c ./configs/modelgw.json
# 或
make build && ./model-gateway -c ./configs/modelgw.json

# 4. 打开面板，用上面的口令登录
#    http://127.0.0.1:8080/panel/
```

`listen` 就是监听端口，写在同一个配置文件里（示例 `:8080`）。

口令来源优先级：`-admin-token` > `MODELGW_ADMIN_TOKEN` > 配置文件 `admin_token_sha256` > 初始化模式。
按上面第 2 步走，示例配置里那个空的 `admin_token_sha256` 就完全用不上，也不存在「无口令」窗口。
只有在三种来源都没给时才会进入初始化模式（仅本机免登录），见「管理口令」。

### 启动参数

| 参数 | 说明 |
| --- | --- |
| `-c` | 业务配置文件路径，**必填**；也可用环境变量 `MODELGW_CONFIG` |
| `-state` | 运行态文件路径；留空则取 `MODELGW_STATE`，再回落到 `<data_dir>/state.json` |
| `-admin-token` | 管理面口令（**明文入参**，只在内存转 SHA-256，不落盘）；留空则取 `MODELGW_ADMIN_TOKEN` |

优先级：命令行 flag > 环境变量 > 配置里的 `data_dir` > 默认 `./data`。
业务配置没有任何兜底：拿不到路径直接退出并打印用法，不做「无配置降级」。

管理口令的优先级见下一节，`-admin-token` 与 `MODELGW_ADMIN_TOKEN` **高于**配置文件里的摘要：

```bash
# 推荐：口令走环境变量，不进 shell history、不进配置文件
export MODELGW_ADMIN_TOKEN='your-strong-token'
./model-gateway -c ./configs/modelgw.json
```

## 业务配置

全部配置在**一份 JSON** 里（`configs/modelgw.json`），字段见 `configs/modelgw.example.json`：

| 段 | 作用 |
| --- | --- |
| `listen` | 监听地址，如 `:8080` |
| `admin_token_sha256` | 管理面板凭据的 **SHA-256**（明文不入库）；**留空 = 初始化模式，仅本机免登录**，见「管理口令」 |
| `data_dir` | 运行态 / 模型缓存的默认目录 |
| `routing` | 最大尝试次数、冷却与熔断、粘性会话 |
| `probe` | 异常成员后台探活（间隔、同产品节流） |
| `logging` | 请求日志目录、保留天数、单文件大小上限 `max_file_mb`（超过则续写 `requests-YYYYMMDD.N.jsonl`） |
| `products` | 下游产品：`base_url` / `api_key` / 超时 / 端点路径 / 出网代理（默认关闭）/ `disabled_models`（停用模型）/ `console_url`（产品后台地址，面板点产品名直达）/ `weight`（仅面板排序，大的在前，停用的排最后） |
| `model_groups` | 对外模型组：成员 = 产品 + 真实模型 + 权重 |
| `tokens` | 虚拟 Token（只存 SHA-256，明文仅创建时返回一次） |

`<data_dir>` 下会生成：

| 文件 | 内容 |
| --- | --- |
| `state.json` | 冷却 / 熔断 / Token 计数 / 产品健康，5s 合并落盘 |
| `models.json` | 各产品模型列表缓存（10 分钟内直接读文件） |
| `logs/` | 按天 `requests-YYYYMMDD.jsonl`（请求日志）与 `prompts-YYYYMMDD.jsonl`（提示词快照），保留期到期一起清理 |
| `usage.json` | 请求与 token 用量累计：小时桶保留 90 天后折叠为日桶（永久），面板「统计」页的数据源；**不受日志保留期影响** |

### 接三家上游：最小可用配置

`products` 就是三家 `*2api` 的实例，`base_url` 指向它们的本地端口，`api_key` 填它们各自的密钥：

```json
"products": [
  {
    "id": "workbuddy",
    "name": "WorkBuddy",
    "protocol": "openai-chat",
    "base_url": "http://127.0.0.1:7863",
    "api_key": "<workbuddy2api 的 api_key>",
    "enabled": true,
    "timeout": { "connect": "5s", "first_byte": "60s", "total": "300s" }
  },
  {
    "id": "trae",
    "name": "TRAE",
    "protocol": "openai-chat",
    "base_url": "http://127.0.0.1:7864",
    "api_key": "<trae2api 的 TW2A_API_KEY>",
    "enabled": true,
    "timeout": { "connect": "5s", "first_byte": "30s", "total": "300s" }
  },
  {
    "id": "qoder",
    "name": "Qoder",
    "protocol": "openai-chat",
    "base_url": "http://127.0.0.1:8790",
    "api_key": "<qoder2api-hub 的密钥>",
    "enabled": true,
    "timeout": { "connect": "5s", "first_byte": "30s", "total": "300s" }
  }
]
```

### 接入非标准 OpenAI 兼容端点（`chat_path` / `models_path`）

默认按 `base_url + /v1/chat/completions` 与 `base_url + /v1/models` 出网，覆盖绝大多数上游。
遇到路径不含 `/v1` 段或需要查询串的服务，用这两个字段覆盖（留空即用默认值）：

| 服务 | `base_url` | `chat_path` | `models_path` |
| --- | --- | --- | --- |
| OpenAI 官方 | `https://api.openai.com` | 默认 | 默认 |
| OpenRouter | `https://openrouter.ai/api` | 默认 | 默认 |
| DeepSeek / Moonshot | `https://api.deepseek.com` | 默认 | 默认 |
| 通义 DashScope 兼容模式 | `https://dashscope.aliyuncs.com/compatible-mode` | 默认 | 默认 |
| **Gemini（OpenAI 兼容层）** | `https://generativelanguage.googleapis.com/v1beta/openai` | `/chat/completions` | `/models` |
| **Azure OpenAI** | `https://<res>.openai.azure.com` | `/openai/deployments/<dep>/chat/completions?api-version=2024-06-01` | 默认 |

```json
"chat_path": "/chat/completions",
"models_path": "/models"
```

约束：只能是**站内路径**（以 `/` 开头、不能是完整 URL），允许带查询串；填错会在保存时被拦下。
Gemini 的原生 API（`/v1beta/models/<m>:generateContent`）不是 OpenAI 协议，本网关不支持，
只能走它的 OpenAI 兼容层。

### 每个产品单独设置出网代理（默认关闭）

`products[].proxy` 缺省即直连；开启后，该产品下的**聊天转发、拉取模型目录、主动嗅探/一键嗅探**
三条出网路径都走同一个代理，不会出现「业务请求通、面板拉不到模型」的分裂状态。

```json
"proxy": {
  "enabled": true,
  "url": "socks5://user:pass@127.0.0.1:1080"
}
```

| 协议 | 写法 | 说明 |
| --- | --- | --- |
| HTTP | `http://127.0.0.1:7890` | 明文 HTTP 代理 |
| HTTPS | `https://proxy.example.com:443` | 到代理本身用 TLS |
| SOCKS5 | `socks5://127.0.0.1:1080` | 本机解析目标域名 |
| SOCKS5h | `socks5h://127.0.0.1:1080` | 由代理侧解析目标域名，避免 DNS 泄漏 |

由 Go 标准库 `net/http` 原生实现，无第三方依赖。认证信息放在 URL 的 `user:pass@` 里，
管理接口只回显掩码（`user:********@`），保存时保持掩码即表示不修改密码。
面板「产品 → 编辑 → 出网代理」可视化配置，列表中用「直连 / 协议名」显示当前状态；
填了非法协议或空地址会在保存时被拦下，不会等到真实请求才失败。

`model_groups` 把三家同一个模型挂成一组，`weight` 决定先用谁：**同一份模型在三家都有时，
按「哪家额度更该先出清 / 更稳」排权重**，失败自动往下一个降级：

```json
"model_groups": [
  {
    "name": "glm-5.3",
    "enabled": true,
    "members": [
      { "product": "workbuddy", "model": "global:glm-5.3", "weight": 100, "enabled": true },
      { "product": "trae",      "model": "glm-5.3",        "weight": 80,  "enabled": true },
      { "product": "qoder",     "model": "glm-5.3",        "weight": 60,  "enabled": true }
    ]
  },
  {
    "name": "deepseek-v4.1-flash",
    "enabled": true,
    "members": [
      { "product": "workbuddy", "model": "global:deepseek-v4.1-flash", "weight": 100, "enabled": true },
      { "product": "trae",      "model": "deepseek-v4.1-flash",        "weight": 90,  "enabled": true }
    ]
  }
]
```

要点：

- **`model` 必须写上游的真实模型名**。workbuddy2api 的模型带 `cn:` / `global:` 域前缀（见它 `/v1/models`），
  漏了前缀会被上游拒；trae2api 与 qoder2api 直接用裸名。
- **组名可以自己起**，不必等于真实模型名。客户端只看到组名，网关转发时替换成成员的真实名。
- 同组允许「不同上游 + 同名模型」并列，这就是跨产品降级；也允许同一产品挂多个模型做型号降级。
- 某个上游整体不可用时，把该产品 `enabled` 设为 `false`，组内成员自动跳过，不用改组。
- 首次接上游后到 `/panel/models` 点「刷新（回源）」拉一次真实模型清单，避免把不存在的模型写进组。
- 成员可选两种时段限制：`effective`（带日期的绝对区间，面板上的「限时生效」）与 `daily`（每天重复的时段，
  如 `{"start":"23:00","end":"07:00"}`；起点晚于终点即跨天，当天 23:00 到次日 07:00）。两者同时开启时，
  **两个都命中**才参与选路。

## 数据面

| 方法 | 路径 | 鉴权 |
| --- | --- | --- |
| POST | `/v1/chat/completions` | 虚拟 Token（`Authorization: Bearer mg-...`） |
| GET | `/v1/models` | 虚拟 Token，仅返回模型组名 |
| GET | `/healthz` | 无 |

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer mg-****" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}'
```

`model` 传**模型组名**；网关按组配置挑成员后，把它替换成成员的真实模型名再转发，其余字段原样透传。
`stream: true` 时逐块 flush，不攒批。

### 客户端接入

任何支持「自定义 OpenAI base_url + key」的客户端都能直接接：

| 客户端 | 填什么 |
| --- | --- |
| NextChat / Chatbox / Cherry Studio | API 地址 `http://127.0.0.1:8080/v1`，密钥 `mg-...`，模型填组名 |
| OpenAI SDK（Python / Node） | `base_url="http://127.0.0.1:8080/v1"`，`api_key="mg-..."` |
| Claude Code / Codex 等 CLI | 把 `ANTHROPIC_BASE_URL` / `OPENAI_BASE_URL` 指到 `http://127.0.0.1:8080` |

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8080/v1", api_key="mg-xxxx")
resp = client.chat.completions.create(
    model="glm-5.3",           # 模型组名，不是上游真实模型名
    messages=[{"role": "user", "content": "hi"}],
)
```

多轮会话想固定落在同一个成员上（避免每轮换号导致上下文不连续），请求头带一个稳定的会话 id：

```bash
curl ... -H "X-Session-Id: my-conversation-42"
```

### 提示词记录（只留本次用户原始输入）

`messages` 是整段对话历史：每轮请求都会把之前所有 `user` / `assistant` / `tool` 再发一遍，
所以一次请求里会有**多条 `user`**（历史提问）。日志只回答「这次用户问了什么」，只取**最后一条 `user` 消息**；
其余消息（含历史提问、`system`、`assistant`、`tool`）只计入 `omitted`，响应体本来就不记。

客户端常在 `user` 消息里注入 `<additional_data>` 等上下文，并用 `<user_query>` 包住真正输入：
有这对标签时**只取标签内的文本**；普通 OpenAI 客户端没有标签，整条保留。

数据分两处存，避免一次响应变成大文件下载：

| 落点 | 内容 | 读法 |
| --- | --- | --- |
| `requests-YYYYMMDD.jsonl` 的 `prompt` 字段 | 摘要：本次输入字符数、预览片段（前 200 字符）、`omitted` 历史消息数 | `GET /api/modelgw/logs` 直接带出，列表只渲染这一小段 |
| `prompts-YYYYMMDD.jsonl` | 本次输入快照（单条 `user`），**最多记 16K 字符**，超出部分只记长度 | `GET /api/modelgw/logs/prompt?request_id=...` |

口径与边界：

- 只记文本：`content` 是数组时只拼 `text` 分段，`image_url` / `input_audio` 等分段记成 `[类型]` 占位，**`data:base64` 原文不会进日志**。
- 历史提问与调用链都不落盘，`omitted` 只记录条数；详情里永远只有 1 个块。
- 单条输入超过 16K 时截前 16K 字符并标 `truncated`。
- 提示词与请求日志同受 `logging.retention_days` 约束，过期一起删；两者都按 `max_file_mb` 滚动。

详情接口两级返回，正文再分片：

```bash
# 第一级：结构。单条输入只给原文长度 / 已存长度 / 预览，不含正文
curl -H "Authorization: Bearer <口令>" \
  "http://127.0.0.1:8080/api/modelgw/logs/prompt?request_id=<id>&date=2026-11-09"
```

```json
{
  "request_id": "9f3c1e2a7b4d5608",
  "found": true,
  "time": "2026-11-09 10:00:00",
  "group": "glm-5.3",
  "chars": 244,
  "stored_chars": 244,
  "truncated": false,
  "omitted": 231,
  "messages": [
    { "index": 0, "role": "user", "chars": 244, "stored_chars": 244, "truncated": false, "preview": "只保留提示词相关…" }
  ]
}
```

```bash
# 第二级：输入的正文分片。offset / limit 按字符（rune）计，默认 4096、上限 16384
curl -H "Authorization: Bearer <口令>" \
  "http://127.0.0.1:8080/api/modelgw/logs/prompt?request_id=<id>&date=2026-11-09&message=0&offset=0&limit=4096"
```

```json
{
  "request_id": "9f3c1e2a7b4d5608",
  "index": 0,
  "role": "user",
  "chars": 244,
  "stored_chars": 244,
  "offset": 0,
  "end": 244,
  "next_offset": 244,
  "content": "只保留提示词相关…",
  "done": true,
  "truncated": false
}
```

`done=true` 表示这一段已读完；`truncated=true` 表示原文本身超过记录上限，后面部分没有落盘。
`found=false` 是正常结果（旧日志、已过保留期、或这次请求没有用户提问），不是错误。
`date` 传日志里那条记录的日期（`YYYY-MM-DD`），缺省按今天找。

> 提示词里可能含业务数据与密钥，日志目录不要放进同步盘或提交到仓库。
> 不需要这项能力时把 `logging.dir` 指到临时目录并缩短 `retention_days`。

## 管理面板

浏览器打开 `http://127.0.0.1:8080/panel/`，输入管理口令（只存在本机 localStorage）。
若尚未设置口令，本机访问会直接进入配置页并提示先设口令。

| 页面 | 作用 |
| --- | --- |
| `/panel/products` | 产品增删改、连通性测试、key 掩码展示；填了 `console_url` 的产品，点产品名可在新标签页打开它自己的后台 |
| `/panel/groups` | 模型组成员、权重上下移、成员状态与手动嗅探 |
| `/panel/models` | 各产品模型清单（可强制回源刷新）；**停用的产品不列出**，单个模型可停用/启用（被模型组使用的不能停用；停用状态按「产品 + 模型名」保存，刷新目录后仍保留） |
| `/panel/logs` | 请求日志查询（按结果 / 产品 / 状态码 / 关键字，关键字也会匹配提示词预览），分页默认 100 条/页（上限 100），可选 50/100，支持 5s 自动刷新（只存浏览器）；**提示词列只放片段，点「详情」看结构、按消息分片加载全文** |
| `/panel/usage` | 统计：请求次数、成功率、平均耗时、token 用量与缓存命中率；按天趋势（默认近 7 天）+ 按「产品 + 模型」的 token Top。**不含积分维度** |
| `/panel/tokens` | 虚拟 Token 创建、启停、组授权 |
| `/panel/settings` | 启动参数（监听 / 数据目录 / 管理口令）+ 选路、熔断、探活、日志 |

管理 API 在 `/api/modelgw/*`，全部需要管理口令；`/panel/*` 只提供静态页。
所有写操作按 `version` 做乐观锁，冲突返回 409（`error.code=20607`）。

### 管理口令（优先 AdminToken，只存 SHA-256）

口令来源优先级**从高到低**，先命中者生效：

| 优先级 | 来源 | 形态 | 是否落盘 |
| --- | --- | --- | --- |
| 1 | `-admin-token` 启动参数 | 明文 | 否，只在内存转摘要 |
| 2 | `MODELGW_ADMIN_TOKEN` 环境变量 | 明文 | 否，只在内存转摘要 |
| 3 | 配置文件 `admin_token_sha256` | SHA-256 | 是（配置里） |
| 4 | 都没有 | — | 进入**初始化模式**（仅本机免登录） |

推荐用第 2 种：明文不写进配置文件、不进 shell history（用 `.env` 或 systemd `EnvironmentFile` 注入）。
启动日志会打一行 `管理面口令来源：<来源>`，只写来源名，**绝不含口令本身**。

配置文件里**不接受明文**：`admin_token` 一旦非空，`Validate` 直接拒绝整份配置。
如果你确实想用配置文件承载口令，就写摘要：

```bash
printf '%s' 'your-token' | sha256sum     # Linux / macOS
python -c "import hashlib,sys;print(hashlib.sha256(sys.argv[1].encode()).hexdigest())" your-token
```

**首次启动（初始化模式）**

上面三种都没给时，管理面进入**初始化模式**：

| 来源 | 行为 |
| --- | --- |
| 本机（TCP 对端为 `127.0.0.1` / `::1`） | **免登录**，可直接进配置页设口令 |
| 其他任何来源 | 一律 401，提示「尚未设置管理口令：初始化只允许本机访问」 |

> 判定只看 TCP 对端地址，**不采信 `X-Forwarded-For`**：该头完全由客户端控制，
> 一旦据此放行，远程伪造 `X-Forwarded-For: 127.0.0.1` 就能改 `base_url`，
> 把上游 `api_key` 引到攻击者地址。反代场景请直接设好口令（口令校验与来源无关）。

流程：

1. `./model-gateway -c ./configs/modelgw.json` 启动（启动日志会提示去哪设口令）；
2. 本机浏览器打开 `http://127.0.0.1:8080/panel/settings`，**不需要输口令**；
3. 在「启动参数 → 管理口令」填一个口令（≥ 8 位）并保存；
4. 重启进程；之后必须用该口令登录，本机也不例外。

> 初始化模式只是为了「首次不必先在命令行算摘要」。生产环境建议直接走第 1/2 优先级，
> 这样从第一次启动起就不存在「无口令」窗口。

**改口令**

- 用 `-admin-token` / `MODELGW_ADMIN_TOKEN`：改完重启即生效，配置文件里那份摘要完全不参与。
- 用配置页「启动参数」：保存即写入摘要，**重启后生效**（旧口令在本次进程内继续有效）。

读取接口 `GET /api/modelgw/settings` 只回 `admin_token_set: true/false` 与 `admin_bootstrap: true/false`，不回显口令或摘要。

### 配置页：哪些即时生效、哪些要重启

| 项 | 生效时机 |
| --- | --- |
| 选路 / 冷却 / 熔断 / 粘性 / 探活开关与间隔 / 日志保留 | 保存即生效（探活间隔下一轮生效） |
| `listen`、`data_dir`、管理口令 | **下次启动生效**（页面会提示当前实际监听地址与 `pending_restart`） |
| 产品 / 模型组 / Token | 保存即生效 |

启动参数采用「留空即不修改」：只提交选路类字段时不会清掉监听地址。

## 异常恢复

四条路径并存，前两条靠真实流量、后两条不耗 token：

| 路径 | 触发 | 行为 |
| --- | --- | --- |
| 软冷却到期 | 时间到 | 自动重新参与选路，下一次真实请求即验证 |
| 熔断半开 | 熔断到期后第一个请求 | 只放行一个探测；成功清零，失败翻倍再开（封顶 1h） |
| 后台探活 | `probe` 定时器（默认 60s，且成员需开启 `probe`） | 对冷却/熔断且临近解除的成员发一次真实请求（`max_tokens=1`） |
| 手动嗅探 | 面板「嗅探」按钮 | 立即对单个成员发一次真实请求，同上 |

`probe.min_gap` 是同一产品的探活节流。探活失败只更新 `last_error`，不会把成员重新打进熔断。
`/v1/models` 只能判断产品是否在线，**模型是否可用必须真实请求**，所以探活约消耗 1 token。
后台异步嗅探按**模型组**开关（`model_groups[].probe`）：开启则组内所有成员都参与，关闭则整组都不参与，默认关闭；
运行状态里的「一键嗅探」不依赖该开关，会串行探测本组所有异常成员。

## 目录结构

```text
model-gateway/
├── cmd/model-gateway/    启动入口（flag 解析、http.Server、路由装配）
├── configs/              示例业务配置
├── internal/
│   ├── config/           配置模型 + 校验 + 原子写 + 口令摘要 + 路径解析
│   ├── router/           选路：分层 / 权重 / 时间窗 / 冷却 / 熔断 / 粘性
│   ├── panel/            管理 API + 管理口令鉴权 + SPA（web/ 为 //go:embed）
│   └── pkg/              catalog / dataplane / probe / prober / proxy / reqlog / store / token
└── scripts/bench.sh      基线压测
```

## 测试与压测

```bash
make test    # go test -gcflags=all=-l ./... -cover
make lint    # go vet ./...
```

```bash
GATEWAY=http://127.0.0.1:8080 TOKEN=mg-**** MODEL=gpt-5 \
CONCURRENCY=4 DURATION=10 bash scripts/bench.sh
```

输出 `requests / ok / fail / qps / p95_ms`，默认写 `data/bench/<时间戳>.txt`（`OUT=` 可改）。脚本只做可复现记录，不设硬门槛。

## 部署注意

- 请求日志与模型缓存都在 `<data_dir>`，容器化时挂卷持久化，否则重启丢失冷却状态与 Token 计数。
- 前置反向代理若开启 gzip，SSE 会被攒批：对 `/v1/*` 关闭压缩。
- 管理口令请设成强随机值；它等同于管理面全权限。
- 三家上游的账号、签到与风控由各自项目负责；**model-gateway 不登录账号、不产生额度**。
- 上游默认只听 `127.0.0.1` 时，把三家 `base_url` 也写 `127.0.0.1`，不要把它们的端口暴露到公网。

## 与上游的关系

`workbuddy2api` / `trae2api` / `qoder2api` 都是各自独立的开源项目，
本仓库只在其上做「多产品聚合 + 统一治理」，不含它们的代码，也不对其可用性做任何保证。
上游接口变化、账号风控与合规边界由各自项目与其使用者承担。
