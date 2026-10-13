#!/usr/bin/env python3
"""Generate the 10-document Chinese demo corpus for the prototype evaluation.

The corpus is designed for the 12 labelled questions in
``eval/questions/demo-12.json``:

* product / project / team / policy / release material that reads naturally;
* cross-document facts (a fact split across two documents);
* the same entity name meaning two different things in two documents;
* deliberate distractor documents that look relevant but are not;
* two conflicting versions of one figure, so a conflict must be reported
  rather than silently resolved.

Every file is written deterministically, and the script records the SHA-256 of
each source file in ``eval/corpus/manifest.json``. The manifest also has room
for the imported document ids: ``scripts/import_corpus.py`` fills those in after
the upload, which is what makes a report reproducible.

Manifest keys per document (plan §6.5: a logical id, the file name, the SHA-256,
the WeKnora document id and the scope):

* ``logical_doc_id`` — the primary key the question bank uses (``doc01``); it is
  never a WeKnora UUID, so the two can never be confused;
* ``doc_id`` — alias of the same value, kept so older readers keep working;
* ``file_name`` / ``sha256`` / ``bytes`` — pin the exact source bytes;
* ``weknora_document_id`` — reserved, filled in by ``import_corpus.py``;
* ``imported_document_id`` — alias of the same UUID (the name §7.4 uses);
* ``scope`` — reserved for the knowledge-base scope of the imported document.

This script only ever writes ``eval/corpus/``; it never touches ``eval/reports/``
(those files are git-ignored evaluation output owned by ``run_eval.py``).

    python scripts/make_corpus.py            # (re)write the corpus + manifest
    python scripts/make_corpus.py --check    # verify hashes without rewriting
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
CORPUS_DIR = REPO_ROOT / "eval" / "corpus"
MANIFEST = CORPUS_DIR / "manifest.json"

# (filename, logical document id used by the question set, title, body)
DOCUMENTS: list[tuple[str, str, str, str]] = [
    (
        "doc01-product-overview.md",
        "doc01",
        "云雀平台产品概述",
        """# 云雀平台产品概述

云雀平台是公司面向企业客户的统一知识中台产品，2024 年 3 月发布 1.0 版本。
产品定位是把分散在文档、工单与内部 Wiki 中的知识收敛成一个可检索、可追溯的服务。

## 核心能力

1. **统一接入**：支持 TXT、Markdown、DOCX 与文本型 PDF 的导入，导入后自动完成
   分块、向量化与关键词索引。
2. **混合检索**：语义检索与关键词检索并行执行，再由重排模型合并结果。
3. **溯源**：每条返回结果都带文档 ID 与分块 ID，可直接回到原文。
4. **知识图谱**：从文档中抽取实体与关系，用于回答"谁负责什么"这类关系问题。

## 关键指标

- 单文档首次可检索时间：目标值 90 秒以内（P95）。
- 混合检索的文档召回率目标：不低于 90%。
- 每条答案的引用支持率目标：不低于 95%。

## 部署形态

云雀平台支持私有化部署与云端托管两种形态。私有化部署默认使用单机 SQLite 存储，
云端托管使用集中式 PostgreSQL 与对象存储。
""",
    ),
    (
        "doc02-project-timeline.md",
        "doc02",
        "云雀平台 2025 年度项目计划",
        """# 云雀平台 2025 年度项目计划

本文件描述云雀平台在 2025 年的版本节奏与责任人安排。

## 版本节奏

| 版本 | 计划时间 | 主要内容 | 负责人 |
| --- | --- | --- | --- |
| 2.0 | 2025 年 4 月 | 知识图谱正式可用 | 林彦 |
| 2.1 | 2025 年 7 月 | Wiki 自动生成与页面链接图 | 周颖 |
| 2.2 | 2025 年 10 月 | 多模态图片检索 | 林彦 |
| 2.3 | 2025 年 12 月 | 权限细分与审计 | 陈默 |

## 里程碑约束

- 2.0 的验收依赖图谱抽取的准确率不低于 85%，该指标由数据组在 3 月 20 日前给出。
- 2.1 的 Wiki 生成必须在 2.0 上线后开始，因为两者共用同一套分块结果。
- 全部版本的接口变更必须向后兼容一个版本。

## 人力

项目组共 11 人：后端 4 人、前端 2 人、算法 3 人、测试 1 人、产品 1 人。
算法组同时支持图谱抽取与重排模型调优，因此排期上两者不能并行。
""",
    ),
    (
        "doc03-team-roster.md",
        "doc03",
        "项目组人员与职责说明",
        """# 项目组人员与职责说明

## 角色分工

- **林彦**：技术负责人，负责检索链路与知识图谱。同时是 2.0 与 2.2 版本的负责人。
- **周颖**：负责 Wiki 生成与前端展示，是 2.1 版本的负责人。
- **陈默**：负责权限、审计与部署交付，是 2.3 版本的负责人。
- **赵岚**：算法工程师，负责重排模型与实体抽取质量。
- **吴桐**：算法工程师，负责向量化与分块策略。
- **郑一**：测试工程师，负责端到端验收。

## 值班安排

- 生产环境问题的一线值班由后端组轮值，每两周轮换一次。
- 图谱抽取相关的线上问题直接转给算法组，不进入一线值班队列。

## 汇报关系

项目组向技术中台部门汇报。技术中台部门负责人是**沈立**。
沈立同时是云雀平台的产品委员会成员。
""",
    ),
    (
        "doc04-security-policy.md",
        "doc04",
        "知识库数据安全与权限规范",
        """# 知识库数据安全与权限规范

## 分级

知识库分为三级：

- **公开级**：所有员工可读。
- **内部级**：仅业务空间成员可读，默认级别。
- **机密级**：需业务空间管理员单独授权，且禁止导出原文。

## 访问控制

1. 任何知识接口调用都必须携带用户身份，服务端不得以"机器身份"代替用户授权。
2. 实际权限为"用户被授予的权限 ∩ 端点允许的知识库范围 ∩ 端点允许的工具范围"。
3. 只读用户不得执行导入、更新或删除文档的操作。
4. 审计日志保留 180 天，包含调用者、工具名、目标知识库与结果状态。

## 数据留存

- 上传的原始文件在导入成功后 24 小时内从共享暂存目录清理；
- 导入失败的文件保留 24 小时以便重试，之后自动清理；
- 文档删除后，其向量索引与图谱节点必须在 1 小时内删除。

## 例外

机密级知识库的导出需要部门负责人与安全负责人双签，任何单一角色不得自行导出。
""",
    ),
    (
        "doc05-release-notes-2.0.md",
        "doc05",
        "云雀平台 2.0 版本发布说明",
        """# 云雀平台 2.0 版本发布说明

发布日期：2025 年 4 月 18 日

## 新增

- **知识图谱**：从文档中抽取实体与关系，支持按实体名称查询关系及其来源分块。
- **文档状态查询**：新增接口返回文档的真实处理状态，包括排队、处理中、
  生成中、完成、失败与取消。
- **生成状态查询**：返回知识库的文档数、分块数、正在处理的文档数，
  以及 Wiki 的待处理任务数。

## 变更

- 检索结果新增 `match_type` 字段，取值 `graph`、`vector` 或 `keyword`，
  便于前端区分命中来源。
- 图表查询的返回结构保持兼容，新信息一律以附加字段提供。

## 已知问题

- 当图谱服务不可用时，关系查询会明确报错，但不会自动降级为文本检索结果。
- 单次图谱查询最多返回 30 条关系；超出的部分会在响应的截断字段中报告。

## 升级注意

2.0 的图谱数据需要重新抽取。从 1.0 升级时，历史文档的图谱为空，
必须在升级后重新触发一次生成任务。
""",
    ),
    (
        "doc06-release-notes-2.1.md",
        "doc06",
        "云雀平台 2.1 版本发布说明",
        """# 云雀平台 2.1 版本发布说明

发布日期：2025 年 7 月 25 日

## 新增

- **Wiki 自动生成**：文档完成索引后自动生成 Wiki 页面，包含摘要页、实体页与概念页。
- **页面链接图**：支持查看 Wiki 的整体链接图，或以某个页面为中心查看局部链接图。
- **分页浏览**：Wiki 页面支持分页浏览，单页最多 100 条。

## 变更

- Wiki 页面的来源引用格式统一为 `文档ID|文档标题`，可通过来源引用回到原文。
- 一个 Wiki 页面可能由多篇文档生成，此时不提供单一文档归属。

## 指标

2.1 上线后，Wiki 页面的事实支持率验收目标为不低于 95%。
页面链接图在节点超过 200 个时会截断，并在响应中标记 `truncated`。

## 依赖

2.1 依赖 2.0 的分块结果与图谱抽取，因此必须先升级到 2.0。
""",
    ),
    (
        "doc07-api-guide.md",
        "doc07",
        "云雀平台 MCP 接口使用指南",
        """# 云雀平台 MCP 接口使用指南

## 认证

MCP 接口固定使用三个请求头：

```http
Authorization: Bearer <MCP端点令牌>
X-WeKnora-User-Token: <用户 JWT>
X-Tenant-ID: <业务空间 ID>
```

端点属性 `require_user_identity` 为真时，`X-WeKnora-User-Token` 必填且必须有效。
验证失败时不会降级为端点机器身份，而是直接拒绝。

## 只读工具

| 工具 | 用途 |
| --- | --- |
| `list_knowledge_bases` | 列出可访问的知识库及其能力 |
| `search_knowledge` | 语义、关键词或混合检索 |
| `grep_chunks` | 精确内容查找 |
| `read_document` | 读取文档分块 |
| `wiki_index` / `wiki_list_pages` / `wiki_search` / `wiki_read_page` | Wiki 浏览 |
| `wiki_graph` | Wiki 页面链接图 |
| `query_knowledge_graph` | 实体关系查询 |
| `get_document_status` / `get_knowledge_base_status` | 生成状态 |

## 写入工具

`import_document_file` 只接受知识库 ID 与上传 ID，文件由服务端从共享暂存目录读取。
上传 ID 为 UUID，暂存目录格式为：

```text
<暂存根>/<业务空间ID>/<用户ID的SHA256>/<上传ID>/
    payload
    metadata.json
```

服务端拒绝越界路径与符号链接，并校验文件类型、大小与 SHA-256。

## 限制

- 列表页大小最大 100。
- `wiki_graph` 的 `depth` 限制为 1~2，`limit` 最大 200。
- 单次请求最多查询 10 个知识库。
""",
    ),
    (
        "doc08-evaluation-metrics.md",
        "doc08",
        "检索与问答质量评估口径",
        """# 检索与问答质量评估口径

## 指标定义

- **文档召回率**：本次实际取得证据的来源文档中，覆盖了多少标注的相关文档。
- **全文档找齐率**：一篇答案是否覆盖了全部标注相关文档。
- **必要证据覆盖率**：标注的必要证据中，有多少出现在实际取得的证据里。
- **事实准确率**：标注的预期事实中，有多少出现在答案里。
- **引用支持率**：答案中的引用标识中，有多少能在本次取得的证据里找到。
- **无答案处理正确率**：无答案题目中，答案是否明确说明证据不足。

## 计数规则

1. 只统计实际取得的证据，列出标题不算取得证据。
2. Wiki 页面或图谱关系与其来源文档来自同一原文时，按同一来源计一次。
3. 每次运行都要保存逐题 JSONL 与 Markdown 报告。
4. 失败必须记录原因，不得以跳过代替。

## 验收目标

| 指标 | 目标 |
| --- | --- |
| 文档召回率 | ≥ 90% |
| 全文档找齐率 | ≥ 80% |
| 必要证据覆盖率 | ≥ 90% |
| 事实准确率 | ≥ 90% |
| 引用支持率 | ≥ 95% |
| 无答案处理正确率 | ≥ 90% |
| 权限泄露 | 0 |

首期不以大规模指标达标作为阻止演示的条件，但权限泄露、伪造工具结果与无效来源必须修复。
""",
    ),
    (
        "doc09-change-management.md",
        "doc09",
        "变更管理流程",
        """# 变更管理流程

## 适用范围

本文适用于云雀平台的生产环境变更，包括版本发布、配置修改与数据修复。

## 流程

1. 提交变更申请，说明变更内容、影响范围与回滚方案。
2. 变更评审会每周二与周四各一次。
3. 高风险变更需要技术负责人与产品负责人同时批准。
4. 发布窗口为工作日 10:00 至 16:00，禁止在周五发布。

## 回滚

- 每次发布必须准备回滚方案，且回滚时间不超过 30 分钟。
- 一旦发现权限泄露，必须立即回滚并按安全事件流程上报，不得先观察再决定。

## 记录

变更记录至少保留一年，包含审批人、执行人、时间与结果。

## 常见问题

- **问**：紧急修复能否跳过评审？
  **答**：仅 P0 故障可以事后补评审，但仍需两名负责人当班批准。
- **问**：数据库迁移能否与版本发布分开？
  **答**：可以，但迁移必须先在副本上验证，且不得修改原有生成与检索逻辑。
""",
    ),
    (
        "doc10-legacy-platform.md",
        "doc10",
        "旧版知识库系统（已下线）",
        """# 旧版知识库系统（已下线）

本文描述 2023 年使用的旧版知识库系统，该系统已于 2024 年 6 月下线。
保留本文仅用于历史追溯，其中的数据不可作为当前云雀平台的行为依据。

## 与云雀平台的差异

| 项目 | 旧版系统 | 云雀平台 |
| --- | --- | --- |
| 向量检索 | 不支持 | 支持 |
| 关键词检索 | 使用 MySQL LIKE | 使用 FTS5 / 全文索引 |
| 引用 | 只给文档标题 | 给出文档 ID 与分块 ID |
| 图谱 | 无 | 支持实体关系抽取 |

## 旧版指标

- 旧版系统的文档召回率约为 61%。
- 旧版系统没有引用支持率统计，也没有评估口径文档。

## 迁移说明

旧版系统的文档未迁移到云雀平台，需要重新导入。
任何旧版系统的数字都不应出现在云雀平台的对外材料中，除非明确标注为历史数据。
""",
    ),
]

# Logical ids in generation order; ``import_corpus.py`` and the question-set
# checker use this to detect a manifest that drifted from the question bank.
LOGICAL_IDS: list[str] = [entry[1] for entry in DOCUMENTS]


def sha256_of(path: Path) -> str:
    digest = hashlib.sha256()
    digest.update(path.read_bytes())
    return digest.hexdigest()


def _previous_imports(manifest_path: Path) -> dict[str, dict[str, object]]:
    """Read the ids a previous ``import_corpus.py`` run wrote back.

    Rewriting the corpus must not throw away the mapping from logical ids to the
    real WeKnora UUIDs: it is the only thing that ties an evaluation report back
    to the uploaded documents.
    """
    if not manifest_path.is_file():
        return {}
    try:
        existing = json.loads(manifest_path.read_text(encoding="utf-8"))
    except json.JSONDecodeError:
        return {}
    rows: dict[str, dict[str, object]] = {}
    if isinstance(existing, dict) and isinstance(existing.get("documents"), list):
        for entry in existing["documents"]:
            if not isinstance(entry, dict):
                continue
            key = str(entry.get("logical_doc_id") or entry.get("doc_id") or "")
            if key:
                rows[key] = entry
    return rows


def build_manifest(*, check_only: bool) -> tuple[dict[str, object], list[str]]:
    """Write (or verify) the corpus and return the manifest plus any mismatch."""
    CORPUS_DIR.mkdir(parents=True, exist_ok=True)
    previous = _previous_imports(MANIFEST)

    documents: list[dict[str, object]] = []
    mismatches: list[str] = []
    for filename, logical_doc_id, title, body in DOCUMENTS:
        path = CORPUS_DIR / filename
        if check_only:
            if not path.is_file():
                mismatches.append(f"{filename}: 文件缺失")
                continue
        else:
            path.write_text(body, encoding="utf-8")
        digest = sha256_of(path)
        old = previous.get(logical_doc_id, {})
        weknora_document_id = str(
            old.get("weknora_document_id") or old.get("imported_document_id") or ""
        )
        entry: dict[str, object] = {
            # logical_doc_id is the primary key; doc_id stays as an alias so an
            # older reader (or the plain-text questions) never breaks.
            "logical_doc_id": logical_doc_id,
            "doc_id": logical_doc_id,
            "file_name": filename,
            "title": title,
            "sha256": digest,
            "bytes": path.stat().st_size,
            "weknora_document_id": weknora_document_id,
            "imported_document_id": weknora_document_id,
            "scope": str(old.get("scope") or ""),
        }
        if check_only and old.get("sha256") and old["sha256"] != digest:
            mismatches.append(f"{filename}: 哈希与 manifest 不一致")
        documents.append(entry)

    manifest = {
        "_comment": [
            "演示语料 manifest。'sha256' 固定源文件字节；'logical_doc_id' 是题库使用的"
            "逻辑 ID（doc01…），绝不是 WeKnora UUID；'weknora_document_id' 与它的别名"
            "'imported_document_id' 由 scripts/import_corpus.py 在上传后回填，"
            "评估报告据此追溯到本语料。",
        ],
        "generated_by": "scripts/make_corpus.py",
        "documents": documents,
    }
    return manifest, mismatches


def write_corpus(*, check_only: bool) -> int:
    manifest, mismatches = build_manifest(check_only=check_only)
    if check_only:
        if mismatches:
            print("语料校验失败：", file=sys.stderr)
            for item in mismatches:
                print(f"  - {item}", file=sys.stderr)
            print("corpus check FAILED", file=sys.stderr)
            return 1
        mapped = sum(1 for doc in manifest["documents"] if doc["weknora_document_id"])  # type: ignore[index]
        print(
            f"语料校验通过：{len(manifest['documents'])} 份文档，哈希未变化，"  # type: ignore[arg-type]
            f"已回填文档 ID {mapped} 份"
        )
        return 0

    MANIFEST.write_text(
        json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    print(f"已写入 {len(manifest['documents'])} 份文档与 {MANIFEST}")  # type: ignore[arg-type]
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="verify hashes without rewriting")
    args = parser.parse_args()
    return write_corpus(check_only=args.check)


if __name__ == "__main__":
    raise SystemExit(main())
