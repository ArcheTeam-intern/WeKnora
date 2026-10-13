# Baseline — 2026-10-13

> 任务 0 工作基线。下一执行模型可对照本文件验证未提交改动、测试结果、容器与版本。

## 1. 代码状态

### WeKnora-Agent 目录裁剪

裁剪已完成，仅保留纯 Agent 边界。

```
WeKnora-Agent/
  .env.example
  .gitignore
  README.md
  agent.py
  main.py
  mcp.json
  requirements.txt
  requirements-dev.txt
  docs/
  tests/
  .venv/                    # 独立 venv，未跟踪
  __pycache__/              # 运行时生成
  .pytest_cache/            # 运行时生成
```

无 backend/、frontend/、deploy/、scripts/、eval/。未引入 FastAPI、Uvicorn、SessionStore、MCPClient 包装、_BudgetedChatModel、证据 Registry、SQLite、multipart 解析、uvicorn 服务。

`agent.py` 仅暴露：
- `READ_ONLY_TOOLS`（13 项只读工具名单）
- `SYSTEM_PROMPT`（提示词）
- `build_agent(model, connections)`（get_tools → 筛选 → create_agent）

`main.py` 职责：
- argparse（`--question`、`--mcp-config`、`--env-file`）
- 加载 env 与 mcp.json，`re.sub` 展开 `${VAR}`
- `ChatOpenAI` 初始化模型
- `agent.ainvoke({"messages": [...]})`，`recursion_limit=25`，`asyncio.timeout(120)`

### WeKnora 仓库未提交修改

来自 `git status --short` 与 `git diff --stat`：

- 18 个文件修改，新增 1009 行，删除 102 行
- 主要分布在：`internal/mcpserver/`、`internal/middleware/`、`internal/types/`、`internal/application/repository/retriever/neo4j/`
- 新增 11 个文件（迁移、scope、upload、graph、status 工具与测试）

不执行 `git reset --hard`、`git clean -fd` 或覆盖文件恢复旧版本。接手人按明确路径增量提交。

### 归档

`E:/GoLandCode/WeKnora-Agent-archive/20261013-105051/` 保留旧 backend、frontend、deploy、scripts、eval、docs、虚拟环境、语料与报告。不删除，不二次移动。

## 2. 测试与依赖

### Agent 单元测试

```
$ ./.venv/Scripts/python.exe -m pytest tests -q
.......                                                                  [100%]
7 passed in 15.22s
```

### 依赖

```
$ ./.venv/Scripts/python.exe -m pip check
No broken requirements found.
```

requirements.txt 锁定五项直接依赖：
- langchain==1.4.4
- langchain-openai==1.7.0
- langchain-mcp-adapters==0.3.2
- mcp==1.30.0
- python-dotenv==1.2.4

### CLI help

```
$ ./.venv/Scripts/python.exe main.py --help
usage: main.py [-h] --question QUESTION [--mcp-config MCP_CONFIG] [--env-file ENV_FILE]
纯 LangChain + WeKnora MCP 问答 Agent
options:
  -h, --help            show this help message and exit
  --question QUESTION   用户问题
  --mcp-config MCP_CONFIG
  --env-file ENV_FILE
```

`--question` 必填。`--mcp-config` 默认 `mcp.json`，`--env-file` 默认 `.env`。

## 3. 容器与镜像

### 现状

| 容器 | 镜像 | 状态 |
| --- | --- | --- |
| WeKnoraAgent-app | weknora-agent-app:local (sha256:8d782da…) | Up 2 hours (healthy) |
| WeKnoraAgent-ui | wechatopenai/weknora-ui:v0.8.2 | Up 2 hours |
| WeKnoraAgent-redis | redis:7.0-alpine | Up 2 hours (healthy) |
| WeKnoraAgent-neo4j | neo4j:2025.10.1 | Up 2 hours (healthy) |
| WeKnora-redis（遗留）| redis:7.0-alpine | Up 2 hours，重复 |

`WeKnoraAgent-app` 即新 WeKnora 服务（来自 `examples/mcp-validation/deploy/Dockerfile.prototype-app`），不是旧 FastAPI backend。容器名沿用历史以保留数据卷与网络。遗留的 `WeKnora-redis` 不在当前 compose 中，将在任务 0 收尾时清理。

### Compose 校验

```
$ docker compose -p weknora-agent-prototype \
    -f examples/mcp-validation/deploy/compose.prototype.yml \
    --env-file examples/mcp-validation/deploy/.env.prototype config --quiet
```

退出码 0，无错误。

## 4. MCP 端点

### 已知端点

`deploy/mcp-endpoints.json` 已写入两个 workspace：

- workspace 1：`e52516ca-6909-4231-93cb-bddcfde4ca5d`，`require_user_identity=true`
- workspace 2：`cc7eb833-82cb-4105-bd79-5ad2ee4a809b`，`require_user_identity=true`

两者的 `Authorization: Bearer …`（端点 token）、`X-WeKnora-User-Token`（用户 JWT）、`X-Tenant-ID`（workspace 数值 ID）、`X-WeKnora-Knowledge-Base-Ids`（1–10 个 UUID 逗号分隔）四个头必须同时存在。

### 端点探测

- 缺任何鉴权头 → `HTTP 401` + `{"code":"UNAUTHORIZED","error":"..."}`
- 端点 token 存在但缺 user token → `{"code":"UNAUTHORIZED","error":"unauthorized: this endpoint requires X-WeKnora-User-Token"}`（用户模式不降级机器身份）

权限与结构化错误契约在任务 2 验证。

## 5. mcp-validation 资产

```
examples/mcp-validation/
  README.md
  deploy/compose.prototype.yml
  deploy/Dockerfile.prototype-app
  scripts/make_corpus.py
  eval/corpus/{doc01..doc10}.md + manifest.json
  eval/questions/demo-12.json
  eval/reports/manual-review.template.json
```

`scripts/mcp_smoke.py`、`import_corpus.py`、`run_eval.py`、`score_eval.py` 当前不存在，按任务 7/8 实现。

`manifest.json` 的真实 `weknora_document_id` 字段当前为空，需要任务 3 导入后回填。不得用 `doc01` 等逻辑 ID 充当服务 UUID。

## 6. 阻塞项

| 信息 | 用途 | 状态 |
| --- | --- | --- |
| 支持工具调用的模型名、base URL、密钥 | 真实 Agent 问答 | 未提供，任务 7 阻塞 |
| 有效的用户 JWT | 真实 MCP 工具调用 | 当前不存 cookie，需登录获取 |
| 真实 WeKnora 镜像重建 | 让本地源码修复生效 | 任务 2/5/6 修复后需 `docker compose build weknora` |

模型、JWT 凭据不写入本文件。

## 7. 任务状态

| 任务 | 状态 |
| --- | --- |
| 0 工作基线 | 本文件 |
| 1 纯 Agent 结构与真实 HTTP 适配验证 | 待开始 |
| 2 WeKnora MCP 权限与结构化错误 | 待开始 |
| 3 文件导入与处理状态 | 待开始 |
| 4 RAG 原文检索与完整性信号 | 待开始 |
| 5 Wiki 核心流程与来源 | 待开始 |
| 6 实体知识图谱真实关系与出处（P0） | 待开始 |
| 7 真实模型的标准 Agent 问答 | 阻塞（缺模型凭据）|
| 8 离线评估 | 待 3/6/7 完成后 |
| 9 前端复用与完整交付 | 待 1–8 完成后 |
