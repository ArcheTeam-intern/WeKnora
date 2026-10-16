# WeKnora 主产品 MCP 五工具集成与安全裁剪方案

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将主产品消费的 WeKnora MCP 收敛为一个业务端点、五个工具，完整保留知识库、PDF解析、检索、原生问答、Wiki/图谱与来源核验；按依赖顺序删除非核心产品能力。

**Architecture:** 继续使用 WeKnora 原生 Go MCP Server，聚合已存在的知识工作流；主产品不重建 RAG、不访问 WeKnora 数据库、不通过 Agent 绕行 REST 业务接口。五个公开工具背后保留必要的内部模块、权限、任务及状态机，非核心代码经过入口关闭、依赖解耦、回归后再物理删除。

**Tech Stack:** Go/Gin/dig/mcp-go、SQLite或PostgreSQL、索引/embedding/rerank、Redis任务、docreader、可选Neo4j；主产品标准MCP客户端；独立Python/LangChain验证程序。

---

## 1. 范围与本次结论

日期：**2026-10-16，北京时间**。本文是待执行方案，本轮没有实施删除或修改生产代码。

本地核查基线：

| 项目 | 基线 |
|---|---|
| WeKnora | `E:\GoLandCode\WeKnora`，HEAD `bea1d33ee`，remote是ArcheTeam-intern/WeKnora |
| WeKnora-Agent | `E:\GoLandCode\WeKnora-Agent`，HEAD `9b0d3d3` |
| 腾讯官方 | Tencent/WeKnora，main本轮核验为 `2fbfb4471581cd5e7d731bc7a2d443fe7d8e95b5` |

本地已完成此前缺失的解析器发现/选择、原生问答模式、知识库新建和范围拆分等修改。**不要继续按10月15日旧审查把这些功能当作未实现。** 当前Go注册目录是22项，不是旧文档的20/21项；某个端点实际可见数量仍由其白名单决定。

本次重新执行：相关MCP测试（模式、解析器、解析配置、建库及图谱配置）通过。具体命令见第10节；没有重新执行全部真实模型/浏览器矩阵，因此“代码具备”与“全部场景已验收”分开陈述。

同时重跑Agent离线测试为60 passed / 1 skipped，Token与页面状态Node测试22 passed。跳过项为live验证，不能计入真实业务通过；这也不证明未来裁剪后的版本已经安全。

用户要保留：知识库管理、PDF上传/解析选项、状态、问答与检索、原文引用、Wiki、知识图谱、知识型智能体配置。用户不需要：消息聚合产品、跨会话长期记忆、IM/嵌入发布、公开网络搜索、智能体执行沙箱及相关配置。

## 2. 腾讯官方实际怎样做

### 2.1 官方没有“只有4个MCP工具”这回事

| 官方实现 | 实际数量 | 关系与适用性 |
|---|---:|---|
| Go原生 `/mcp/<endpoint_id>` | **12工具，4分组** | 推荐参照；分组不改变tools/list数量 |
| 旧Python `mcp-server/` | **31工具** | 已弃用；多为REST逐项映射，不作为本次方向 |
| CLI `weknora mcp serve` | **10工具** | 精选stdio工具面，不是后端全量接口镜像 |
| DeepSeek Harness插件 | **4工具** | 原生插件经REST调用，**不是MCP端点**；只能借鉴能力分工 |

来源分别为[官方Go注册表](https://github.com/Tencent/WeKnora/blob/2fbfb4471581cd5e7d731bc7a2d443fe7d8e95b5/internal/mcpserver/server.go#L204-L223)、[Python弃用声明及实现](https://github.com/Tencent/WeKnora/blob/2fbfb4471581cd5e7d731bc7a2d443fe7d8e95b5/mcp-server/README.md)、[CLI精选工具](https://github.com/Tencent/WeKnora/blob/2fbfb4471581cd5e7d731bc7a2d443fe7d8e95b5/cli/internal/mcp/tools.go)、[Harness工具注册](https://github.com/Tencent/WeKnora/blob/2fbfb4471581cd5e7d731bc7a2d443fe7d8e95b5/packages/dsh-weknora/src/tools.ts)。

Go原生四组是：

- 检索阅读：list_knowledge_bases、search_knowledge、grep_chunks、list_documents、read_document。
- 问答：ask。
- Wiki：wiki_search、wiki_read_page、wiki_index。
- 写入：add_document、update_document、delete_document。

这些是真实独立工具，不是四个带任意action的总入口。[官方目录定义](https://github.com/Tencent/WeKnora/blob/2fbfb4471581cd5e7d731bc7a2d443fe7d8e95b5/internal/types/mcp_endpoint_tools.go#L10-L64)

### 2.2 可以参考的“合并”

官方 `search_knowledge.mode` 合并语义/关键词/混合检索；`ask` 合并创建或复用会话、原生问答和引用收集，续问只需回传session_id。原生ask只运行端点配置的Agent，避免调用方任意指定内部高权限Agent。[检索参数](https://github.com/Tencent/WeKnora/blob/2fbfb4471581cd5e7d731bc7a2d443fe7d8e95b5/internal/mcpserver/tools_retrieve.go)、[ask实现](https://github.com/Tencent/WeKnora/blob/2fbfb4471581cd5e7d731bc7a2d443fe7d8e95b5/internal/mcpserver/tools_ask.go#L26-L180)

原生add_document提供Markdown/URL写入，**没有二进制PDF上传、解析器选择和本地新增的完整状态契约**。因此不能原样复制官方12工具再删到5个，否则会删掉用户最需要的上传流程。[官方写入schema](https://github.com/Tencent/WeKnora/blob/2fbfb4471581cd5e7d731bc7a2d443fe7d8e95b5/internal/mcpserver/tools_ingest.go#L12-L103)

### 2.3 本方案取舍

| 方案 | 优点 | 问题 | 结论 |
|---|---|---|---|
| 原生工具只勾5项 | 改动小，最接近腾讯 | 本地上传/导入/状态/发现/建库与检索阅读问答无法同时覆盖 | 不能满足本项目完整主流程 |
| **五个有明确职责的业务工具** | 保留能力，调用方不处理碎片流程 | 要补聚合、动作权限和状态回归 | **采用** |
| 单个execute加任意action/path | 表面最少 | schema模糊、权限易放大、把内部实现暴露给模型 | 不采用 |

“一个业务端点+五工具”是本项目定制设计，不能宣传成腾讯官方就是五个。

## 3. 主产品的五工具契约

拟新增的核心工具面称 `core-v1`；本节是**目标设计，不是当前已有接口**。

| 工具 | 职责 | 合并现有能力 |
|---|---|---|
| `knowledge_base` | `action=list|get|create`；库及安全能力发现 | list/create KB、KB状态、解析器与可发布问答配置发现 |
| `ingest_document` | `action=submit|status`；PDF传输、解析入队和统一状态 | upload_file、import_document_file、upload/document状态 |
| `search_knowledge` | 证据检索，明确区分检索模式与资料来源 | 原文检索、字面查找、Wiki搜索、实体关系检索 |
| `read_document` | 文档/Wiki原文回读与引用核验 | read_document、list_documents、Wiki目录与读页 |
| `ask` | 原生快问/知识推理、受控Wiki融合策略、引用和续问 | 现有ask及原生QA服务，不重建LangChain后端 |

这不是删除其余17个内部函数。先让主产品只发现五个，再去掉旧对外目录和失去用途的适配代码；内部检索、分块、任务、授权、Wiki读写生成仍各司其职。

### 3.1 `knowledge_base`

有限动作：

- list：可见库的分页目录，返回能力和ready状态；不能用“开关=true”代替可用。
- get：指定库详情、文档/索引/Wiki/图谱状态，以及本调用方可用的解析器和问答profile。
- create：用户主动建库，复用现有角色、模型默认继承、索引配置和图谱类型校验；不提供任意模型密钥/服务地址设置。

```json
{"action":"get","knowledge_base_id":"kb-A","include_capabilities":true}
```

能力返回至少含：parser名称/支持文件类型/available/不可用原因、允许的mode与发布profile、Wiki enabled/ready、graph enabled/graph_ready、模型配置是否满足入库前提。不得返回模型密钥、Neo4j凭据或docreader内部地址。

创建时：显式模型选择须在当前空间授权范围内；继承默认模型失败返回配置错误。graph打开但无实体/关系类型不得标为ready。受限端点创建新库后能否扩展范围必须是管理员预先声明的策略，不能由普通create请求扩大端点权限；范围更新要原子合并，避免并发覆盖。返回 `selectable_on_endpoint` 及原因。

### 3.2 `ingest_document`

submit把“安全上传字节→持久化回执→调用原生导入→入队”封装成一次工作流，**不等待完整解析或Wiki生成**。

```json
{
  "action":"submit",
  "knowledge_base_id":"kb-A",
  "file":{"name":"acceptance.pdf","encoding":"base64","content":"JVBERi0..."},
  "process_config":{"parser_engine_rules":[{"file_types":["pdf"],"engine":"<从能力发现选出的引擎名>"}]},
  "idempotency_key":"一次用户上传操作的唯一值"
}
```

`<从能力发现选出的引擎名>`是契约说明，不是硬编码的真实引擎。主产品文件控件调用MCP提交字节，文件内容不经过LLM消息；不接受远端客户端的本机file_path，不回退Agent REST，不要求共享卷。现有服务端文件大小上限继续生效，文档明确base64膨胀及JSON请求限制；本阶段不再加分片工具凑复杂度。大文件扩展另议，不冒充标准MCP上传。

响应含ingest_id、upload_id、document_id（若已创建）、deduplicated、requested/applied解析配置及阶段。status仅传ingest_id，统一查看上传/导入/解析/索引/Wiki/图谱：

```json
{"action":"status","ingest_id":"ingest-123"}
```

阶段是结构化状态，不给无法测量的假百分比。解析完成不等于索引可查，索引可查不等于Wiki/图谱生成完毕；disabled/not_configured/failed均独立显示。

保留已有租约、回执清理、断点恢复和实算SHA256。幂等键需绑定调用方/端点/目标KB/字节及处理配置摘要；重复请求不能修改配置后悄悄复用旧任务。dedup复用旧解析时必须报告配置未重新应用，不能用旧结果证明选定引擎已执行。请求失败但文档已创建需返回可恢复身份，不无限重传。

### 3.3 `search_knowledge`

检索方式 `mode=hybrid|semantic|keyword` 与资料来源 `sources=[document,wiki,graph]` 是两个维度。前者在原文检索路由中生效；Wiki/图谱用自己的查询方式，不能假装所有资料都做BM25+向量。

```json
{"query":"青岚计划由谁负责","knowledge_base_ids":["kb-A"],"mode":"hybrid","sources":["document","wiki"],"limit":10}
```

结果保持分来源类型与证据身份，包含document_id/chunk_id或Wiki slug/source_refs。图谱返回实体关系及其原始来源；不能返回无出处三元组作为确定事实。所有来源都使用同一授权范围。

有限的 `match=literal` 可复用现有grep能力；非法/无有效字面项的模式明确拒绝。需要文档内穷尽查找时引导read_document，不宣称索引Top-K能够找齐全部文档。

返回partial、参与来源、失败来源、分页/截断信息。用户明确要求的来源不可用时显式错误或partial，不能以空数组悄悄降级。第一阶段可保留“分来源结果集”，不要求发明跨类型分数排序。

### 3.4 `read_document`

有限 `action=list|read`，`source_type=document|wiki`；list替代独立文档目录/Wiki目录工具，read支持已有offset/limit/query。

```json
{"action":"read","source_type":"document","knowledge_id":"doc-1","offset":0,"limit":20}
```

```json
{"action":"read","source_type":"wiki","knowledge_base_id":"kb-A","slug":"projects/qinglan"}
```

每个chunk、Wiki页和source_ref都重新核验范围；不能只检查外层knowledge_id。source_ref指向原始文档时仍用本工具读取核验。图谱关系的原文同样回到其文档/chunk，不再新增graph_read工具。正文图像资源沿用现有授权资源链接转换，不暴露原始磁盘路径。

### 3.5 `ask`

```json
{"question":"哪些批次可以放行，为什么？","knowledge_base_ids":["kb-A"],"mode":"quick-answer"}
```

- mode继续使用现有 `quick-answer|smart-reasoning`，不擅自改成quick/agent导致兼容混乱。
- 如需知识型自定义Agent/Wiki融合，增加受限 `profile_id`，只能取knowledge_base能力响应中管理员发布的profile；不能直接接受任意agent_id。
- 默认行为继承端点的允许配置。mode/profile冲突要拒绝，不能收了参数却忽略；返回实际mode/profile与effective_knowledge_base_ids。
- `smart-reasoning`不等于自动选中hybrid-rag-wiki预设。需要Wiki融合的发布profile使用现有混合预设并限制到知识工具，详见独立机制说明。
- 服务器始终禁用公开网络搜索、记忆、历史搜索、shell/browser/技能安装等已裁能力，旧Agent配置也不得重新注入。
- 用户问答Agent限制为知识型工具；Wiki异步生成所需的受控内部写页/链接工具继续保留，不能因问答只读而把Wiki生产链一起禁掉。
- 保留session_id用于同范围续问；KB范围、mode/profile变化时前端新开会话，后端也检查绑定或有效scope fingerprint，防止旧历史跨范围污染。
- 引用需结构化且可回读；空检索、模型失败、超时不能伪装成空答案成功。

## 4. 五工具也必须有细粒度权限

同一个工具有只读/写动作时，工具注解设保守值；不能因为list公开就给create权限，也不能把所有list用户一律要求ingest。最小动作矩阵：

| 工具/动作 | 权限与旧能力要求 |
|---|---|
| knowledge_base.list/get | retrieve + 本调用方可见范围；能力字段还按原能力可见性过滤 |
| knowledge_base.create | 原create_knowledge_base许可 + ingest + 原生建库角色检查 |
| ingest.submit | 原upload_file与import_document_file均允许 + ingest + 目标KB写权限 |
| ingest.status | 对应状态读许可 + 回执原始调用方校验 + KB读权限 |
| search.document/wiki/graph | 对应旧工具许可逐来源检查，不能只因允许search就开放全部Wiki/图谱 |
| read.document/wiki | 对应原读工具许可 + 逐来源授权 |
| ask | chat + retrieve + 允许profile + 最终KB可见范围 |

主产品请求的KB是检索选择，不是权限授予。有效范围取调用方可见、端点允许、显式选择及发布profile额外限制的交集；Agent默认KB只是缺省来源，不能用它替代授权。参数与header矛盾时明确拒绝，不能默默取更大范围。

当前本地fork的端点Bearer+用户JWT机制继续保留，主产品提供用户身份；官方原生的端点机器主体方案与它不同。不要为精简工具删除身份中间件或把用户Token放进模型提示词。

## 5. 迁移方案：核心工具面与兼容工具面分开

建议在同一个WeKnora进程、同一endpoint存储/身份/限流下，新挂载 **`/mcp/core/:endpoint_id`**；旧 `/mcp/:endpoint_id` 仅作过渡。该路径是待新增设计，不是当前已存在。

同一Server依赖复用两个MCP handler：core只注册五工具；legacy暂保留现有目录。不要在一个tools/list中同时列出新五项和旧二十二项，也不要给同名工具发布两种相互矛盾schema。

初期动作授权从现有端点白名单映射，保留原retrieve/chat/ingest scope，不额外暴露任意动作。新增独立core guard，不能直接绕过现有guardTool，也不能因core工具名称不在旧白名单就全部403。限流按一次公开调用计，内部编排不重复扣多次或完全漏计。

主产品只连core；独立Agent的MCP URL及工具名单同步更新，改用五工具中的只读动作。UI文件上传使用普通SDK程序调用，不把写工具交给LLM自由决策。所有既有客户端迁完并有兼容期调用记录后，移除legacy对外路由与不再使用的schema/包装代码；原生Agent内部Wiki工具等继续保留。

## 6. 核心实现评审：保留、完善、裁剪

| 能力 | 当前源码状态/评审 | 本轮计划 |
|---|---|---|
| KB列表/创建 | 已有tools_kb_create；继承模型与graph_ready已有修复 | 保留；纳入并发范围更新、缺模型与图谱配置集成测试 |
| PDF上传/导入 | 标准upload_file + import_document_file，持久化回执/租约 | 保留状态机，收拢为submit/status，不重写另一套 |
| 解析器 | list_parser_engines + import process_config校验已接通 | 保留；当前MCP仅暴露parser_engine_rules，不宣称全部内部process字段均可用 |
| 问答mode | 两个原生模式已实现并有测试 | 保留；补发布profile与模式/KB会话绑定 |
| RAG检索/原文 | search/grep/read等已有实现 | 聚合公开面，保留分页、来源与权限 |
| Wiki/图谱 | 已有生成与工具；开关与ready不等价 | 保留。Wiki自动进入快问向量检索不能仅凭WikiBoost插件存在就声称已打通 |
| Agent配置 | KB范围、模型、原生知识工具是核心 | 留知识型配置；删网络/记忆/沙箱/发布等配置项 |
| 消息产品 | 历史聚合/搜索/问答入库是扩展 | 删除扩展；基础session/message不能删 |
| WeKnora-Agent | CLI标准LangChain+可选薄工作台 | 保留验证用途；主产品不依赖它作为业务中间层 |

## 7. 删除清单与连带依赖

### 7.1 必须保留的底座

`internal/mcpserver`、端点管理与认证、tenant/principal/RBAC、KB/knowledge/chunk、文件存储与资源授权、模型配置、embedding/rerank、docparser/docreader、会话/基础消息、原生QA、知识型Agent、Wiki/graph与其任务。

Redis任务/流、SQLite/索引、Neo4j按当前核心配置保留；不是Agent数据库。不要因裁长期记忆而删 `internal/stream/memory_manager.go`，那是内存流实现。

### 7.2 逐模块裁剪表

| 模块 | 可删入口与专用实现 | 必须先解耦/停止 | 不得连删 |
|---|---|---|---|
| 消息聚合/历史搜索 | `routes_chat.go`中的search/historyStats、message.go中的历史KB索引/搜索、SearchConversations工具、ChatHistorySettings相关项 | qa完成后的IndexMessageToKB；旧Agent allowed-tools；历史索引任务生产者 | sessions/messages、CreateMessage、load_history、消息完成/取消和引用存储 |
| 长期记忆 | `routes_memory.go`、handler/memory、service/memory、memory专用仓储/工具/前端Memory设置 | session/Agent/handler构造参数；MEMORY_RECALL事件；MemoryAffinity；回答后写记忆；asynq与sync任务消费者 | LOAD_HISTORY；WikiBoost及CHUNK_RERANK事件；普通KB/chunks；memory命名的流/内存结构 |
| IM/嵌入发布 | `internal/im`、im/embed handlers与routes_agent中的回调/渠道接口、前端IM/Embed集成页 | container.registerIMService启动副作用；渠道连接；embed framing配置与cleanup | SessionService、原生QA、`channel="mcp"`消息标识；MCP Server发布管理 |
| 公开网络搜索 | web_search/providers/credentials路由与专用服务、web_search/web_fetch工具、WebSearchSettings | WEB_FETCH流水线事件；Agent运行时注册；默认provider查询；DeleteSession的webSearchStateRepo清理 | 模型HTTP、远程解析器、SSRF防护、已决定保留的URL文档导入/资源下载 |
| 智能体执行沙箱 | terminal/desktop/local-browser、shell/file/skills工具、Sandbox/Skill/Browser设置、执行后artifact/checkpoint/fork扩展 | AgentQA必需stager断言、sandbox布局解析、turn lease、handler装饰依赖、reaper/cleanup、旧绑定回收 | docreader.parser.SandboxExecutor；转换/OCR隔离；基础QA取消互斥与消息完成；表格文字解析 |
| 第三方MCP客户端（本期无需求） | 工具箱外部MCP、OAuth/审批管理入口及运行时挂载 | internal/mcp manager、工具注册、OAuth/审批DI与遗留连接 | internal/mcpserver、MCP endpoint表/认证、主产品MCP通道 |

上述是代码裁剪目标，不是“只隐藏菜单就完成”。本地化项/类型/依赖包只在确认没有核心引用后删除；旧数据库迁移脚本不能随模块目录盲删。

### 7.3 需要重点避开的级联点

1. `tools_ask.go:239/283/389` 直接创建会话/消息并进入QA。删除基础消息将立即破坏MCP ask。
2. `session_knowledge_qa.go:212/219` 编排MEMORY_RECALL；先改pipeline事件列表，再删插件。MemoryAffinity与WikiBoost共用CHUNK_RERANK，只删记忆插件。
3. `router/task.go:320`、`router/sync_task.go:268` 都有记忆任务。关生产者、处理遗留任务，再删两路消费者，不能只删Redis版本。
4. `session.go:560/633/693` 清理网络搜索状态时没有nil保护。移除实现前抽掉这项职责或提供明确空实现；不要返回typed-nil骗过dig。
5. `session_agent_qa.go:190` 要求attachment stager，随后解析sandbox。先让知识问答不再依赖执行环境，保留合法附件解析，再删沙箱provider。
6. `router.RouterParams` 有大量非optional dig字段。删一个provider可能使整个应用启动失败，即使该功能没有请求。必须一起改构造参数、注册与启动测试。
7. `container.go`的IM Invoke、browser/skills/reaper会启动后台行为。隐藏UI和不注册HTTP不能阻止这些副作用。
8. `docreader/parser/doc_parser.py` 的SandboxExecutor是转换进程隔离；它与用户Agent VM不是同一模块。

沙箱解耦后，回合互斥、取消与并发防护仍须存在；如果当前turn lease同时承担这些职责，应保留或抽成中性的会话运行控制，不能简单替换成永远成功的空实现。浏览器执行工具和解析器内部用于渲染的浏览器也要按调用方区分。

## 8. 可执行任务与删除顺序

### Task 0：固定基线，不重做已完成项

**文件：** 当前两仓Git状态；新建 `examples/mcp-validation/eval/reports/core-profile/manifest.json`（脱敏，按现有reports管理方式保存）。

- [ ] 记录HEAD、分支、diff与未跟踪文件；不回滚LOCAL-SETUP/override或其他用户变动。
- [ ] 跑解析/mode/建库现有测试，保存实际命令与状态；完成一轮当前PDF→两QA模式→引用→Wiki/图谱基线。
- [ ] 固定公开工具清单、现有消费者和端点许可，不把旧文档数量当事实。

### Task 1：建立知识核心运行档位，关闭非核心可达性

**拟修改：** `internal/config/config.go`、`internal/container/container.go`、`internal/router/{router,deployment_capabilities}.go`、`frontend/src/config/deploymentCapabilities.ts`、侧栏/设置路由；拟新增 `internal/container/core_profile.go` 与 `core_profile_test.go`。

计划新增运行档位 `WEKNORA_PRODUCT_PROFILE=knowledge-core`，明确这是新配置，不是已有变量。复用deployment capabilities区分MCP Server与MCP Client，减少第二套开关。

- [ ] 先写启动/路由反例：core档无IM/embed/memory/websearch/terminal/desktop/browser入口和启动连接，但KB/解析/MCP/QA可启动。
- [ ] 重构非核心provider为可选装配或知识核心的独立装配函数，现有必填RouterParams同步修改；禁止单纯置nil然后留下无保护调用。
- [ ] 服务器强制限制知识Agent工具集；历史/自定义Agent中的旧能力不得重新启用。
- [ ] 保留full档作过渡回滚，core档部署成为推荐。此阶段不物理删DB表或共享源码。

### Task 2：五工具业务面

**拟新增：** `internal/mcpserver/{core_server,core_policy,core_knowledge_base,core_ingest,core_search,core_read,core_ask}.go` 及对应 `*_test.go`。

**拟修改：** `internal/router/routes_agent.go`中MCP路由、MCP API文档/客户端示例、`internal/types/mcp_endpoint_tools.go`的目录/能力映射（保留旧端点数据可读）。

- [ ] 写协议测试：core tools/list在完整许可端点恰好五项，旧二十二项不能混入；未许可工具/动作即使猜名称仍拒绝。
- [ ] 实现CoreHandler与同进程共享依赖，不启动第二个数据库/后台队列；挂载新core路径并保留原认证。
- [ ] 从现有handlers提取共同的经授权业务函数，由core与legacy复用。不要core内部伪造JSON-RPC调用legacy，也不要MCP回调自己的REST。
- [ ] 逐项实现第3节请求/响应，动作权限、分页/partial、错误和scope回归在同一批变更中完成。
- [ ] 明确重复、失败恢复、处理配置固定规则；不能以“少几个工具”为由省略回执持久化和租约。

### Task 3：主产品及验证客户端迁移

**修改：** Agent `mcp.json`、`agent.py`、`webapp.py`、`web/assets/{app,views}.js`、tests及README；主产品接入层对应标准MCP配置和业务页面。

- [ ] 文件控件直接submit→status；选库、解析器和mode/profile来自核心能力发现，不要求使用者输入二十二个工具参数。
- [ ] 统一主产品qa范围和MCP参数；切范围/模式/profile清理会话并抛弃旧响应；后台验证scope一致。
- [ ] 独立LangChain只接只读动作，避免knowledge_base.create/ingest.submit被模型自主触发；可用受限schema或只读端点，由服务端兜底。
- [ ] 用户可见引用回读有效；脱敏轨迹留诊断入口。主产品不调用WeKnora-Agent REST做代理。
- [ ] 用A/B库与随机PDF问题证明scope，记录实际解析器/mode/profile，不能只看按钮变化。

### Task 4：按依赖从低到高真正删除代码

**顺序固定：** IM/Embed → 消息历史产品 → 长期记忆 → 公开网络搜索 → 执行沙箱 → 无用第三方MCP客户端 → 旧对外工具适配。

每一批都执行：

- [ ] 禁新生产者和入口，停止启动连接/调度；清点遗留任务与资源。
- [ ] 解耦constructor/DI/pipeline/完成回调；旧任务明确终止或排空，不制造无限retry。
- [ ] 运行core启动和两问答模式回归，验证取消、重试、会话删除等非happy path。
- [ ] 搜索跨目录生产引用，物理删除专用handler/service/repository/UI/tests中失去意义的部分，留下新的禁用/权限反例测试。
- [ ] 更新go.mod与前端依赖锁，仅移除确实无引用的包；保留共享types和兼容字段直至迁移完成。
- [ ] 显式列本批文件提交，不混入其他工作区变动；每批独立可回滚，不要求用户批准一次危险大删除。

执行沙箱这一批尤其需要完整应用DI启动和真实知识Agent回归。若无法保持smart-reasoning，不能把该模式偷偷关掉再宣称裁剪成功；需继续拆除无关执行依赖。

### Task 5：旧MCP目录下线与存量数据处理

- [ ] 主产品和验证客户端只使用core，确认兼容期无旧路由调用，再移除legacy路由/schema/仅供兼容的包装代码。
- [ ] 保留原生Agent运行时知识工具，即使其名字不出现在core tools/list中也不能删。
- [ ] sessions/messages/custom_agents/knowledge/chunks/wiki/graph/upload/mcp端点/身份表保留。旧非核心表先只停写，不DROP、不全库VACUUM、不重建卷。
- [ ] 物理数据清理另出可审计迁移：备份、行数/关联清点、资源回收、升级验证后才安排；本计划的代码裁剪不依赖清数据才能生效。

## 9. 安全裁剪的验收与回滚

不是承诺“绝不会级联”，而是以可执行检查阻止带级联回归的变更交付。

| 门禁 | 必须证明 |
|---|---|
| 装配/启动 | 删除provider后core仍完成dig构造与router注册；不创建非核心连接或进程 |
| 入库 | PDF文字、扫描图、表格；默认与显式引擎；真实新解析、dedup、失败恢复、并发幂等 |
| 检索 | 原文/Wiki/图谱正向命中及source回读；分页/partial；无资料如实报告 |
| 问答 | quick-answer、smart-reasoning、发布的rag-wiki profile均工作；有效mode与KB可核验 |
| 作用域 | A可答/B新会话不能引用A；旧session、chunk/wiki/upload ID不能绕过权限 |
| 禁用能力 | 旧Agent配置填回memory/web/shell/browser仍不可运行；已删API不可达 |
| 后台 | Redis/asynq与Lite/sync两模式的解析/Wiki任务都可完成；无无主memory/IM/sandbox任务重试 |
| 生命周期 | 取消、重试、文档删除/重解析、会话删除无panic/复活任务/资源泄漏 |
| 工具数 | 推荐业务端点完整许可时恰好五项；只读端点可按许可更少，不能冒出二十二项 |

每批保存代码版本、配置名称（不含秘密）、迁移前后测试和非核心引用清单。失败只回滚该批代码/配置；因为没有drop数据，可恢复旧应用读取同一份数据。不要靠删除测试、空实现返回成功来通过门禁。

## 10. 测试命令与记录

本轮已运行并通过的定向检查：

```powershell
docker exec -w /review-src -e CGO_ENABLED=1 WeKnoraAgent-correction-check go test -tags sqlite_fts5 ./internal/mcpserver -run 'TestResolveAskMode|TestResolveAskAgent|TestResolvedMode|TestListParserEngines|TestParseImportProcessConfig|TestCreateKnowledgeBase|TestGraphExtractionConfig' -count=1
```

后续实施必跑，以下不是本轮已完成结果：

```powershell
# 在 E:\GoLandCode\WeKnora，或具备CGO/FTS5的校验容器相同目录
go test -tags sqlite_fts5 ./internal/mcpserver ./internal/middleware ./internal/router ./internal/types
go test -tags sqlite_fts5 ./internal/application/service ./internal/application/service/chat_pipeline ./internal/agent/tools ./internal/container
go build -tags sqlite_fts5 ./cmd/server

# E:\GoLandCode\WeKnora\frontend
npm run type-check
npm run build
npm test

# E:\GoLandCode\WeKnora-Agent
.\.venv\Scripts\python.exe -m pytest tests -q
node --test tests/test_token_session.js tests/test_identity_lifecycle.js
```

定向测试通过不代表正式启动和主产品E2E已经通过。执行时核验容器只读挂载、缓存和CGO，不为过测试改动用户资料。

## 11. 给执行模型的指令

> 先读本方案和 `docs/architecture/2026-10-16-qa-rag-wiki-agent-mechanisms.md`，核验当前HEAD与源码，再按Task0-5实施。主产品目标是一个core MCP端点、五工具，不是把二十二项直接删掉；知识业务仍在WeKnora。按动作保持原权限、范围与原生工作流。删除消息历史产品而保留基础session/message；删除长期记忆而保留短期history；删除IM/Embed而保留MCP Server；删除智能体执行沙箱而保留文档解析隔离。逐批解耦DI/任务/pipeline后物理删专用代码，不删数据卷、不drop存量表、不削弱核心两问答模式或Wiki/图谱来凑完成。交付实际命令、五工具协议、核心与负向验收证据及剩余缺口。
