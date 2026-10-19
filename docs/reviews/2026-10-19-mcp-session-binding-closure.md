# MCP 会话绑定缺口收口记录（2026-10-19）

对应核验报告：`docs/reviews/2026-10-19-mcp-surface-verification.md`（下称"报告"）。本轮按报告 §7"后续范围"执行：只修两个会话绑定缺口（Spec P2、Standards P2），并顺带报告建议的 Standards P3；不做工作台迁移，不增删工具，不改变五工具聚合。

核验起点 HEAD：`508d1e7b2`。

## 1. Spec P2：Agent 自带默认库不再绕过会话范围比较

报告的触发条件：端点未固定知识库范围、发布的自定义 Agent 自带库 B；首问显式选 A 绑定 `[A]`，续问省略 `knowledge_base_ids` 时旧解析器返回 nil（`kbScopeKnown=false`），校验直接跳过比较，原生问答落回 B 而历史仍来自 A。

修法（报告"最小完善"的完整方案，非兜底方案）：`resolveAskKnowledgeBases`（`internal/mcpserver/tools_ask.go`）的 agent-默认分支不再返回 nil，而是把 Agent 自带配置解析成**实际授权后的 canonical 集合**，绑定、比较与转发同源：

- `KBSelectionMode="all"`：取端点可见且经授权的范围，并应用与原生一致的能力过滤（`tools.DeriveKBFilterForAgent`/`KBSatisfiesAgentRequirements`，同 `session_knowledge_qa.go`），所以钉住的集合正是原生检索本会自行解析出的集合。
- `"selected"`/未设置/未知模式：把 `Config.KnowledgeBases` 逐项经 `matchKnowledgeBase` 在授权集合内重解析；调用者读不到的条目被丢弃并 WARN（原样信任会在原生检索内失败），全部不可读时明确报错而不是放行未知范围。
- `RetrieveKBOnlyWhenMentioned`：core ask 没有 @mention 通道，原生在该标志下不检索任何库，因此解析为**确定的空集合**（绑定、不转发），不再是未知。MCP 请求不携带 QuestionOrigin，锚点分支在此面不存在。
- `mode="none"` 与空配置库仍走既有"端点可见全集"回退（此前已记录的有意分歧），未改动。

防线补强（报告"至少拒绝从已有显式绑定切换到未知默认范围"）：`validateAskSessionBinding` 现在在 `kbScopeKnown=false` 且既有绑定含显式集合时拒绝（`core_ask.go`）。修复后所有解析分支都返回已解析集合，该分支常态不可达，是为未来解析器回归准备的 fail-closed 防线。

legacy `/mcp/:id` ask 与本面共用该解析器：省略选择器时 legacy ask 现在也收到显式解析后的范围（与此前"交由原生自行解析"的有效行为对同租户已发布 Agent 结果一致，但响应中 `knowledge_base_ids` 不再为空）。

### 反例转正（`internal/mcpserver/core_ask_plan_test.go`）

- `TestCoreAskAgentOwnedDefaultCannotBypassExplicitBinding`：报告反例（显式 A → 默认 B）。断言 plan 解析出 `[kb-2]`、`kbScopeKnown=true`、续问被拒且拒绝不改写绑定。**补强前对照：在 `508d1e7b2` 的 `core_ask.go`/`tools_ask.go` 上失败**（`Should be true`）。
- `TestCoreAskAgentOwnedDefaultContinuesWhenScopesAgree`：报告要求的正例（默认与显式相同有效范围 → 允许续问同一会话）。
- `TestCoreAskAgentOwnedAllModeIsPinnedToTheVisibleScope`：`all` 模式钉住可见集合；显式 A 后续问被拒、新会话绑定 `[kb-1 kb-2]`。补强前对照失败（续问放行）。

## 2. Standards P2：旧未绑定会话的首次补绑成为原子操作

报告的触发条件：两个并发请求同时读到无绑定旧会话，分别按 A/B 走"先读后写"补绑均成功，后写覆盖前写。

修法（报告的"仓储条件更新"方案——单条 UPDATE 自带"仍未绑定"条件，行级原子，无需事务与行锁）：

- `internal/application/repository/session.go`：新增 `AdoptLastRequestState`，WHERE 追加方言感知的 `unboundBindingCondition`：postgres 用 `agent_config->>'endpoint_id'`；mysql/sqlite 用 `JSON_VALID`/`json_valid` 守卫 + `json_extract`（sqlite 先 CAST AS TEXT，列在 SQLite 下为 jsonb 亲和）。判定规则与 Go 侧读取一致：NULL、空串、非法 JSON、无/空 `endpoint_id` 都算未绑定（Scan 读不出的行本来就是无绑定的旧行）；未知方言直接拒绝补绑（fail-closed），不会带着错误条件写。
- `internal/application/service/session.go`：`AdoptSessionScopeBinding`，校验 state 必须命名 endpoint（CAS 条件的识别键），返回 `(adopted bool, err error)`；零行是协议答案（冲突方），不是失败。
- `internal/mcpserver/core_ask.go`：adoption 路径改走 `adoptAskSessionBinding` → CAS 写入；败方**重读并重验证**实际存的绑定：同范围放行、异范围在写问答消息前拒绝、行消失或仍显示无绑定一律拒绝（重试措辞）。新建会话路径仍用严格 `StoreSessionScopeBinding`，不变。

### 反例转正（`internal/mcpserver/core_ask_adoption_test.go` + `repository/session_test.go`）

- `TestCoreAskConcurrentLegacyAdoptionRefusesDifferentScope`：报告 barrier 探针的正式形态（两次读在汇合点同时观察无绑定快照，写侧串行化等价真实 UPDATE 语义）。断言成功数恰为 1。**补强前对照：旧代码实际 2，失败**。
- `TestCoreAskConcurrentLegacyAdoptionAllowsSameScope`：同范围并发双请求都应放行。
- `TestCoreAskAdoptedSessionIsBoundLikeAFreshOne`：补绑完成后异范围续问立即被拒（INVALID_ARGUMENT）。
- `TestSessionRepositoryAdoptLastRequestState`：仓储层原子性——NULL/UI 记忆/非法 JSON/空 endpoint_id 可收养；已绑定行二次收养零行且不覆盖；跨租户/跨用户/不存在均零行；无 endpoint 的 state 前置拒绝。

## 3. Standards P3：停写守卫按最终执行方法判定

`internal/database/migration_legacy_compat_test.go`：`gormWriterMethods` 移除 `Unscoped`（链修饰符而非执行方法；逐链游走会看到终端方法，`Unscoped().Delete(...)` 仍被抓，`Unscoped().Count(...)` 不再误判）。夹具新增正反例 `unscoped_read.go` / `unscoped_delete.go`。**补强前对照**：把 `Unscoped` 放回写表跑同一测试，4 命中 vs 期望 3（`unscoped_read.go` 被误标），失败；修复后通过。既有报告证据（`standards-scanner-probe.log`）与本报告一致。

## 4. 验证结果与限制

| 项目 | 结果 |
| --- | --- |
| 七包回归 + repository 八包路径（`-tags sqlite_fts5 -timeout=240s -count=1`，skip=LoadFromExcel\|DuckDB） | mcpserver 8.777s、repository 1.288s、service 79.389s、database 41.275s、types 3.815s、router 1.829s、container 2.792s、asynqdl 0.731s 全 ok，退出码 0（`weknora-agent-check:local` 容器，本机无 gcc）。 |
| `go build -tags sqlite_fts5 ./cmd/server` | 通过（产物在容器临时目录，不替换运行服务）。 |
| `go vet` 上述八包 | 通过。 |
| 补强前对照（旧生产代码 + 新测试） | mcpserver 3 项反例红（含 barrier 探针 success=2）、database 守卫夹具红；正例与既有控制在旧代码上即绿，说明新断言精确指向缺口。 |

限制，如实说明：本轮为离线验证。未在活栈重建镜像跑 `core_acceptance.py` 三阶段真实验收（本轮改动不触传输/工具发现面，报告 §6 的验收要求是反例转正 + 七包回归，均已满足）；真实 Postgres 上的 CAS 条件按报告探针同等口径由 sqlite 仓储测试 + 方言条件复核覆盖，`->>`/`JSON_VALID` 分支未经真库回放。跨用户场景未引入新判定：owner 校验顺序不变。

后续若恢复工作台迁移（报告 §3 五步），本报告的两处收口正是迁移前必须稳定的 core `ask` 语义。
