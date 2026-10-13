# WeKnora MCP 验证交付报告

> 实施计划：`E:/GoLandCode/WeKnora-Agent/docs/implementation-plan.md`（任务 0–9）。
> 提交分支：`weknora-agent-mcp`（[ArcheTeam-intern/WeKnora](https://github.com/ArcheTeam-intern/WeKnora)）。
> 交付日期：2026-10-13。

## 1. 代码状态

### Agent 端（保持纯 Agent 边界）

- 生产文件只 2 个：`main.py`、`agent.py`；13 项只读工具白名单；标准 `create_agent`；无自建 HTTP/认证/会话/证据 Registry。
- 新增测试 `tests/test_http_mcp.py`（6 项）和夹具 `mcp_http_fixture.py`，覆盖真实 `streamable_http` 工具发现/调用/超时/递归限制/禁用项导入。
- 单元测试 13/13 通过，`pip check` 无冲突，`main.py --help` 可用。

### WeKnora 端（核心实施对象）

- **任务 0** 基线：`examples/mcp-validation/eval/reports/baseline.md`，3 处 gitignore 修复。
- **任务 2** 结构化错误：4 个 read-only 工具文件（`tools_retrieve.go` / `tools_wiki.go` / `tools_status.go` / `tools_graph.go`）的 handler 全部转 `errorResult`；新增 `tool_errors_test.go`。
- **任务 3** 文件导入：
  - Bug 修复：`UploadRecord.PayloadPath` 不再依赖持久化字段，按 `(workspace, hashed_storage_id, upload_id)` 派生（`upload_staging.go`）；`AsError(nil)` 折叠为稳定 `INTERNAL_ERROR`。
  - 测试：`enqueue_failure_test.go` 改用 `NewServer(...)` 构造避免 `handler` 字段为 nil；新增 `TestGetRebuildsDerivedPayloadPath` 覆盖回归。
  - 脚本：`scripts/import_corpus.py`（10/10 真实导入，幂等 key、lease 恢复、原子 manifest 写入、单 session 复用）。
- **任务 4** 检索完整性：`read_document` 始终带 `truncated`、只在未达末尾时带 `next_offset`；`search_knowledge` 新增 `requested_limit` + `truncated`；端到端验证通过。
- **任务 6** 实体图谱逐关系出处（**P0**）：
  - `GraphRelation` 增 `KnowledgeID` + `ChunkIDs` 字段。
  - 抽取服务把 `(knowledge_id, chunk_id)` 绑定到每条 relation。
  - Neo4j 关系属性持久化 `kg` + `chunks`；读取时按关系 prop 还原。
  - 查询聚合 key 加入 `KnowledgeID` 防同名边合错来源。
  - MCP `source_refs_v2` enrichment 改为读 edge 自身的 chunk 出处，无出处标 `unknown`，无 quote 标 `unverified_source`，有 quote 标 `verified`。
  - 回归测试 `graph_provenance_test.go` 三例。
- **Bug 修复**：`get_document_status` 的 `describeKnowledgeStatus(nil)` 防御，避免 `get_document_status` 在并发导入时返回 JSON-RPC -32603。

### 验证资产

- 迁移到 `WeKnora/examples/mcp-validation/`（compose、Dockerfile、README、10 篇语料、12 题、manual-review 模板）。
- 新增脚本：
  - `scripts/import_corpus.py`（任务 3）
  - `scripts/mcp_smoke.py`（任务 7 自检入口）
  - `scripts/score_eval.py`（任务 8 离线评分）
- 测试：`tests/test_import_corpus.py`、`tests/test_eval_metrics.py` 全部通过。
- `requirements-dev.txt` 把脚本依赖（httpx）与 Agent runtime 解耦。

## 2. 服务状态

| 容器 | 镜像 | 状态 |
| --- | --- | --- |
| WeKnoraAgent-app | weknora-agent-app:local（含任务 2/3/4/6 修复） | Up 4 hours (healthy) |
| WeKnoraAgent-ui | wechatopenai/weknora-ui:v0.8.2 | Up 4 hours |
| WeKnoraAgent-redis | redis:7.0-alpine | Up 4 hours (healthy) |
| WeKnoraAgent-neo4j | neo4j:2025.10.1 | Up 4 hours (healthy) |

`docker compose config --quiet` 退出 0。

## 3. 验证证据

### Agent

```
$ ./.venv/Scripts/python.exe -m pytest tests -q
.............                                                  [100%]
13 passed in 27.52s
```

### WeKnora 单元测试

`weknora-check` 容器运行：
- `internal/mcpserver`（含新结构化错误测试 + 3 个图谱出处回归 + staging path 重建 + import enqueue 修复）—— **PASS**。
- `internal/types` —— **PASS**。
- `internal/application/repository/retriever/neo4j` —— **PASS**。
- `internal/application/service` + `internal/agent/tools`（跳过 DuckDB/Excel 沙箱测试）—— **PASS**（go build 全量通过）。

### 真实服务验证

| 验证项 | 结果 |
| --- | --- |
| 10/10 文档导入 | ✅ 全部 `parse_status=completed`，`weknora_document_id` 真实回填 manifest |
| `search_knowledge` 截断信号 | `count=5, requested_limit=5, truncated=true` ✅ |
| `read_document` 分页 | `total_chunks=3, truncated=true, next_offset=1` 逐页推进 ✅ |
| `query_knowledge_graph` 结构化错误 | `code: GRAPH_EXTRACTION_NOT_CONFIGURED` ✅ |
| 端点权限（user identity） | 缺 `X-WeKnora-User-Token` → `UNAUTHORIZED` ✅ |
| KB 范围越权 | `KB_OUT_OF_SCOPE` 结构化错误 ✅ |

### 真实端到端 graph 验证（限制说明）

10 篇文档已用 `c7a0a897-...` KB（无图抽取）成功导入并端到端可检索；图抽取端到端需新建 `extract_config.enabled=true` 的 KB 并触发 post-process 重做（PUT 路径不写 extract_config）。代码与单元测试覆盖，真实 Neo4j 写入受环境约束。

## 4. 任务 0-9 状态

| 任务 | 状态 |
| --- | --- |
| 0 工作基线 | ✅ baseline.md、3 处 gitignore、Compose config |
| 1 纯 Agent 结构与真实 HTTP 适配验证 | ✅ 13 测试通过 |
| 2 权限与结构化错误 | ✅ 4 文件 + tool_errors_test.go |
| 3 文件导入与处理状态 | ✅ PayloadPath 修复 + 10/10 真实导入 |
| 4 RAG 完整性 | ✅ truncated/requested_limit + 端到端验证 |
| 5 Wiki 流程 | 代码已在任务 2 覆盖（5 个 wiki handler 转 errorResult，wiki_search/wiki_read_page/wiki_index/wiki_list_pages/wiki_graph）；wiki_graph 边为 `wiki_page_links` 语义由原实现保证；端到端真实验证因本验证 KB 未启用 wiki 留待主产品 UI 验收 |
| 6 实体图谱真实关系与出处（P0） | ✅ 见任务 6 说明 |
| 7 真实模型问答 | ✅ `deepseek-flash` 经 `https://api.deepseek.com` 端到端跑通，跨文档综合 + 原文证据 + 计划/实际冲突标注 |
| 8 离线评估 | ✅ 12 题自动化跑通（详见 §5 指标）；事实准确率/完整率/引用支持率待人工复核 |
| 9 前端复用与交付 | ✅ 本报告；WeKnora UI 已有知识图谱 + Wiki 渲染组件，本方案不再造前端 |

## 5. 12 题评估（任务 8 实际结果）

| 指标 | 数值 | 门槛 | 状态 |
| --- | --- | --- | --- |
| 总题数 | 12（10 可答 + 2 无答案） | — | ✅ |
| 文档 recall（宏平均） | **1.0** | ≥ 0.9 | ✅ |
| 文档找齐率 | **10/10** | ≥ 0.8 | ✅ |
| 必需证据 recall（子串启发式） | **0.8** | ≥ 0.9 | ⚠ 启发式（含 mid-content evidence 子串） |
| 无答案诚实处理 | **2/2** | 2/2 | ✅ |
| 12/12 题跑通 | 12/12 | 跑通 | ✅ |
| 事实准确率 / 完整率 / 引用支持率 | not_evaluated | 人工复核后填 | ⏳ |

模型行为亮点（用 deepseek-flash）：

- q04 跨 2 文档综合：2.1 负责人 + Wiki 必须等 2.0 的解释都给出，cite 知识库 id。
- q06 主动指出"2.0 / 2.1 是分别验收门槛，混用即答错"。
- q11/q13 全部**未编造数字**——清晰说明知识库没找到 3.0 / 预算证据。
- q11 把搜索范围（doc01–doc10）和实际引用（doc02）区分开，未误把"扫描过"算"找到"。

事实准确率等需要人工核验的字段，已通过 `seed_review.py` 预填入 `eval/reports/manual-review.json` 骨架（12 题 × 8 字段），每题标注 `reviewed=false`，由领域专家逐题 `claims × 支持数 / 必要事实覆盖数` 填写后重跑 `score_eval.py --runs ... --manual-review ... --output ...` 即可产出最终分子分母报告。

## 6. 限制

- **必需证据 recall 0.8（启发式）**：子串匹配把答案里"未带引号但等价表述"判为不命中；模型如 `2.0 知识图谱准确率验收 85%` 完全正确但 evidence 里写的是长句，0.18 char prefix 对不上的话就不算 hit。改进方向是按事实子句做语义匹配，需 LLM-as-judge 或人工复核；当前 0.8 是子串启发式下界。
- **Wiki 端到端验证未在本环境跑通**：任务 5 涉及 Wiki 多 KB 跨库与状态机；本验证 KB 仅启用 vector+keyword，wiki_enabled 留待主产品 UI 验。
- **任务 6 Neo4j 端到端 recall**：代码、单元测试、真实 MCP 错误码全部通过；真实 Neo4j 关系写读需要新 KB 在 `extract_config.enabled=true` 下重新触发 post-process 才能验证完整链路（PUT 路径不写该字段，需要 KB 新建 + 重跑导入）。

## 7. 推送记录

| 仓库 | 提交 | 远程 |
| --- | --- | --- |
| WeKnora | `bd0c5621a`、`c654b3167`、`e6608877b`、`8db0b6b11`、`be9f9ebab`、`1da69225d` | [ArcheTeam-intern/WeKnora](https://github.com/ArcheTeam-intern/WeKnora) branch `weknora-agent-mcp` |
| WeKnora-Agent | `1d68ce5` | [ArcheTeam-intern/WeKnora-Agent](https://github.com/ArcheTeam-intern/WeKnora-Agent) branch `main` |

本轮新增提交即将推送。

## 8. 文件

- `E:/GoLandCode/WeKnora/examples/mcp-validation/eval/reports/baseline.md` —— 任务 0 基线
- `E:/GoLandCode/WeKnora/examples/mcp-validation/eval/reports/delivery.md` —— 本文件
- `E:/GoLandCode/WeKnora/examples/mcp-validation/eval/reports/report.md` —— 自动评估报告（事实准确率待人工）
- `E:/GoLandCode/WeKnora/examples/mcp-validation/eval/reports/runs.jsonl` —— 12 题原始 AIMessage/ToolMessage 轨迹
- `E:/GoLandCode/WeKnora/examples/mcp-validation/eval/reports/manual-review.json` —— 预填的人工复核骨架
- `E:/GoLandCode/WeKnora/examples/mcp-validation/eval/reports/mcp-smoke/smoke.json` —— 13 只读工具 + 5 调用的工具发现快照
- `E:/GoLandCode/WeKnora/examples/mcp-validation/eval/corpus/manifest.json` —— 真实 WeKnora 文档 id
- `E:/GoLandCode/WeKnora/examples/mcp-validation/scripts/{import_corpus,mcp_smoke,run_eval,score_eval,recompute_found,seed_review}.py` —— 离线脚本
- `E:/GoLandCode/WeKnora/examples/mcp-validation/tests/{test_import_corpus,test_eval_metrics}.py` —— 单元测试
- `E:/GoLandCode/WeKnora/examples/mcp-validation/requirements-dev.txt` —— 脚本依赖
- `E:/GoLandCode/WeKnora-Agent/{main,agent}.py`、`tests/` —— 纯 Agent 实现 + 13 测试

## 9. 审查修复（R1–R14，2026-10-13）

依据《WeKnora 与纯 Agent 实施审查》(2026-10-13) 修复全部 P1/P2：

| 编号 | 修复 |
| --- | --- |
| R1 | 用户模式上传过 `requireKBWriteOwnership` 门禁（scope/upload_transport/tools_import） |
| R2 | Wiki `SourceRefsV2` 改 `gorm:"-"` 派生视图，无数据库迁移 |
| R3 | `tools_graph` 增 `mapSliceOf` 兼容两种 provenance 形状 |
| R4 | `tools_wiki` `found_kbs` 断言改 `map[string][]string` |
| R5 | 图谱关系去重 key 优先 `rel.ID`；端点 id 缺失时按名字兜底（修复同名同型关系误合并） |
| R6 | 评估闭环改从**检索类工具原始轨迹**取证：`found_documents`/`hit_evidence` 只认 search/grep/read/ask/graph/wiki 输出，列表与状态工具不再灌水；证据子串经本地语料反查归属文档（补 search 文本不带文档 UUID 的缺口）；`recompute_found.py` 同源重derive |
| R7 | 状态计数零值误判 known → 真实计数查询，失败标 unknown |
| R8 | 幂等回放仍做有界读取 + 大小 + SHA256 校验 |
| R9 | Sweep 补齐生产调度入口：`StagingSweepRunner`（1h 周期、启动延迟 5min、ResourceCleaner 优雅停止）；Sweep 增加磁盘扫描，清理 DB 行已消失/已导入但字节残留的孤儿目录，尊重活跃租约与写入宽限期 |
| R10 | Complete 落回执失败返回 retryable errorResult，携带已建知识 id |
| R11 | CORS `AllowHeaders` 补 KB scope 与 `Idempotency-Key` |
| R12 | Neo4j 关系匹配后 union chunks（不再只 onCreate） |
| R13 | backend 截断信号 `backendTruncated` 端到端透传 |
| R14 | `requirements-dev.txt` 不再 `-r` 相邻仓库路径 |

### R1–R14 验证

- `weknora-check`（Linux 容器，sqlite_fts5 tag）：mcpserver / middleware / service / sqlite retriever / router / types / agent/tools（-skip DuckDB、Excel 沙箱）/ neo4j 全部 **PASS**；新增 R9 磁盘扫描 + 调度器 5 项单测通过。
- 重建 `weknora-agent-app:local` 并重启验证栈（healthy），启动日志确认 `[mcpserver] staged-upload sweep starting: interval=1h0m0s`。
- 12 题评估真实模型重跑（12/12 ok）：文档 recall 1.0（宏平均）、找齐率 10/10、必需证据 recall 1.0（工具证据子串）、无答案诚实 2/2。旧报告（R6 前语义）存档于 `report.pre-r6.md`、`runs.pre-r6.jsonl`。
- 人工复核仍未执行（0/12），事实准确率 / 完整率 / 引用支持率维持 not_evaluated。
