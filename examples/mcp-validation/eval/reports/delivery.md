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
| 7 真实模型问答 | 阻塞于**无模型凭据**（`AGENT_MODEL` / `AGENT_MODEL_BASE_URL` / `AGENT_MODEL_API_KEY` 未提供）。`mcp_smoke.py` 入口已就绪，提供任一模型后可直接跑 |
| 8 离线评估 | 核心脚本就绪（`import_corpus.py`、`mcp_smoke.py`、`score_eval.py` + 单测）。12 题人工核验需配合任务 7 一起做；run_eval.py 待任务 7 跑通后照同样的原生 SDK + script 范式补齐 |
| 9 前端复用与交付 | 本报告就是任务 9 的交付物；WeKnora UI 已有知识图谱 + Wiki 渲染组件，本方案不再造前端 |

## 5. 限制

- **真实模型问答未跑**：缺模型凭据；提供后 5 分钟内可用 `mcp_smoke.py` 和 `main.py --question` 触发。
- **Wiki 端到端验证未在本环境跑通**：任务 5 涉及 Wiki 多 KB 跨库与状态机；本验证 KB 仅启用 vector+keyword，wiki_enabled 留待主产品 UI 验。
- **任务 8 评估报告留 `not_evaluated`**：人工核验需业务领域专家；脚本与度量已交付（`document_metrics`/`evidence_metrics`/`fact_metrics` 等），加 12 题结果即可产出分子分母报告。

## 6. 推送记录

| 仓库 | 提交 | 远程 |
| --- | --- | --- |
| WeKnora | `bd0c5621a`、`c654b3167`、`e6608877b`、`8db0b6b11`、`be9f9ebab` | [ArcheTeam-intern/WeKnora](https://github.com/ArcheTeam-intern/WeKnora) branch `weknora-agent-mcp` |
| WeKnora-Agent | `1d68ce5` | [ArcheTeam-intern/WeKnora-Agent](https://github.com/ArcheTeam-intern/WeKnora-Agent) branch `main` |

## 7. 文件

- `E:/GoLandCode/WeKnora/examples/mcp-validation/eval/reports/baseline.md` —— 任务 0 基线
- `E:/GoLandCode/WeKnora/examples/mcp-validation/eval/reports/delivery.md` —— 本文件
- `E:/GoLandCode/WeKnora/examples/mcp-validation/eval/corpus/manifest.json` —— 真实 WeKnora 文档 id
- `E:/GoLandCode/WeKnora/examples/mcp-validation/scripts/{import_corpus,mcp_smoke,score_eval}.py` —— 离线脚本
- `E:/GoLandCode/WeKnora/examples/mcp-validation/tests/{test_import_corpus,test_eval_metrics}.py` —— 单元测试
- `E:/GoLandCode/WeKnora/examples/mcp-validation/requirements-dev.txt` —— 脚本依赖
- `E:/GoLandCode/WeKnora-Agent/{main,agent}.py`、`tests/` —— 纯 Agent 实现 + 13 测试
