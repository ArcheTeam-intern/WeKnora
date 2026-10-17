# 后端裁剪复审补齐记录（2026-10-17）

对照 [后端裁剪复审与补齐清单](./2026-10-17-backend-pruning-review.md) 的 S1/S2/S3/C1/C2 逐项补齐，
并补上真实验收中发现的一处同类缺陷。本文只记录"改了什么、怎么验证"，原始证据留在本机
`.planning/2026-10-17-backend-review/`（未提交）。

## 一、补齐内容

### S1 上传状态闭环（P1）

- `coreIngestStatus` 优先读上传回执里的 `document_id`（旧字段名 `knowledge_id` 仅作回退）。
- 新增统一归一化 `structuredValue`：legacy handler 会把 `documentStatusReport`、`documentSummary`
  这类**结构体直接塞进 map**，原先的 `.(map[string]any)` 断言拿不到值，导致解析/索引状态恒为
  `unknown`。现在按 MCP 客户端真正看到的 JSON 形状往返一次再读，字段名不再硬碰具体 Go 类型。
- `document_id` 的读取改用同一归一化：`ingest_document.submit` 之前返回空 `document_id`，
  真实验收时发现（属于同一个类型断言缺陷），一并修掉，并加 `documentIDFromImport` 固化两种形状。
- wiki/graph 状态仍然只在知识库维度上报，单文档状态标 `not_reported`，不做推断。

### S2 字面检索的许可（P2）

- `match=literal` 现在要求端点显式具备 `grep_chunks` 许可（文档来源），来源许可（wiki/graph）保持独立。
- 顺带收紧 `match` 取值：只接受空值与 `literal`，其他值在**权限校验之前**以 `INVALID_ARGUMENT` 拒绝，
  不再静默退回普通排序检索。

### S3 会话与有效范围绑定（P2）

- `SessionLastRequestState` 增加 `Mode` / `ProfileID` / `EndpointID` / `AskAgentID`：绑定的是**实际执行的
  配置**（端点 + 应答 agent + 调用方选择的 mode/profile）与**服务端解析出的有效知识库集合**，
  不是请求里的原始参数。
- 现在先解析执行计划（mode/profile → agent；选择器/端点范围/可见集合 → 有效 KB 列表），再校验会话；
  范围、端点、应答配置任一变化都在写入消息之前拒绝。省略选择器与显式传入同一集合等价，顺序变化不误判。
- 新建会话在**执行前**写入绑定（严格写入：仓储错误与零行更新都算失败，失败即拒绝本次问答）；
  升级前遗留的未绑定会话在使用时同样严格补绑，因此"未绑定"只可能是旧版本写下的行，
  不会再被新会话的写入失败冒名顶替。

### C1 Task 5 停写守卫（P2）

- 扫描器重写为 `collectProductionGoFiles`（分别遍历 `internal/` 与 `cmd/`，排除 `migrations/`
  与 `internal/database/testdata`，`_test.go` 不入扫描），错误不再吞掉；断言扫描量 > 100 且
  internal/cmd 都有文件。
- 夹具树验证：临时源码树里带 `db.Exec("INSERT INTO tenant_skills ...")` 必须被标红，
  ORM 读、`_test.go` 写入不得被标红，不可读文件必须让扫描失败。
- 新增 ORM 侧守卫：`RetiredTaskTypes` 之外的退役持久化契约（`MCPOAuthRepository` /
  `MCPToolApprovalRepository` / `MCPToolApprovalService`）只允许出现在接口声明文件里，
  且不得有生产实现（`SaveToken`/`UpsertPolicy` 等退役写方法）。
- 新增"旧记录升级"夹具：预置五张退役表各一行 + tenants/knowledge_bases/knowledges/sessions 核心行，
  跑迁移后再逐项断言内容保留、核心行仍可读写、退役表不再新增行；并静态断言没有任何 up 迁移
  `DROP TABLE` 退役表。
  **范围说明（收窄）**：该用例覆盖的是"当前 schema 带旧数据再跑一次迁移"（第二次 Up 为 NoChange），
  证明的是带数据重启不丢行，**不等于**历史 schema 版本的升级重放；本批没有新增迁移，也没有发现丢行。
  若要正式升级验收，需另补旧版本 schema 夹具。
- ORM 停写守卫（收口复核 T1 后补强）：除"退役契约只许声明、不得有实现"外，新增按表名的写链扫描
  ——`Table("退役表").Create/Save/Updates/Update/Delete/...`（中间可跨 `Where` 等链式调用）以及
  `UPDATE/DELETE FROM/REPLACE INTO <退役表>` 的原始语句。夹具树里 `Table(...).Create(...)` 与
  `Where(...).Updates(...)` 必须被标红、`Table(...).Count(...)` 读操作不得被标红；另在真实生产树里
  植入一条同形写链验证会被拦下（探针树，未提交）。
- 扫描能力边界写进注释：只覆盖静态表名的 SQL 文本与命名式 ORM 写入；运行时拼接表名的写入、
  以及 `TableName()` 返回退役表但字面量不出现在调用点的模型写入，不在扫描范围内。

### C2 旧 memory 队列退役（P2）

- `types.RetiredTaskTypes` 记录退役策略：`memory:extract` 保留声明、队列仍在拓扑内，
  asynq 路径与 Lite `SyncTaskExecutor` 都注册同一个 `drainRetiredTask`——首次投递即完成
  （不重试、不进死信），不解 payload、无任何服务依赖，只记一条带计数的 WARN 日志；
  `DrainedRetiredTaskCount()` 暴露进程内计数。未恢复任何长期记忆逻辑，也没有全局吞未知任务错误。
- 守卫测试：mux 覆盖率（拓扑里每个任务类型要么有处理器、要么在退役表里）、退役任务不产生死信、
  **未知任务类型仍然 `ErrHandlerNotFound` 并照旧进死信**、活跃知识类任务不受 drain 影响。

### 既有测试债务

- `expectedSQLiteMigrationVersion` 33 → 35：常量在 `6c83d7ab4`（2026-09-26）写死，而
  `000034`/`000035` 由 `bd0c5621a`（2026-10-13）带入，**与本轮裁剪无关**，属既有基线债务；
  注释里写明了必须随迁移一起 bump，且刻意不改成"从目录推导"，避免迁移被删除时静默放过。
- gofmt：`74332e1d7` 等本分支自身提交留下了 9 个未格式化文件（多行结构体对齐、单行 if 展开、
  多余空行），一并整理，无逻辑改动。

## 二、验证

### 离线

- `go build -tags sqlite_fts5 ./cmd/server`、`go vet ./internal/... ./cmd/...`：通过。
- 全包 `go test -tags sqlite_fts5 ./internal/...`：改动涉及的包全绿（database 35.6s、router、
  mcpserver、types、container、application/service 82s、asynqdl、chat_pipeline）。
  仍失败的 15 项全部是既知环境/基线问题：14 项 Clash fake-IP（198.18.0.0/15）触发 SSRF 拦截
  （OSS/SSRF/yuque/notion/parity/embedding/utils 等），1 项本机既有断言
  `TestPutTenantParserConfigAdminPreservesRedactedSecrets`。
- 反例验证（探针树，不污染仓库）：在生产代码里塞一条退役表 INSERT → 扫描器报错；
  塞一个 `SaveToken` 实现 → ORM 守卫报错；塞一条 `DROP TABLE tenant_skills` 迁移 → 升级守卫报错。

### 真实五工具 MCP 验收（重建镜像后的活栈，37 项通过 / 1 项环境受限）

- **positive 14/14**：tools/list 恰好 5 个工具；KB 列表/详情（解析引擎目录、docreader 在线）；
  `ingest_document.submit` 返回 ingest_id **与 document_id**；仅凭 ingest_id 轮询到
  `parse=completed, index=completed`（`applied_parser_engine=builtin`，请求规则已应用）；
  document_id 分支等价；未知 ingest_id 明确报错；文档/字面检索命中；
  wiki 检索 10 页、图谱检索 10 条关系；`read_document` 列表/读文档（读回原文精确包含标记串）/
  读 wiki 页（12.5 KB）。
- **ask 7 通过 / 1 受限**：quick-answer 命中已知语料答案（林澈/顾言）并给引用与会话 id；
  同范围续问沿用上下文；**换知识库复用会话被拒**（"bound to a different knowledge_base set"）；
  **换 mode 复用被拒**（"bound to a different mode"）；同一知识库集合换顺序不被误判；
  无答案问题明确回答"知识库没有资料"（无捏造）。smart-reasoning 报 "rerank model is not configured"
  ——工作区只配了 KnowledgeQA/Embedding/VLLM 三个模型，**没有 Rerank 模型**，属环境配置限制，
  与本轮裁剪无关。
- **negative 16/16**：未启用工具按名拒绝（缺 `list_parser_engines` / `create_knowledge_base`）；
  错误端点 token 401、非法用户 token 401；越权知识库选择被点名拒绝；
  畸形 PDF 解析失败可见（`parse=failed` + PDFium 报错）；未配置 wiki 的库给出明确指引；
  临时受限端点（有 `search_knowledge` 无 `grep_chunks`）上：普通检索正常、**literal 被拒**
  （要求 `grep_chunks`）、混合来源被拒（要求 `wiki_search`）、图谱来源被拒、wiki 读被拒；
  非法 `match=fuzzy` 被拒；临时端点用完即删。
- **C2 在线投递**：向活栈 Redis 真投 3 个 `memory:extract` 任务，应用日志出现 3 条
  `drainRetiredTask` 计数 1→2→3；队列 `pending/retry/archived/active` 全为 0、`processed=3`
  ——首次投递即完成，无重试、无死信。

### 验收产物清理

本轮创建的验收文档（acceptance-record/broken.pdf 共 9 个）、其生成的 wiki 摘要页（4 个）与
临时受限端点均已删除；知识库只保留原有语料。

## 三、未做（按用户暂缓）

前端切片、WeKnora-Agent 侧改造不在本轮范围；`smart-reasoning` 依赖的 Rerank 模型仍是环境缺口。

## 四、收口复核后的补强（2026-10-17 第二次）

独立复核报告（`2026-10-17-backend-closure-verification.md`）指出 S3 未收口、默认 profile 回归、
ORM 守卫漏检。以上 S3/C1 段落已按补强后的实现更新，另有两项：

- **V1 默认智能体/profile**：核心面原先把省略的 mode 强填成 `quick-answer` 再转发，导致端点配置的
  默认 agent 与显式 `profile_id` 都被绕过。现在改为先解析执行计划（显式 mode 钉住内置 agent；
  省略 mode 或指定 profile 交给端点配置），并把解析结果同时用于校验、绑定与转发。
- **V3 绑定写入失败**：见上文 S3 第三条——新建会话先绑定后执行，失败不产生可续用会话；
  `sessionService` 新增严格写入 `StoreSessionScopeBinding`（区分仓储错误与零行更新），
  原 `UpdateSessionLastRequestState` 保持"UI 备忘 best-effort"语义不变。
- 复核的四组反例已转为正式测试（`internal/mcpserver/core_ask_plan_test.go`），并做了前后对照：
  同一批测试在补强前的 `core_ask.go`/`tools_ask.go` 上失败 7 项（含四组反例），在补强后全绿。
- T1 的 ORM 守卫补强见上文 C1 段落；植入同形写链的生产树探针验证会被拦下。
