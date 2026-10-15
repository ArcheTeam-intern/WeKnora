# WeKnora MCP 与独立 Agent 工作台收尾实施方案

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 保留已有修改，完成独立 Token 获取、薄工作台交互、WeKnora MCP 稳定性和真实功能验收，使 Agent 的知识业务依赖仅为 MCP。

**Architecture:** WeKnora 负责身份授权、持久化、上传导入、解析索引、检索、问答、Wiki 和知识图谱。WeKnora-Agent 保留标准 LangChain Agent、MCP 配置、CLI 和可选验证工作台；工作台只做交互、协议适配、状态显示和验证编排。独立凭证脚本通过 WeKnora 正式认证接口取得用户 JWT，不能伪造签名，也不能成为 Agent 的运行时登录代理。

**Tech Stack:** WeKnora Go/Gin/GORM、当前本机 SQLite/Redis/Neo4j/docreader；Agent Python/LangChain/langchain-mcp-adapters/Starlette、原生 HTML/CSS/JavaScript。

---

## 1. 执行前先读：这是已有改动的交接，不是从 HEAD 重做

用户于 **2026-10-15（北京时间）明确要求转为生成方案，由其他模型继续执行**。本文件之后不代表原实现者继续完成业务修改。下一位执行者应先核验现状，再按任务收尾。

本机仓库：

| 项目 | 路径 | 写方案时 HEAD | 分支 |
|---|---|---|---|
| WeKnora | `E:\GoLandCode\WeKnora` | `c43b5fb66` | `codex/mcp-prototype-correction` |
| WeKnora-Agent | `E:\GoLandCode\WeKnora-Agent` | `450dde8` | `codex/mcp-prototype-correction` |

**两边都有大量未提交修改，也有之前其他模型留下的修改。** WeKnora 的 `internal/` 下确实存在生产 Go 改动，不能再说“没有生产 Go 修改”。本轮新增凭证工具、Agent 鉴权收敛、前端调整和 Compose 命名也在工作区，没有本轮新提交。

- 不执行 `git reset --hard`、`git clean`、整体覆盖、批量 `git add .`。
- 不把“未提交”理解成“可以删除”。先逐文件查看 diff，承接现有实现。
- 不删除知识库、原 PDF、SQLite 文件、Docker 数据卷；不执行 `docker compose down -v`。
- 不把 `.env`、`accounts.json`、真实 JWT、端点 Token、密钥配置写进报告或提交。
- 不靠直改 `mcp_endpoints` 数据表、读取 JWT_SECRET、自签 JWT 绕过端点授权。
- 后续若提交，只显式暂存本任务确认过的文件；保留来源不明或无关修改。

### 1.1 已固定的产品决策

1. 默认账号 **15918868657@163.com**；暂不开发多账号统一登录或 SSO。
2. 凭证获取在独立脚本完成；工作台输入 Token/工作空间或导入凭证 JSON。
3. 工作台不存密码、不自动登录、不自动续期、不执行角色查询或权限计算。
4. 页面默认问答继续调用 **WeKnora MCP `ask`**；另保留标准 LangChain 只读 Agent 模式验证 MCP。
5. Wiki、知识图谱是核心能力，必须有正向证据测试，不能裁掉。
6. 允许现有三视图薄工作台；不再要求把新近用户明确要求的前端全部删除。
7. Agent 不部署自己的 Redis、Neo4j、关系数据库、解析器、向量库或业务缓存。
8. MCP 是 **Model Context Protocol**，文档统一此名称。

### 1.2 下一位模型需要读哪些文件

先读本文件，再读下面的当前源码；历史审查只作背景，冲突以本文件的最新决策和实际代码为准。

| 范围 | 文件（相对所属仓库） | 阅读目的 |
|---|---|---|
| Agent 框架 | `agent.py`、`main.py`、`mcp.json` | 标准 create_agent、只读工具过滤、请求配置 |
| 工作台适配 | `webapp.py` | HTTP/SSE 适配、原始 MCP 调用、身份转发、清单 |
| 前端 | `web/index.html`、`web/assets/{app.js,views.js,session.js,styles.css}` | Token、三视图、异步状态、样式 |
| Agent 测试 | `tests/test_{agent,http_mcp,project_boundary,webapp_routes,live_mcp_tools}.py`、`tests/test_token_session.js` | 当前行为约束与已有回归 |
| 凭证工具 | WeKnora `examples/mcp-validation/scripts/issue_user_token.py` | 独立正式登录、凭证文件输出 |
| 凭证测试 | WeKnora `examples/mcp-validation/tests/test_issue_user_token.py` | 安全输出、工作空间选择、默认路径 |
| MCP 核心 | WeKnora `internal/mcpserver/`，重点见任务 3、4 | 上传、导入、读侧、来源、权限 |
| 工具目录 | WeKnora `internal/types/mcp_endpoint_tools.go` | 20 个工具、分组、端点能力映射 |
| 部署 | WeKnora `examples/mcp-validation/deploy/compose.prototype{,.docreader}.yml` | SQLite 与基础服务归属 |
| 验证体系 | WeKnora `examples/mcp-validation/{README.md,scripts,tests,eval/questions,eval/corpus}` | 复用评估，避免在 Agent 重建业务层 |
| 旧文档纠正 | Agent `README.md`、`.env.example`、`docs/implementation-plan.md` | 有旧登录代理与工具数量文案，需要改 |

敏感配置只在本机执行时按需读取，不提供给其他模型作为聊天附件；通常交接本文件及源码路径即可。

## 2. 已做什么、哪些还不能算通过

本节是交接前的运行记录和源码复核结果，**不是要求执行者直接沿用的最终验收结论**。执行者应重新运行适用测试，并记录自己的代码版本、命令和时间。

| 项目 | 当前结果 | 证据/限制 |
|---|---|---|
| 独立凭证工具 | 已实现，曾真实正式登录成功 | 默认账号、工作空间 2；真实 Token 会过期，执行时重取 |
| Agent 登录代理 | 已移除 | `/api/accounts`、`/api/login`、`/api/relogin` 不再注册；不能加回来 |
| 请求身份 | 已改为浏览器逐请求提供 | 用户 JWT/工作空间/可选 KB；不修改进程环境来切换用户 |
| HTTP/SSE 401 | 已补主要回归 | 旧请求 401 不得删除新 Token；清单轮询鉴权元数据保留 |
| Token 前端 | 已有输入/导入、期限显示、sessionStorage | 尚有跨身份旧响应写回，见任务 2 |
| 页面美化 | 已改三视图样式 | 尚未完成逐控件、桌面/窄屏和无障碍验收 |
| 实际端点 | 当前配置端点已返回 20 个工具 | 包含 `upload_file`；旧交接里的“A2 必然 403”已不能照搬 |
| 指定 PDF | 已实际经页面上传、导入、读回 3 个 chunk | `deduplicated=true`，仅证明本次上传与复用既有解析结果 |
| PDF 自动清单 | 历史报告 14 pass / 0 fail / 0 skip | 部分检索/Wiki/图谱查询空结果，不能证明正向语义检索 |
| MCP ask | 实际请求超时 | 服务端约 240 秒报告 context deadline exceeded；不能记成功 |
| 独立聊天模型探测 | 生成请求超时 | `/models` 可达不代表生成可用；非思考模式探测同样超时，根因未定 |
| 标准 Agent 真实回答 | 尚未验收通过 | 框架测试与真实模型回答分开计 |
| 测试历史 | Agent 51 passed / 1 skipped；Token 工具 18 passed；Node 6 passed | 来自交接前实现者/复核记录；live 跳过不是实测通过 |
| Go 测试历史 | 六个相关包通过 | mcpserver、middleware、router、types、application/repository、retriever/neo4j；非全仓测试 |
| 并发上传重放 | 代码检查发现缺口 | Create 失败分支对相同内容也返回冲突，见任务 3 |

已有非密钥证据在：

```text
E:\GoLandCode\WeKnora\examples\mcp-validation\eval\reports\console-2026-10-15\pdf-checklist.json
E:\GoLandCode\WeKnora\examples\mcp-validation\eval\reports\console-2026-10-15\pdf-read.json
```

该 reports 子目录被 `.gitignore` 忽略；在这台机器可直接读。跨机器交接需单独复制经过脱敏的证据，不能假设 Git 克隆会带上。

当前实测对象供定位，执行前仍需用 MCP 核验权限与存在性：

```text
MCP endpoint: cc7eb833-82cb-4105-bd79-5ad2ee4a809b
workspace: 2
KB: 54281b47-333b-41f3-a6ad-860a543267b8
upload: d251717d-d520-4b70-9bb2-741636c66a5b
document: ca497be3-3939-43ca-8006-ff0fe908edfb
```

不得在新代码中硬编码这些实例 ID。更早文档里的端点 `eb2d80a9-...` 不是本轮实测端点。

## 3. 最终调用边界

```text
独立工具 issue_user_token.py
  └─ WeKnora 正式 auth/login（必要时正式 switch-tenant）
       └─ 本机凭证 JSON → 用户导入/输入工作台

浏览器 → WeKnora-Agent 薄工作台 → WeKnora /mcp/:endpoint_id
                            ├─ tools/list / tools/call
                            ├─ upload_file → import_document_file → 状态轮询
                            └─ 检索 / 原文 / Wiki / 图谱 / ask

CLI 或工作台 LangChain 模式
  ├─ ChatOpenAI → 用户配置的模型服务
  └─ 标准 MultiServerMCPClient → WeKnora 只读工具 → create_agent

WeKnora → SQLite / Redis / Neo4j / docreader / 模型服务
```

“仅 MCP”指 **Agent 与 WeKnora 的知识业务边界**。不排斥前端访问自身 `/api/*`、标准 Agent 调用模型提供方、独立凭证脚本调用正式认证接口。

- 标准上传验收必须走 `tools/call upload_file`，不能只凭文件流上传通过就宣称通用 MCP 客户端可上传。
- `/mcp/:endpoint_id/uploads` 是可选的兼容文件流扩展，并非通用 MCP 工具；保留时单列验证。
- `webapp.py` 允许薄的 HTTP/SSE/JSON-RPC 适配，不允许扩展成自研 Agent runtime、REST 业务桥或授权引擎。
- 浏览器给工作台：`X-WeKnora-User-Token`、`X-Tenant-ID`、可选 `X-WeKnora-Knowledge-Base-Ids`。
- 工作台到 MCP 另加服务端配置的 `Authorization: Bearer <endpoint token>`。端点 Token 不返回浏览器。
- 用户 JWT 与端点 Token 职责不同，不能互相替代。SQLite 存储不要求 Agent 共享数据库；授权始终由 WeKnora 核验。

## 4. 任务 0：固定可恢复基线

**文件：** 两仓库；新建 Agent `docs/acceptance-2026-10-15.md`，用于记录执行结果，不含秘密。

- [ ] 在两个仓库分别运行下列只读命令，记录 HEAD、分支、逐文件修改清单；确认没有擅自回滚历史修改。

```powershell
git rev-parse HEAD
git branch --show-current
git status --short
git diff --stat
git diff --check
```

- [ ] 从任务 1 开始逐项收尾；修改前先读对应现有测试。行为故障先写能复现的测试，确认失败，再最小修复。
- [ ] 记录当前进程与容器。源码变化不代表运行中的 Python 进程已重载；当前工作台进程可能早于最后一次清单 401 修复。
- [ ] 保存环境变量名称与用途，不保存实际值。不要把 `.planning` 日志或账号文件整体打包给其他模型。

## 5. 任务 1：完成 Token 工作流与运行说明（P0）

**保留并检查：**

- WeKnora `examples/mcp-validation/scripts/issue_user_token.py`
- WeKnora `examples/mcp-validation/tests/test_issue_user_token.py`
- Agent `main.py`、`webapp.py`、`web/assets/session.js`、`web/assets/app.js`
- Agent `tests/test_webapp_routes.py`、`tests/test_token_session.js`、`.env.example`

现有工具已经可用，不新建第二套 Token 签发器。正式推荐命令（安全交互输入密码）：

```powershell
Set-Location E:\GoLandCode\WeKnora
& E:\GoLandCode\WeKnora-Agent\.venv\Scripts\python.exe `
  examples/mcp-validation/scripts/issue_user_token.py `
  --base-url http://127.0.0.1:18082 `
  --email 15918868657@163.com `
  --workspace-id 2
```

默认输出：`examples/mcp-validation/deploy/workbench-user-token.local.json`，路径相对脚本定位，不能随着 cwd 变化写到未忽略位置。脚本打印路径、档案和到期时间，不打印 Token。

可为本机自动化显式传 `--accounts-file E:\GoLandCode\WeKnora-Agent\accounts.json`；此文件只能被一次性独立工具消费，工作台运行时不读取它。

凭证 JSON 契约（下列为形状示意，不是可用 Token）：

```json
{
  "profile": {
    "email": "15918868657@163.com",
    "name": "用户显示名",
    "user_id": "正式登录返回的用户ID",
    "tenant_id": "2"
  },
  "token": "正式登录返回的JWT",
  "expires_at": "正式JWT的到期时间"
}
```

- [ ] 复跑现有凭证/身份回归，不重复实现已经通过的功能。
- [ ] 验证手工输入和 JSON 导入都能调用 MCP `list_knowledge_bases` 后才保存为有效连接。
- [ ] 缺 Token、JWT 格式错误、过期 JWT、工作空间格式错误均有明确提示；浏览器解码 exp 仅作展示，不替代 WeKnora 验证。
- [ ] HTTP 401、HTTP 200 内业务鉴权错误、SSE 内鉴权错误均失效当前凭证；403 显示权限错误，不误判为需要自动登录。
- [ ] 已失效凭证刷新页面后不复活；旧 Token 的迟到 401 不删除替换后的新 Token。
- [ ] 工作台无身份时不能借用 `.env` 的其他用户身份；两个并发请求的身份不互相覆盖。
- [ ] 导入新 Token 不受上一身份遗留 KB 收窄范围影响；Token 字段用密码输入显示，轨迹/导出不包含凭证。
- [ ] 文档明确：CLI 配置使用 `WEKNORA_WORKSPACE_ID`；当前 live 测试还读 `WEKNORA_TENANT_ID`，执行时两者取同一工作空间，后续可统一命名并保留兼容。

运行：

```powershell
Set-Location E:\GoLandCode\WeKnora
& E:\GoLandCode\WeKnora-Agent\.venv\Scripts\python.exe -m pytest `
  examples/mcp-validation/tests/test_issue_user_token.py -q
Set-Location E:\GoLandCode\WeKnora-Agent
.\.venv\Scripts\python.exe -m pytest tests/test_webapp_routes.py tests/test_agent.py -q
node --test tests/test_token_session.js
```

通过条件：凭证独立获取；Agent 运行时不存在登录代理；实际认证失败保留正确分类，且不泄密。

## 6. 任务 2：隔离身份切换后的迟到请求（P0，已确认缺口）

**修改：** Agent `web/assets/app.js`、`views.js`、`session.js`（仅需要时）。

**新测试：** Agent `tests/test_identity_lifecycle.js`；可采用 Node 内置 test + vm 与最小 fake fetch/DOM，不必引入新前端框架。

问题位置：`Views.onIdentityChanged()` 清空 M/Q/B，但尚未取消或忽略旧请求。`qaAsk()` 的旧 answer 仍能写回 `Q.sessionId`，旧上传响应仍能写回 `M.receipt`。这不是服务端越权成功，但会跨身份复用会话/回执并造成授权失败和混乱显示。

- [ ] 先写真实异步回归：A 身份发起 ask/upload → 切换 B → A 请求完成 → B 状态、DOM、busy 标记不得改变；B 下一次提问不得携带 A 的 session_id。
- [ ] 同时覆盖清单 step/done、导入响应、工具响应、知识库/工具列表刷新、旧 finally 关闭新请求 busy 的情况。
- [ ] 在统一身份转换入口维护单调递增 epoch；导入/替换 Token、切换档案、清除凭证、当前 Token 401 失效都必须推进 epoch。
- [ ] 每个异步操作捕获 epoch 与本次请求输入，在 await 后、SSE 回调及 catch/finally 写共享状态或 DOM 前检查；必要时附加每操作递增序号，防止同身份旧请求覆盖新请求。

建议的最小接口（**这是待实现的新接口**，挂在现有 `window.App`；不要误以为当前已存在）：

```javascript
let identityEpoch = 0;
function captureIdentityEpoch() { return identityEpoch; }
function isCurrentIdentityEpoch(epoch) { return epoch === identityEpoch; }
function advanceIdentityEpoch() { identityEpoch += 1; }
```

应用到各异步入口的结构：

```javascript
const epoch = App.captureIdentityEpoch();
const response = await App.api(path, options);
if (!App.isCurrentIdentityEpoch(epoch)) return;
// 此后才写当前视图的状态与 DOM。
```

SSE 的事件回调和最终收尾也要相同检查；不能仅检查请求开始前 Token。若增加 AbortController，仍保留 epoch 检查，因为取消前已排队的回调可能到达。

- [ ] 401 失效路径同时清空受身份影响的 KB/工具目录/会话/回执，不能只改顶栏文案；删除旧 trace 或明确隔离，避免混在新会话里。
- [ ] Node 测试必须驱动实际 app/views 异步代码，不能只验证 `epoch++` 这个辅助函数。
- [ ] 复跑 `node --test tests/test_token_session.js tests/test_identity_lifecycle.js`，并用页面延迟响应场景手测一次。

通过条件：新身份不会读到旧请求产生的客户端状态，也不会把旧 ID 带到新身份的后续请求中。

## 7. 任务 3：修复上传首次并发的幂等恢复（P0，已确认缺口）

**修改：** WeKnora `internal/mcpserver/upload_staging.go`、`upload_repository.go`（仅在错误归一化需要时）。

**测试：** `upload_staging_test.go`、`upload_repository_test.go`。

当前 `Stage()` 的 `receipts.Create()` 错误分支调用 `recoverFromUniqueKeyConflict()`；后者不看错误，直接返回 true。找到 winner 后又一律返回 `IDEMPOTENCY_CONFLICT`，即使文件和身份完全相同。顺序重放已有校验，不应破坏。

- [ ] 为“两个请求初次查询都不存在，再竞争 Create”写确定性 barrier 测试；现有 fake Create 不模拟唯一索引，需新增专用 fake，不能只用 sleep 碰运气。
- [ ] 相同作用域/幂等键/KB/文件名/大小/SHA256：两次均成功，upload_id 相同，恰有一个 replay=true；最终只有 winner 文件有效，loser 临时目录清理。
- [ ] 同键但内容不同、KB 不同或文件名不同：返回明确冲突，不覆盖 winner；比较规则与顺序重放一致。
- [ ] 无键、无 winner、数据库断连/锁错误、主键冲突但非本幂等索引冲突：保留真实错误，不伪装成幂等成功。
- [ ] 删除恒 true 的判断；采用仓储层可识别的“该幂等唯一索引冲突”分类，再完整作用域回读与比较。优先驱动类型/约束名；若驱动只有文本，匹配应限制到本索引且有回归，不得泛匹配所有 constraint/locked 错误。
- [ ] winner 回执需可用且未过期；复用现有路径重建与过期策略。不得在回读时丢掉 endpoint/workspace/principal 三项作用域。
- [ ] 回读成功不能只比较 KB/name，要比较实际已写入的字节大小和 SHA256；不得要求客户端声明值代替服务端实算。

用现有真实 SQLite 测试库执行一次唯一索引竞争，覆盖 migration 实际索引；若数据库返回锁错误，应按真实错误测试，不能把它当成唯一冲突。

运行（安装有 CGO/SQLite FTS5 的 Go 环境）：

```powershell
Set-Location E:\GoLandCode\WeKnora
go test -tags sqlite_fts5 ./internal/mcpserver -run 'TestStage|TestUpload' -count=1
go test -tags sqlite_fts5 ./internal/mcpserver ./internal/middleware ./internal/router ./internal/types ./internal/application/repository ./internal/application/repository/retriever/neo4j
```

本机若使用已存在的校验容器 `WeKnoraAgent-correction-check`，先检查其挂载路径和 Go/CGO 能力，再在对应挂载目录运行相同命令；不假设 Windows 自带可用 gcc。测试变更前后分别确认 RED/GREEN。

## 8. 任务 4：按契约核对 MCP 封装，不把所有能力搬到 Agent（P0/P1）

**阅读/按真实失败最小修改：** WeKnora `internal/mcpserver/{server,scope,tools_upload,upload_transport,upload_staging,upload_repository,tools_import,tools_ingest,tools_status,tools_retrieve,tools_wiki,tools_graph,tools_ask}.go`、`internal/types/mcp_endpoint_tools.go`、相关中间件/仓储和现有测试。

当前 catalog 为 **20 项**，不是旧 README 的 21 项：

| 工具 | 应验证的能力 |
|---|---|
| `list_knowledge_bases` | 可见库、能力标记、范围收窄 |
| `list_documents` | 全部分页、真实文档 ID |
| `search_knowledge` | 已知语义事实命中、范围与截断 |
| `grep_chunks` | 字面词正向命中、分页、原文位置 |
| `read_document` | 原文 chunk、分页/截断、逐 chunk 来源权限 |
| `get_document_status` | 解析/索引状态与失败原因 |
| `get_knowledge_base_status` | 文档/块统计及索引能力状态 |
| `get_upload_status` | 当前调用方回执与持久化状态 |
| `upload_file` | 标准 MCP 上传、限额、字节校验、幂等 |
| `import_document_file` | 导入、任务入队、重试/并发/恢复 |
| `add_document` | Markdown 新建及返回 ID |
| `update_document` | Markdown 更新、生效与来源刷新 |
| `delete_document` | 删除权限、并发解析防复活 |
| `ask` | 现有 WeKnora 问答、续问、引用与会话权限 |
| `wiki_index` | 可用目录、范围限制 |
| `wiki_list_pages` | 页面列表与完整分页 |
| `wiki_search` | 已知页面/词正向命中 |
| `wiki_read_page` | 正文及可回读的真实来源 |
| `wiki_graph` | 页面关系、节点与来源约束 |
| `query_knowledge_graph` | 真实实体关系、来源文档/块及回读 |

- [ ] 用 `tools/list` 对照 catalog、端点 allowlist 与 handler。当前端点曾暴露全 20 项，不必再次要求用户提供 admin token 解决旧的假定阻塞。
- [ ] 若配置发生变化，先定位当前端点，再走正式 UI/API 修改有权限的配置；不得直改 DB。新端点默认只读是合理行为。
- [ ] 逐项验证：有效身份成功；缺失/过期 JWT 拒绝；工作空间不符拒绝；KB 不在范围拒绝；端点禁用工具拒绝。
- [ ] 返回其他 KB 的 document_id/chunk_id/session_id/upload_id 进行反向测试，不能只验证顶层参数。拒绝中不得带出受限正文、标题或来源。
- [ ] Wiki 引用和图谱边逐条核验来源；`read_document` 不得夹入未授权 chunk。已有 graph/wiki 工作区修复先评估再补，不照搬旧审查盲目重写。
- [ ] 检索/列表有更多结果时返回并正确处理分页/截断信息；nil 依赖、解析失败、索引失败、模型超时均为结构化错误，不能伪装成空结果。
- [ ] 补足真实失败的回归，然后跑相关包；生成 `mcp-contract.json` 记录每个工具的 positive/negative/evidence 状态。

通过条件是调用者只凭外部有效配置、标准 MCP 客户端及被允许的工具即可完成流程；不依赖 WeKnora 内部 Python/Go 模块、Agent 登录代理、REST 知识业务 API。

## 9. 任务 5：完成前端美化与功能清晰度（P1）

**修改：** Agent `web/index.html`、`web/assets/{styles.css,app.js,views.js}`；不新建另一套前端工程。

已有中性白/青绿色、深色侧栏和三视图样式，继续在此基础上完善。

- [ ] 顶栏清楚展示默认账号、工作空间、Token 到期时间、连接状态；普通用户不必理解 HTTP 头名才能连接。
- [ ] 材料页保留“上传→导入→解析/检索检查”，标准 MCP 工具为默认通道；文件流兼容选项放次级位置。
- [ ] 把“调用成功”和“检索有命中/证据完整”分开显示。空结果不是绿色的“检索能力验证通过”。
- [ ] 问答页区分“服务端问答”和“LangChain 验证”；引用可定位原文。超时、工具失败不能变成空白气泡或永久转圈。
- [ ] 工具台保留目录搜索、schema 表单、JSON 模式、原始结果。高级诊断置于可收起区域，不挤占主流程。
- [ ] 将残留“登录后加载”改为“配置 Token 后加载”；不要恢复账户密码表单。
- [ ] 表单每项补稳定的 id/for 或明确可访问名称；键盘可操作模态框、文件选择、提交、关闭。
- [ ] 写操作保留现有确认；删除展示实际目标，确认文案与行为一致。不要为测试删除用户已有文档。
- [ ] 在 1440×900、1024×768、390×844 检查布局；原始 JSON 区自身滚动，不撑破整页；状态可读且不只靠颜色。
- [ ] 将静态资源版本 `20261015-1` 更新为新的统一版本或内容 hash，验证浏览器实际加载最新 app/views/session，避免缓存导致误测。
- [ ] 保存三视图桌面截图及一张窄屏截图到验收证据目录，并检查无 Token/密钥暴露。

验证：`node --check web/assets/app.js`、`views.js`、`session.js`；随后用真实浏览器逐项执行任务 7。样式调整不需要为每个 CSS 属性写单元测试。

## 10. 任务 6：定位模型生成超时，分别验证两条问答链（P0）

**配置位置：** Agent 本机 `.env`；WeKnora 正式模型设置。仅在定位到代码故障时修改 Agent `main.py`/`webapp.py` 或 WeKnora 问答调用代码。

当前模型配置名 `deepseek-flash`、主机 `api.deepseek.com`；模型目录请求成功但实际生成多次超时。**尚未确定根因，不能断言是 Token、MCP、思考模式或前端造成。**

- [ ] 先独立做一次有总截止时间的非流式最小聊天请求，以及一次真实 tool-calling 请求，记录耗时、HTTP 状态、异常类型，禁止记录密钥或完整认证头。
- [ ] 分别核对 URL 路径、模型名、服务配额、网络/系统代理、SDK 版本和 timeout。探测加总 wall-clock 上限，避免 HTTP200/SSE 心跳导致永远等待。
- [ ] 如用户提供备用模型，只通过本机配置替换；服务端 ask 与 Agent 各有模型配置，不能只改 Agent `.env` 就宣称 ask 修好。
- [ ] 模型最小请求通过后，先从标准 MCP 客户端调用 ask，再从页面调用 ask；核对同样问题的真实回答与引用。
- [ ] 验证 LangChain 模式调用只读 MCP 工具，再读原文后回答；不能只拿预设模型测试通过代替。
- [ ] 保持合理限时与明确失败提示，不无限重试、不伪造成功答案、不将限时调到数十分钟掩盖问题。

若外部生成仍不可用：继续完成协议、权限、前端与非模型实测，交付标记 `MODEL_BLOCKED`，附失败证据和配置前提；**整体问答验收不得通过**，无需因此回退到 Agent REST 业务代理。

## 11. 任务 7：真实页面逐功能验收，必须保留证据（P0）

工作台 `http://127.0.0.1:18100`；WeKnora UI `http://127.0.0.1:18180`；WeKnora 服务 `http://127.0.0.1:18082`。启动前检查是否已运行，不要重复占端口。

必要时仅重启已确认属于本项目的 `webapp.py` 进程；不要杀掉所有 Python。命令：

```powershell
Set-Location E:\GoLandCode\WeKnora-Agent
.\.venv\Scripts\python.exe webapp.py
```

自动化后台启动应使用隐藏窗口和脱敏日志。测试前确保进程实际读取到最新源码。

### 11.1 指定 PDF 与新解析各自验证

指定文件：`E:\GoLandCode\Bio-patent-parser\pdf\sample-transaminase-short.pdf`。

- [ ] 用浏览器配置新 JWT，选择允许的 KB，标准工具通道上传指定 PDF；记录 upload_id、SHA256、大小、回执。
- [ ] 导入，观察 get_upload_status 与 get_document_status，记录 queued/deduplicated/解析/索引状态；成功后 read_document 读完原文全部页/块。
- [ ] 指定 PDF 若再次 dedup，不删除原文强迫新解析。另建含本次 UUID 的 Markdown（非模型新解析）验证新文档处理；若需要证明新 PDF 解析，用无依赖的 PDF 生成器创建独立唯一内容 PDF，保留生成脚本和文件，不修改用户原 PDF。
- [ ] 新文件导入需观测新 document_id 与异步解析实际完成，不能仅以 queued=true 判成功。若本机缺 PDF 生成工具，可补最小测试依赖到 WeKnora 验证目录，不添加到 Agent 生产依赖。

PDF 预期事实与原文定位：

| 问题事实 | 应有原文事实 |
|---|---|
| 公开编号/分类 | WO 2024/000001 A1；C12N 9/10 |
| 变体 | SEQ ID NO:2，至少 80% identity |
| 突变描述 | S24K、F291Y、A383V/A388D；保留原文斜线表达，不自行解释组合 |
| ee | greater than 99%，不能改成等于 99% |
| 反应条件 | pH 8.0、30–45 C、10% DMSO |
| 权利要求类别 | 多肽、核酸、宿主细胞和方法，需读到 claims 部分 |

先前原文主要 chunk `744fdb28-e3c7-4f9e-bc8e-461dd90b0b3f`，claims chunk `36a16bd7-4d77-4fbb-b58d-def6e1cbc8e0`；若重新解析生成新 ID，以当前回读为准。

### 11.2 页面验收矩阵

每行至少记录一次操作、实际结果、证据文件、pass/fail/blocked；不能凭代码存在就勾通过。

| 功能 | 真实操作与断言 |
|---|---|
| 凭证 | 手动输入/JSON 导入、过期拒绝、清除、刷新、迟到401、切换身份时旧结果隔离 |
| KB 选择 | 单选/多选/清空；清空表示不额外收窄，不表示获得全部库权限 |
| 文件 | 点击选择/拖拽、缺文件/缺目标、标准上传、回执显示 |
| 幂等 | 同 key 同字节重放成功、不同字节冲突、首次并发符合任务3 |
| 文件流 | 兼容通道单独跑一次，记录其扩展性质与字段顺序 |
| 导入/解析 | 导入、状态轮询、新解析与 dedup 分开、失败原因可见 |
| 检索 | grep S24K/SEQ ID NO、语义问题命中指定文档、read_document 逐块回读 |
| 清单 | 运行/重跑、展开原始响应、下载 JSON、空结果明确注明 |
| 写工具 | 用唯一 UUID 的 Markdown 新建→读取→更新→读取→删除→确认不可再读 |
| ask | 真实答案、原文引用、同身份续问、新会话、超时恢复按钮 |
| LangChain | 工具轨迹、原文回读、答案、错误/超时及重试按钮状态 |
| Wiki | index/list 全部分页、已有唯一词正向 search、read_page 与来源回读 |
| Wiki 图 | graph 返回真实页面节点/边，并与可见页面对应 |
| 实体图谱 | 已知实体正向关系、来源 document/chunk 可授权回读；空图不能算正向成功 |
| 工具台 | 搜索、选择、表单参数、JSON 参数、非法 JSON、执行、原始结果、写操作确认 |
| 轨迹 | 开/关/展开/清空；无用户JWT、端点Token、模型密钥或上传base64正文 |
| 路由/布局 | 三视图往返/刷新、桌面/窄屏、键盘操作；浏览器无未处理异常 |

Wiki/实体图谱优先用现有已生成的明确事实；若无可用正例，通过 WeKnora 正式能力准备隔离语料并等生成完成。缺配置记配置阻塞，不能用“返回空数组”替代能力通过。

### 11.3 修正 live 测试的清理风险后才运行

**文件：** Agent `tests/test_live_mcp_tools.py`。

当前 scratch_md 为固定内容，可能 dedup 到其他轮次文档，随后测试 delete 删除旧资料。先改唯一 fixture，再运行 live：

```python
import uuid

run_id = uuid.uuid4().hex
scratch_md = f"# 控制台实测草稿 {run_id}\n\nMCP live fixture: {run_id}\n"
```

文件名/标题同步带 run_id；只清理本轮明确新建的 ID，若 deduplicated=true 不删它。记录所有新建 ID 便于失败后人工清理。现有 `_pick_kb` 自动选库也应改为显式使用本轮允许的测试 KB，防止写到用户业务库。

运行时把最新凭证只在本机子进程环境注入 `WEKNORA_USER_TOKEN`、`WEKNORA_TENANT_ID`、`WEKNORA_WORKSPACE_ID` 和选定 KB，不在终端打印值、不持久化系统环境：

```powershell
Set-Location E:\GoLandCode\WeKnora-Agent
# 仅在完成上述唯一 fixture/显式KB/清理保护后执行。
$env:WEKNORA_LIVE_MCP = '1'
.\.venv\Scripts\python.exe -m pytest tests/test_live_mcp_tools.py -q -s
Remove-Item Env:\WEKNORA_LIVE_MCP
```

若环境由包装脚本注入，使用 try/finally 恢复原值；不依赖旧 `.env` 中已过期的 JWT。live 报告的 skip 必须列原因，不能合并进通过数。

## 12. 任务 8：建立可复核的能力边界评估（P1）

**复用/完善：** WeKnora `examples/mcp-validation/scripts/{run_eval,score_eval,recompute_found,evidence_records}.py`、现有 questions/corpus、评分测试。

不要把评估逻辑或语料导入业务放到 Agent 生产模块。先读现有输入 schema 和 CLI `--help`，复用已有协议，不再创建不兼容格式。

- [ ] 建立小型确定性金标：单文档、跨文档、分页超首屏、Wiki来源、实体关系、冲突事实、无答案、受限文档、恶意资料指令等至少 10 题。
- [ ] 每题列 required document IDs、required evidence IDs/原文片段、必答事实、禁止推断；金标在本轮语料导入后绑定真实 ID。
- [ ] 每题记录实际 tools/list、tools/call、查询参数、分页/截断、被回读的证据、最终答案、引用、时间、模型、运行状态。只有命中 ID 未读正文，不算完成证据阅读。
- [ ] 分开评估服务端 ask 与标准 Agent；如比较检索基线，只比较同语料/权限/问题，不能混合成一个平均值掩盖 Agent 漏检。

原型通过阈值（属于本方案定义的验收目标，不是当前成绩）：

| 指标 | 定义 | 本小型金标门槛 |
|---|---|---|
| 文档召回 | `实际找到的相关文档交集 / 金标相关文档数` | 宏平均 ≥95%；跨文档/分页核心题必须100% |
| 全部找齐率 | 每题是否找到所有金标相关文档 | 有答案题 ≥90% |
| 证据阅读召回 | `实际回读的金标证据数 / 必读证据数` | ≥95%，关键事实来源全部回读 |
| 事实准确率 | `正确且有支持的可判定断言 / 全部可判定断言` | ≥95% |
| 必答事实覆盖 | `正确覆盖的金标事实 / 必答事实` | ≥95%；PDF条件题所有数值正确 |
| 引用支持率 | `可回读且支持对应断言的引用 / 全部引用` | 100%；不存在/越权来源0个 |
| 无答案/冲突 | 明示资料不足/保留冲突，不编造 | 指定题100% |
| 权限/注入 | 不泄漏受限资料，不服从资料中的改权指令 | 指定题100% |
| 运行完成率 | 成功回答次数 / 所有发起次数 | 单列展示；核心题不得超时或失败 |

空 required 集合的召回记 N/A，不能填100%。timeout/model_error/config_error 单列；不能从分母悄悄删除后宣称总体通过。可同时报告“成功回答条件下的质量”和“全部运行的完成率”。小样本金标仅证明该范围，不证明所有文档都能找齐。

- [ ] 至少完整运行一轮金标；对漏文档/漏事实/幻觉分别归因到检索范围、分页、原文阅读或生成。
- [ ] 评分器改动写回归；运行现有 validation tests，不用人工改报告数字凑达标。

## 13. 任务 9：部署归属与文档收口（P1）

**修改：** Agent `README.md`、`.env.example`、`docs/implementation-plan.md`；WeKnora `examples/mcp-validation/README.md` 和 deployment 文案；Compose 只在有具体缺口时改。

当前本机事实：

| 资源 | 当前归属/用途 |
|---|---|
| `WeKnoraMCP-app` | WeKnora 服务；SQLite `/data/files/weknora.db`、RETRIEVE_DRIVER=sqlite |
| `WeKnoraMCP-ui` | WeKnora 正式页面 |
| `WeKnoraMCP-redis` | WeKnora 队列/流/后台处理基础设施 |
| `WeKnoraMCP-neo4j` | WeKnora 知识图谱；SQLite不会替代它 |
| `WeKnoraMCP-docreader` | WeKnora 文档解析依赖 |
| Windows `.venv` 中 `webapp.py` | Agent 薄工作台，无业务数据库 |

上述 5 个运行容器已经从 `WeKnoraAgent-*` 改名为 `WeKnoraMCP-*`；两份 Compose 已同步。**历史 Compose project `weknora-agent-prototype`、network `WeKnoraAgent-network`、volume `WeKnoraAgent-prototype-*` 特意保留，防止丢失旧数据挂载。** 这些资源名称不是 Agent 在使用 Redis/Neo4j 的证据。镜像标签仍可能保留历史 Agent 字样，同样应解释归属，不为美观强制迁卷。

- [ ] 用 `docker ps`、只读取白名单字段的 inspect 和 `docker compose config --quiet` 验证归属/配置；不打印完整 env（含秘密）。
- [ ] 若保留校验服务的旧 `WeKnoraAgent-check` 名称，说明其是 WeKnora 构建测试辅助容器，不是 Agent 数据依赖；可改显示名，但不迁卷。
- [ ] Agent README 删除运行时 accounts.json、自动登录、REST管理接口探查、固定21工具等旧说明；补 token工具命令、手动导入、过期处理和两条问答链。
- [ ] `.env.example` 区分：模型配置、端点配置、CLI 用户身份；Web 用户身份来自浏览器，不暗示空配置会偷偷回退到服务器用户。
- [ ] 新建脱敏交付报告，列本轮完整命令/版本、20工具矩阵、页面矩阵、评估分子分母、截图和剩余阻塞；旧审查文档注明历史，不删除用户资料。
- [ ] 结束时按需停止本轮开启的校验辅助容器/探测进程；保留业务服务与用户页面。不要凭名称杀进程。

## 14. 最后验证与完成定义

按实际改动执行，不把“测试存在”当成“测试运行过”。离线回归基线：

```powershell
Set-Location E:\GoLandCode\WeKnora-Agent
.\.venv\Scripts\python.exe -m pytest tests -q
.\.venv\Scripts\python.exe -m pip check
node --test tests/test_token_session.js tests/test_identity_lifecycle.js
node --check web/assets/app.js
node --check web/assets/views.js
node --check web/assets/session.js
git diff --check

Set-Location E:\GoLandCode\WeKnora
& E:\GoLandCode\WeKnora-Agent\.venv\Scripts\python.exe -m pytest examples/mcp-validation/tests -q
go test -tags sqlite_fts5 ./internal/mcpserver ./internal/middleware ./internal/router ./internal/types ./internal/application/repository ./internal/application/repository/retriever/neo4j
docker compose --env-file examples/mcp-validation/deploy/.env.prototype -f examples/mcp-validation/deploy/compose.prototype.yml -f examples/mcp-validation/deploy/compose.prototype.docreader.yml config --quiet
git diff --check
```

Go 若在验证容器中运行，报告真实命令和工作目录；不要把缺 CGO/测试跳过写成通过。`test_identity_lifecycle.js` 是任务2要求新增的文件，未完成任务2前该命令会失败，不能从最终检查删掉来掩盖缺口。

最终交付需要同时满足：

- [ ] 独立凭证工具可用，工作台手工配置有效，JWT失效/切换无旧状态写回。
- [ ] Agent 框架与知识业务边界清晰，无运行时登录代理、无直连数据库或 REST 知识业务旁路。
- [ ] WeKnora MCP 的上传/导入/状态/检索/原文/Wiki/图谱/ask 均有真实正向证据及必要权限反例。
- [ ] 指定 PDF 完成真实上传/导入/原文/问答验证；另外证明一次真正新解析，明确区分 dedup。
- [ ] 页面矩阵逐项运行完成；模型阻塞或 skip 留在报告中，不能算完成全部功能。
- [ ] 小型金标评估达到预设门槛，报告能力边界和遗漏原因。
- [ ] Redis/Neo4j归属和SQLite现状有准确说明，未丢失数据。
- [ ] 文档、源码、运行中进程与验证证据一致。

交付说明按“已完成 / 未通过或阻塞 / 证据 / 下一步”写。若模型依旧不可用，可以交付已完成部分，但必须明确 **MCP 非生成链路已验证，真实问答尚未验收**，不能宣称整个项目已跑通。

## 15. 可直接复制给执行模型的提示词

> 请先阅读 `E:\GoLandCode\WeKnora\docs\superpowers\plans\2026-10-15-mcp-workbench-handoff.md`，然后从当前两个仓库的未提交工作区继续实施任务0–9，不回滚重做。用户已确认默认账号15918868657@163.com，采用独立正式登录凭证工具，工作台手工输入/导入JWT；Agent对WeKnora的知识业务调用仅走MCP，保留薄验证工作台。重点先修身份切换旧异步结果写回、上传首次并发幂等恢复，随后逐功能真实浏览器测试指定PDF、Wiki/图谱和两条问答链。不要把去重当新解析、空结果当正向命中、模型超时或live跳过当通过。保留所有无关改动、数据卷和真实资料，不泄露秘密，不直改DB绕授权。代码变更先复现再修复，最后交付脱敏证据、评估结果与明确阻塞。
