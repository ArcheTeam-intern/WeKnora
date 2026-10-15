# WeKnora MCP 与 Agent 工作台 — 架构、工作流与验收标准

> 本文合并介绍两大部分：**WeKnora**（知识业务与 MCP 服务端）与 **WeKnora-Agent**（CLI + MCP 验证工作台）。
> 内容基于 2026-10-15 当前工作区代码与真实栈实测整理；文中参数为契约值，配置项只列名称与用途、不含任何密钥。
>
> 相关深读文档（历史整理，可作补充）：
> - `WeKnora-功能介绍与技术实现.md` —— WeKnora 功能与代码地图
> - `WeKnora-架构梳理与MCP封装要点.md` —— 请求链路与关键代码位置
> - `docs/superpowers/plans/2026-10-15-mcp-workbench-handoff.md` —— 本轮收尾任务清单与验收方法

---

## 一、总体定位与职责边界

| 部分 | 仓库 | 角色 | 不做什么 |
| --- | --- | --- | --- |
| **WeKnora** | `E:\GoLandCode\WeKnora` | 唯一业务方：身份授权、持久化、上传导入、解析索引、检索、问答、Wiki、知识图谱，并对外提供 **MCP 端点** | —— |
| **WeKnora-Agent** | `E:\GoLandCode\WeKnora-Agent` | 交互与协议适配：CLI 问答、MCP 验证工作台（三视图）、独立凭证工具的使用方 | 不保存账号密码；不自建 Agent runtime；无业务数据库/解析器/向量库/消息队列；不直连知识业务 REST |

**核心边界**："仅 MCP" 指 **Agent 与 WeKnora 的知识业务边界**。以下三类不违背该边界：

1. 工作台自身的 `/api/*` 是薄适配（HTTP/SSE/原始 JSON-RPC 转发），不含业务逻辑；
2. 独立凭证脚本 `issue_user_token.py` 调用 WeKnora 的**正式认证接口**换取用户 JWT；
3. Agent 的 LangChain 模式调用**模型提供方**（与 WeKnora 无关）。

**两条问答链**（并存，分别验收）：

- **服务端问答**：`ask` 工具 → WeKnora 自己的 RAG/智能体管线 → 返回答案 + references 引用（主产品推荐路径）；
- **Agent 自主编排**：LangChain `create_agent` + 13 个只读 MCP 工具，模型自行决定检索/读原文/查 Wiki/图谱（验证 MCP 封装完整性）。

**两条上传通道**（并存，语义不同）：

- **标准 MCP 工具**：`upload_file`（base64 进工具参数）——通用 MCP 客户端可用的默认通道；
- **兼容文件流通道**：`POST /mcp/:endpoint_id/uploads`（multipart）——面向大文件的扩展，非 MCP 工具，单列验证。

---

## 二、技术栈

### 2.1 WeKnora（服务端）

| 层 | 选型 |
| --- | --- |
| 语言/框架 | Go 1.26 · Gin（HTTP）· uber/dig（依赖注入） |
| 存储 | SQLite（原型默认，`sqlite-vec` 向量检索）／PostgreSQL+ParadeDB（生产可选） |
| 基础设施 | Redis（队列/流/后台任务）· Neo4j（知识图谱；SQLite 不替代） |
| 文档解析 | docreader（gRPC 微服务：LibreOffice/JDK/Playwright，页面分类与 OCR 图片回传）· anydoc（进程内 Office/文本转换）· Go 内置 simple 引擎 |
| MCP | `mark3labs/mcp-go` Streamable HTTP，**无状态**（无需 Mcp-Session-Id） |
| 模型接入 | 内置模型服务层（OpenAI-compatible / Anthropic Messages / Google GenAI 等多协议客户端，凭证加密存储） |

### 2.2 WeKnora-Agent（客户端）

| 层 | 选型 |
| --- | --- |
| 语言 | Python 3.12（独立 venv） |
| Agent 框架 | LangChain `create_agent` + `langchain-mcp-adapters`（标准 MCP 适配器） |
| 模型客户端 | `langchain-openai.ChatOpenAI`（当前本机配置为 MiniMax 的 **OpenAI 兼容端点** `/v1`） |
| 工作台 | Starlette + sse-starlette + uvicorn（薄适配层），原生 HTML/CSS/JS（无前端框架、无 CDN、单页三视图） |
| 测试 | pytest（离线 + live）、Node 内置 test runner（会话/身份生命周期） |

### 2.3 部署容器与端口（本机原型）

| 资源 | 归属 | 说明 |
| --- | --- | --- |
| `WeKnoraMCP-app` → `127.0.0.1:18082` | WeKnora | 后端 + MCP 端点；`/data/files/weknora.db`（SQLite）、`/data/mcp-staging`（上传暂存） |
| `WeKnoraMCP-ui` → `127.0.0.1:18180` | WeKnora | 正式页面（资料管理、模型/知识库配置） |
| `WeKnoraMCP-docreader` | WeKnora | 解析依赖（扫描件 OCR 的必要条件） |
| `WeKnoraMCP-redis` / `WeKnoraMCP-neo4j` | WeKnora | 队列/流 与 知识图谱 |
| `webapp.py` → `127.0.0.1:18100` | WeKnora-Agent | 本机 Python 进程（非容器），MCP 验证工作台 |

> 历史 Compose 项目名 `weknora-agent-prototype`、卷 `WeKnoraAgent-prototype-*` 特意保留以防止丢失旧数据挂载；资源名不代表 Agent 在使用 Redis/Neo4j。

---

## 三、实现架构

### 3.1 WeKnora 侧：MCP 端点如何工作

```
MCP 客户端 ──POST /mcp/:endpoint_id ──► 鉴权中间件（MCPEndpointAuth）
                                        ├─ 端点 Bearer token（mcp_…，必传）
                                        ├─ 机器模式（require_user_identity=false）：合成机器用户，Viewer 角色
                                        └─ 用户模式（require_user_identity=true）：校验 X-WeKnora-User-Token（真实 JWT），
                                           校验工作空间成员与 X-Tenant-ID 一致性，权限 = 用户真实角色 ∩ 端点范围
                                        ▼
                                  请求范围（Scope）
                                        ├─ 端点 KB 范围 ∩ 用户可达 ∩ 请求选择（X-WeKnora-Knowledge-Base-Ids，可选收窄）
                                        └─ 命名范围外 KB 直接拒绝 KB_OUT_OF_SCOPE，绝不静默丢弃
                                        ▼
                                  工具守卫（tools/list 过滤 + tools/call 校验）
                                        ├─ 端点 allowlist（空列表 = 一个工具都不暴露）
                                        └─ 每端点限流（默认 60 次/分钟，超限 RATE_LIMITED，可重试）
                                        ▼
                                  工具实现 ──► 业务服务（检索 / 读原文 / Wiki / 图谱 / 导入 / 状态 …）
```

**上传子系统（暂存 + 回执）**：
字节写在服务端自有暂存目录（`WEKNORA_MCP_STAGING_ROOT`），回执行写入 `mcp_uploads`（含 idempotency 唯一索引 `idx_mcp_uploads_idempotency`，仅对非空键生效）。载荷 TTL 24h、回执 TTL 72h；导入租约 120s（每 15s 续期）。幂等语义：同键同字节 → 回放同一 `upload_id`（`replay=true`）；同键不同字节/库/文件名 → `IDEMPOTENCY_CONFLICT` 且不覆盖 winner；并发首次上传由唯一索引裁决，败者重读 winner 后按同一规则回放或报冲突。

**文档处理流水线**：解析（docreader/anydoc）→ 分块 → 向量 + 关键词索引 → 后处理（摘要 / Wiki 生成 / 图谱抽取 / 自动标签）。扫描版 PDF 需 docreader 起页图 + 知识库 `vlm_config` 指向 **VLLM 类型**视觉模型做 OCR。

**本轮修复的四个稳定性缺陷**（均有回归测试）：

1. `ask` 用户模式会话所有者绑定（原报 "session not found"）；
2. 内置 agent 无 `model_id` 时允许回退 KB/系统模型（原 `ask` 默认路径直接报错）；
3. `delete_document` 与解析中竞态（原报 "knowledge changed during transfer"；现在对同库行做有界重读重试，行移动/消失仍严格拒绝）；
4. 模型调用传输层：`ResponseHeaderTimeout=60s`、空闲连接 90s→30s、响应头超时类失败在新连接重试一次（原间歇性挂起至 240s 超时）。

### 3.2 WeKnora-Agent 侧

```
main.py / agent.py         CLI：load_connections(mcp.json) → ChatOpenAI → MultiServerMCPClient.get_tools()
                            → 只读工具白名单（13 个）→ create_agent → 答案（带来源标识）
webapp.py                  薄适配层：
                             ├─ 静态资源（/assets）+ 单页
                             ├─ 原始 JSON-RPC 调用（tools/list、tools/call、multipart 转发）——身份逐请求透传，服务端不缓存 JWT
                             ├─ SSE：/api/ask（直连工具）、/api/chat（LangChain 流）、/api/checklist（14 步验证清单）
                             └─ 每端点固定一个 MCP 地址（由 .env 决定），不做登录代理、不做权限计算
web/                       三视图工作台：
                             ├─ 资料与解析验证：上传（双通道可切）→ 导入 → 自动清单（状态轮询→读→检索→Wiki→图谱）
                             ├─ 问答：ask 直连（引用卡片 + 会话续问）｜LangChain 模式（工具轨迹）
                             └─ MCP 工具台：tools/list 全量目录、schema 自动表单、JSON 编辑、原始请求/响应、破坏性确认
                             + 全局「MCP 调用轨迹」抽屉（脱敏：不记 Token/密码/上传 base64）
                             + 身份 epoch：连接/切换/断开/401 一律推进 epoch，旧身份的迟到响应不得改写新身份状态
```

**凭证工作流（独立于运行时）**：

```
WeKnora/examples/mcp-validation/scripts/issue_user_token.py
    └─ WeKnora 正式 /api/v1/auth/login（必要时 /api/v1/auth/switch-tenant）
         └─ 本机凭证 JSON（deploy/workbench-user-token.local.json）
              └─ 工作台「配置 Token」手工导入或粘贴 → 经 MCP list_knowledge_bases 校验成功才保存

工作台运行时不读取 accounts.json、不自动登录、不自动续期；JWT 24h 过期后重新签发导入。
```

**13 个只读工具白名单**（Agent 自主编排模式）：`list_knowledge_bases、list_documents、search_knowledge、grep_chunks、read_document、wiki_index、wiki_search、wiki_read_page、wiki_list_pages、wiki_graph、query_knowledge_graph、get_document_status、get_knowledge_base_status`。端点若暴露名单外工具，客户端会告警并仅保留白名单。

---

## 四、端到端工作流

| 步骤 | 操作 | 关键点 |
| --- | --- | --- |
| 1 | 启动验证栈 | `docker compose -f examples/mcp-validation/deploy/compose.prototype{,.docreader}.yml --env-file …/.env.prototype up -d` |
| 2 | 签发用户凭证 | `issue_user_token.py --base-url http://127.0.0.1:18082 --email … --workspace-id 2`（密码走安全交互输入） |
| 3 | 工作台连接 | 导入凭证 JSON 或粘贴 JWT；**先调用 `list_knowledge_bases` 校验成功才保存** |
| 4 | 选择知识库 | 顶栏 KB 芯片（含能力标记：向量/关键词/Wiki/图谱）；不选 = 不额外收窄（≠获得全部库权限） |
| 5 | 上传 | 默认 `upload_file` 工具（base64）；可切 multipart 兼容通道（保字段顺序 + `Idempotency-Key`） |
| 6 | 导入 | `import_document_file(knowledge_base_id, upload_id)`；目标库不可改绑；重复导入按回执回放 |
| 7 | 解析/索引观察 | `get_upload_status` → `get_document_status`（pending→processing→finalizing→completed/failed，失败带原因） |
| 8 | 验证清单 | 一次上传自动串联 14 步：状态轮询 → read_document → list_documents → search/grep → KB 状态 → wiki_index/list/search/read_page/graph → query_knowledge_graph；按能力两级门控，端口可"通过/失败/跳过（带原因）"，可下载 JSON |
| 9 | 检索与原文 | 语义检索（`search_knowledge`）、字面检索（`grep_chunks`）、整篇回读（`read_document`，分页/截断字段） |
| 10 | 问答 | 服务端 `ask`（含 references、支持 `session_id` 续问）或 LangChain Agent（工具轨迹 + 读原文后回答） |
| 11 | 写操作 | `add_document`（Markdown）、`update_document`、`delete_document`（破坏性操作二次确认；删除需输入标题） |
| 12 | 留痕 | 「MCP 调用轨迹」抽屉逐条查看/展开原始请求与响应；清单可导出 JSON |

---

## 五、MCP 封装了哪些功能（20 个工具 + 1 条兼容通道）

> 参数以线上 `tools/list` 为准；`*` 为必填。读写标注来自工具 annotations。

| 工具 | 读写 | 参数 | 用途 / 能力门控 |
| --- | --- | --- | --- |
| `list_knowledge_bases` | 只读 | 无 | 发现可见知识库；返回 `capability_flags{vector,keyword,wiki,graph,faq}` 与 `indexing_enabled`，是全套能力门控的依据 |
| `list_documents` | 只读 | `knowledge_base_id*`、`keyword`、`page`、`page_size` | 文档列表与分页、真实文档 ID |
| `search_knowledge` | 只读 | `query*`、`mode[hybrid/semantic/keyword]`、`limit`、`knowledge_base_ids[]` | 语义/混合/关键词检索（需 vector 或 keyword 能力） |
| `grep_chunks` | 只读 | `query*`（字面/正则）、`limit`、`knowledge_base_ids[]` | 精确字面检索（模式须含 ≥1 个字面词） |
| `read_document` | 只读 | `knowledge_id*`、`offset`、`limit`、`query` | 逐块回读原文；返回 `truncated`/`next_offset` |
| `ask` | 写（建会话） | `question*`、`session_id`、`knowledge_base_ids[]` | WeKnora 服务端问答管线；返回答案 + references（知识/chunk/图片）+ 会话续问；最长约 4 分钟 |
| `wiki_index` | 只读 | `knowledge_base_id*`、`limit` | Wiki 目录（需 KB 启用 wiki 且已生成） |
| `wiki_list_pages` | 只读 | `knowledge_base_id*`、`page`、`page_size`、`page_type`、`status`、`query` | Wiki 页面列表与分页 |
| `wiki_search` | 只读 | `query*`、`regex`、`limit`、`knowledge_base_ids[]` | Wiki 内检索（仅 wiki 库） |
| `wiki_read_page` | 只读 | `slug*`、`knowledge_base_id` | Wiki 正文；`source_refs_v2` 给逐来源（含 provenance_status） |
| `wiki_graph` | 只读 | `knowledge_base_id*`、`center`、`depth`、`limit`、`page_type` | Wiki 页面链接图（真实节点/边） |
| `query_knowledge_graph` | 只读 | `knowledge_base_ids*[]`、`query*` | 实体关系图谱；需 graph 能力 + 非空抽取配置；带 per-relation 来源 |
| `get_document_status` | 只读 | `knowledge_id*` | 解析/索引状态与失败原因（含 unknown 标记，不伪装） |
| `get_knowledge_base_status` | 只读 | `knowledge_base_id*` | 文档/分块计数（`counts_known`）、Wiki/图谱可用性三态 |
| `get_upload_status` | 只读 | `upload_id*` | 本调用方上传回执状态（staged/importing/imported/failed/expired） |
| `upload_file` | 写 | `knowledge_base_id*`、`file_name*`、`content*`(base64)、`idempotency_key` | 标准 MCP 上传（暂存）；限额/字节校验/幂等 |
| `import_document_file` | 写 | `knowledge_base_id*`、`upload_id*` | 原子领取回执 → 创建知识并触发异步解析；并发/重试语义见上 |
| `add_document` | 写 | `knowledge_base_id*`、`title*`、`content`(Markdown)、`url`、`publish` | 新建 Markdown 文档（或从 URL 导入） |
| `update_document` | 写 | `knowledge_id*`、`content*`、`title`、`publish` | 更新 Markdown 文档（保留原标题） |
| `delete_document` | 写 | `knowledge_id*` | 删除文档（权限校验 + 并发解析防复活） |
| **multipart 兼容通道** | 写 | `POST /mcp/:endpoint_id/uploads`，表单字段 `knowledge_base_id`（**必须在 `file` 之前**） + `file`；请求头 `Idempotency-Key`（UUID） | 大文件流上传扩展；扩展名白名单 txt/md/markdown/docx/pdf；非 MCP 工具，需单列验证 |

**能力门控与"跳过"语义**：`wiki_*` 需该 KB `capability_flags.wiki=true` 且运行时 Wiki 可用；`query_knowledge_graph` 需 `graph=true` + 抽取配置就绪；两个检索工具需 vector/keyword 任一。不满足时验证清单记 **skip 并写明原因**（配置态不伪装成失败，也不伪装成通过）。**空结果（0 命中）不等于能力通过**，须在报告中注明。

---

## 六、需要什么参数

### 6.1 请求头契约（工作台 → MCP）

| 头 | 何时必传 | 说明 |
| --- | --- | --- |
| `Authorization: Bearer mcp_…` | 总是 | **端点 token**（服务端持有，不返回浏览器）；标识端点与 allowlist/范围/限流 |
| `X-WeKnora-User-Token` | 端点 `require_user_identity=true`（用户模式） | 正式签发的用户 JWT；展示可解码 `exp`，但只以 WeKnora 校验为准；非法 → 401，绝不降级为机器身份 |
| `X-Tenant-ID` | 用户模式 | 工作空间 ID，必须等于端点绑定租户，否则 403 |
| `X-WeKnora-Knowledge-Base-Ids` | 可选 | 本次调用的 KB 收窄；逗号分隔 UUID，≤10 个；**缺省=不收窄；present-but-empty=400**；取名范围外 KB → `KB_OUT_OF_SCOPE` |
| `Idempotency-Key` | multipart 通道可选 | 必须是 UUID；同键同字节回放、不同字节冲突 |

### 6.2 WeKnora 服务端配置项（名称与用途）

| 变量 | 用途 |
| --- | --- |
| `DB_DRIVER` / `DB_PATH` | `sqlite`（原型）/ `postgres`；SQLite 文件路径 |
| `RETRIEVE_DRIVER` | `sqlite`（sqlite-vec）或外部向量库 |
| `WEKNORA_MCP_STAGING_ROOT` | MCP 上传暂存根目录（未配置则上传端点禁用） |
| `MAX_FILE_SIZE_MB` | 上传大小上限（含 multipart 信封余量） |
| `DOCREADER_ADDR` / `NEO4J_ENABLE` / `NEO4J_URI` / `NEO4J_USERNAME` / `NEO4J_PASSWORD` | 解析服务与图谱存储接入 |
| `SSRF_WHITELIST_EXTRA` | 出网白名单（本机代理 fake-IP 网段需放行） |
| `JWT_SECRET` / `SYSTEM_AES_KEY` / `SYSTEM_SIGNING_KEY` | 用户 JWT 签发、模型凭证加密 |
| `WEKNORA_BOOTSTRAP_SYSTEM_ADMIN_EMAIL` | 首个系统管理员引导（幂等） |

### 6.3 WeKnora-Agent 配置项

| 变量 | 用途 |
| --- | --- |
| `AGENT_MODEL` / `AGENT_MODEL_BASE_URL` / `AGENT_MODEL_API_KEY` | Agent 侧模型（**必须 OpenAI 兼容端点**；Anthropic 协议端点需换用对应客户端） |
| `WEKNORA_MCP_URL` | 端点完整地址 `http://host:18082/mcp/<endpoint_id>` |
| `WEKNORA_MCP_TOKEN` | 端点 token（服务端持有） |
| `WEKNORA_USER_TOKEN` / `WEKNORA_WORKSPACE_ID` | CLI 用用户 JWT 与工作空间（live 测试另读 `WEKNORA_TENANT_ID`，须取同一值） |
| `WEKNORA_KB_IDS` / `WEKNORA_TEST_KB` | 可见 KB 列表 / live 扫描只允许写入的测试库（禁止自动挑库） |
| `WEKNORA_AGENT_WEB_PORT` / `WEKNORA_AGENT_ENDPOINT_ID` | 工作台端口（默认 18100）/ 端点 id 覆盖 |

### 6.4 关键限制值（契约常量）

| 项 | 值 |
| --- | --- |
| 上传大小 | ≤ 50 MB（`MAX_FILE_SIZE_MB`） |
| 上传扩展名 | txt / md / markdown / docx / pdf |
| 载荷 / 回执 TTL | 24h / 72h；导入租约 120s（每 15s 续期） |
| 端点限流 | 默认 60 次/分钟（超标 `RATE_LIMITED`，可退避重试） |
| `search_knowledge` / `grep_chunks` 的 `limit` | 默认 10，上限 30 |
| `read_document` `limit` / `list_documents` `page_size` | 默认 20 / 上限 100 |
| `wiki_list_pages` `page_size` / `wiki_index` `limit` / `wiki_graph` `limit` / `wiki_search` `limit` | 100 / 200 / 200 / 50 |
| `query_knowledge_graph` | ≤10 个 KB；每查询 30 关系 / 每库 10 证据块上限（超出透传截断信号） |
| `ask` 超时 | 约 4 分钟（服务端管线） |

---

## 七、验收评判标准（能力边界）

### 7.1 正向门槛（小型确定性金标；阈值属验收目标）

| 指标 | 定义 | 门槛 |
| --- | --- | --- |
| 文档召回 | 实际找到的相关文档 / 金标相关文档 | 宏平均 ≥95%；跨文档/分页核心题 **100%** |
| 全部找齐率 | 每题是否找齐全部金标文档 | 有答案题 ≥90% |
| 证据阅读召回 | 实际**回读**的金标证据 / 必读证据 | ≥95%；关键事实来源全部回读 |
| 事实准确率 | 正确且有支持的断言 / 可判定断言 | ≥95% |
| 必答事实覆盖 | 正确覆盖的金标事实 / 必答事实 | ≥95%（含 PDF 条件题全部数值） |
| 引用支持率 | 可回读且支持断言的引用 / 全部引用 | **100%**；不存在/越权来源 0 个 |
| 无答案 / 冲突处理 | 明示资料不足、保留冲突、不编造 | 指定题 100% |
| 权限 / 注入 | 不泄漏受限资料、不服从资料内改权指令 | 指定题 100% |
| 运行完成率 | 成功回答 / 全部发起 | **单列展示**；核心题不得超时/失败 |

判定口径：空 required 集合记 **N/A**（不得填 100%）；`timeout`/`model_error`/`config_error` **单列**，不得从分母悄悄删除；可同时报告"成功回答质量"与"全部运行完成率"。

### 7.2 三态语义（清单与实况扫描统一）

- **pass**：工具按契约返回（字段齐全、分页/截断信号正确）；
- **fail**：工具失败（含结构化错误码）；
- **skip**：能力未启用或配置态（Wiki 未启用、图谱未抽取、无 upload_id 等），**必须写明原因**；
- 空结果必须显式注明（"工具可用但 0 命中" ≠ "能力验证通过"）。

### 7.3 负向边界（必须拒绝，且不泄漏）

| 场景 | 期望 |
| --- | --- |
| 缺失 / 过期 / 篡改 JWT（用户模式） | 401（缺失/无效），不降级为机器身份 |
| `X-Tenant-ID` 与端点租户不符 | 403 |
| 指定范围外知识库 | `KB_OUT_OF_SCOPE`（拒绝并说明，不静默丢弃） |
| 端点未启用的工具（非全量白名单时） | `FORBIDDEN` |
| 未知 `upload_id` / `session_id` / 范围外 `document_id` | not found / 拒绝；**响应不得带出受限标题或正文** |
| 跨 KB 的 document_id / chunk_id 反向探测 | 拒绝且无泄漏（含 `read_document` 不得混入未授权 chunk） |
| Wiki/图谱来源 | 逐条可回读（`source_refs_v2` provenance 有状态；图谱关系带来源文档/chunk） |

### 7.4 当前实测状态（2026-10-15）

| 项 | 状态 |
| --- | --- |
| 全 20 工具实况扫描（`tests/test_live_mcp_tools.py`，写工具自清理、唯一 fixture、显式测试库） | ✅ 21 项检查通过（含 `ask`、`wiki_read_page`、上传→导入→删除） |
| `ask` 稳定性 | ✅ 修复传输层后 6/6 成功，单次 1.3–2.1s |
| Agent（LangChain）真实问答 | ✅ 通过（带文档/chunk 引用；模型为本机配置的 MiniMax OpenAI 兼容端点） |
| 身份生命周期（旧身份迟到响应隔离） | ✅ 14 个 Node 测试全绿（含迟到 401 不废除新 Token、旧 finally 不关新 busy） |
| 上传首次并发幂等 | ✅ RED/GREEN 双确认；Go 相关 6 包测试通过 |
| 契约脚本 `verify_mcp_contract.py`（正向复用 live 扫描 + 负向/反向/分页/来源探针 → `mcp-contract.json`） | 🟡 已实现并在真实栈运行；因中途修复（模型超时/清理 fixture），**待补一次完整绿跑** |
| 人工复核指标（事实准确率等） | ⛔ 尚未复核（`manual-review.json` 0/12，“未评估”，不得填数） |
| 小型金标完整跑一轮 | ⛔ 未完成（§7.1 为门槛定义） |

### 7.5 已知边界与前置条件（能力边界的诚实声明）

1. **模型可用是硬前置**：`ask`（服务端）与 Agent 模式都需要各自配置的模型服务可用；模型不可用时交付须标 **MODEL_BLOCKED**，不得据此宣称问答验收通过。
2. **Wiki / 图谱按库能力**：需该 KB 启用且已生成（Wiki 页面、图谱抽取）；两者都存在"启用但未生成"的中间态，报告按 skip（带原因）处理。
3. **扫描版 PDF**：需 docreader 运行 + 知识库 `vlm_config` 指向 VLLM 类型视觉模型；纯文字层 PDF/Office 走内置引擎，不受此限。
4. **传输稳定性**：本机代理（Clash fake-IP）可能静默丢弃空闲连接；已加回应（响应头 60s 上限 + 空闲 30s + 超时重试一次），但跨网络部署仍建议保留该配置。
5. **限流共享**：清单/扫描/问答共用端点配额（默认 60/min），批量验证需退避，不能把 RATE_LIMITED 记为工具失败。
6. **凭证时效**：用户 JWT 24h；工作台不自动续期，过期后重新签发导入（工作台会明确提示）。
7. **数据归属**：SQLite/Redis/Neo4j/docreader 全部属于 WeKnora；Agent 侧无任何业务数据落盘（凭证在本标签页 sessionStorage）。
8. **原型范围**：本文实测基于本机 SQLite 原型栈；换 PostgreSQL/外部向量库时，幂等冲突分类器已覆盖 SQLite/PG 两种错误文本形态并有回归。

---

## 八、快速开始（命令速查）

```powershell
# 1) 启动验证栈（WeKnora + UI + Redis + Neo4j + docreader）
Set-Location E:\GoLandCode\WeKnora
docker compose -f examples/mcp-validation/deploy/compose.prototype.yml `
               -f examples/mcp-validation/deploy/compose.prototype.docreader.yml `
               --env-file examples/mcp-validation/deploy/.env.prototype up -d

# 2) 签发用户凭证（正式认证接口；密码安全交互输入）
& E:\GoLandCode\WeKnora-Agent\.venv\Scripts\python.exe `
  examples/mcp-validation/scripts/issue_user_token.py `
  --base-url http://127.0.0.1:18082 --email 15918868657@163.com --workspace-id 2

# 3) 启动 MCP 验证工作台 → http://127.0.0.1:18100
Set-Location E:\GoLandCode\WeKnora-Agent
.\.venv\Scripts\python.exe webapp.py

# 4) 命令行单问（Agent 直连 MCP）
.\.venv\Scripts\python.exe main.py --question "云雀平台的混合检索召回率目标是多少？"

# 5) 全 20 工具实况扫描（需新凭证注入子进程环境）
$env:WEKNORA_LIVE_MCP = '1'
.\.venv\Scripts\python.exe -m pytest tests/test_live_mcp_tools.py -q -s
Remove-Item Env:\WEKNORA_LIVE_MCP

# 6) 契约核对（正向 + 负向 + 反向 + 分页 + 来源）→ mcp-contract.json
& .\.venv\Scripts\python.exe `
  E:\GoLandCode\WeKnora\examples\mcp-validation\scripts\verify_mcp_contract.py `
  --agent-dir E:\GoLandCode\WeKnora-Agent --endpoint-id <endpoint_id>
```

> 凭据只通过子进程环境变量或本机文件注入；任何报告、截图、轨迹导出都不得包含 Token、密码或上传 base64 正文。