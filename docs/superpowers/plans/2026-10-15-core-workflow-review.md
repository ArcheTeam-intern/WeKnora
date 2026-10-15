# 当前实现复核：收敛为 PDF 解析与知识库问答

日期：2026-10-15，北京时间。本轮根据用户执行后的工作区重新审查；旧交接中的故障状态不直接沿用。本轮只做验证、审查、测试资料生成，未修改生产业务代码、端点权限或已有文档。

## 一、结论

**基础链路有进展，但仍未满足本次明确的产品预期。** 当前更接近“20个MCP工具的开发验证台”，缺少“选择PDF解析方式”和“选择WeKnora原生问答模式”两项关键连接。

顶栏知识库跨页面显示，本身不能说明失效。本轮真实工具调用已验证范围有效；问题是上传目标和全局范围相互独立，以及范围变更后旧会话未清理。

用户要的主流程应当是：

```text
资料页：选择目标知识库 → 选择PDF → 选择解析引擎/方式 → 上传并解析 → 查看状态
问答页：选择检索知识库 → 选择WeKnora问答模式 → 提问 → 查看答案与可核验引用
```

标准 LangChain Agent 保留为独立验证入口。Wiki、知识图谱保留在 WeKnora 和 MCP 能力中，但用户不需要在上传时填写其底层工具参数。

## 二、本轮重新运行的验证

| 项目 | 实际命令/操作 | 本轮结果 |
|---|---|---|
| Agent Python | Agent `.venv/Scripts/python.exe -m pytest tests -q` | 54 passed / 1 skipped；live未跑，不能计入成功 |
| Token/身份状态 | `node --test tests/test_token_session.js tests/test_identity_lifecycle.js` | 14 passed |
| WeKnora验证脚本 | Agent Python运行 `-m pytest examples/mcp-validation/tests -q` | 42 passed |
| Go相关包 | `docker exec -w /review-src -e CGO_ENABLED=1 WeKnoraAgent-correction-check go test -tags sqlite_fts5 ./internal/mcpserver ./internal/models/api ./internal/types` | 三包通过；不是全仓Go测试 |
| 实际连接 | 工作台读取现有本机凭证，再经MCP校验连接 | 可见3个知识库、20个工具；未打印真实Token |
| KB范围反例 | 顶栏仅选核心验收库，工具台请求另一个库的 `list_documents` | `KB_OUT_OF_SCOPE`，范围限制生效 |
| KB范围正例 | 保持顶栏范围，改为请求所选库 | 成功列出7份文档，包括既有样本PDF |
| 实际MCP ask | 仅依据既有 `sample-transaminase-short.pdf` 问pH/温度/DMSO/ee | 答出8.0、30-45℃、10%、大于99%，返回该PDF原文chunk引用 |
| 实际LangChain | 切换独立Agent执行链，要求检索并回读同PDF | 真实调用list_knowledge_bases/list_documents/read_document，答出pH8.0和ee>99%，document/chunk ID一致 |
| 切换KB会话 | ask成功后，改顶栏为另一个库 | 原 `qa-session` 仍保留；未自动新会话 |
| 解析/模式选择 | 阅读schema、前端及服务调用 | 未实现用户想要的两个选择链路，见Spec |

本轮未把新生成的测试PDF上传到任何知识库，留给用户首次上传。未重新跑20工具全部正负例、全量评估或所有前端控件，不能据上述结果宣布整体完成。

MCP ask的答案数值正确，但页面仍把 `<kb .../>` 原始标记显示为文字，且引用区同时列了未用于回答的其他候选资料。这属于引用呈现需要收口的问题，不等于本次答案来自那些额外文档。

截图目录：`E:\GoLandCode\WeKnora\output\review-20261015`，含 `ask-answer.png`、`langchain-answer.png`、`kb-selector-mismatch.png` 与选择冲突的DOM记录JSON。测试辅助源：`.planning/2026-10-15-current-review/header_timeout_repro.go`。

LangChain此次能完成真实检索，但页面将上游模型的`<think>`包装也当成最终正文展示，需要按SDK内容/推理通道区分处理，不应混入用户答案。此表现不影响本次核对到的数值和ID正确性。

## 三、Spec：需求与实现差距

### [P1] PDF解析选项没有接到MCP

- Agent `web/assets/views.js:28` 只有标准工具/文件流通道，不是解析器。
- WeKnora `internal/mcpserver/tools_import.go:60` 的 `import_document_file` schema只接受知识库与upload_id。
- 同文件约223行调用 `CreateKnowledgeFromFile(..., nil)`，最后的processOverrides固定nil。
- 原生上传接口已有 `types.KnowledgeProcessOverrides`，包括 `parser_engine_rules` 和 `parser_engine_overrides`，但MCP未封装。

影响：用户上传时无法选择引擎。即使界面未来增加下拉框，如果仍传nil，也不生效。

### [P1] 问答选择的是执行入口，尚未封装原生模式

- Agent `views.js:372` 的 ask/LangChain是两套编排入口。
- WeKnora `tools_ask.go:33` 没有mode参数；约125行固定读取端点DefaultAgentID。
- WeKnora原生存在 `quick-answer` 与 `smart-reasoning`，服务端runQA已能根据原生Agent走KnowledgeQA或AgentQA。

影响：用户不能通过当前页面选择WeKnora原生快速问答/智能推理模式。不能把点击LangChain按钮当成原生模式切换成功。

### [P2] 上传目标与顶栏范围冲突

- `views.js:275` 的syncKbSelect优先保留旧m-kb；`app.js:59`的mcpHeaders发送顶栏选择。
- 本轮页面复现：顶栏仅选“核心验收”库，材料页目标仍为“MCP验证语料”。
- 直接查询其他库会被服务端拒绝，说明不是服务端无视顶栏，而是前端有两个相互冲突的状态。

应拆成资料页“上传目标”和问答页“检索范围”，不要跨页面强加一个含义不明的全局可编辑选择器。

### [P2] 切换KB不重置问答上下文

- `views.js:712` 的onSelectionChanged没有清Q.sessionId，也不失效在途旧范围请求。
- 后续ask仍发送旧session_id；服务端同所有者会话可复用，原生多轮历史按session读取。
- 本轮实际观察到切库后session显示不变。旧历史影响答案的条件是端点启用多轮；本轮没有把该条件下的混入答案作为实测成功复现。

应在KB范围或问答模式变化时，新建逻辑会话，并让旧请求结果不能写回新范围。仅身份epoch测试通过不足以覆盖范围变化。

### 需要明确的空选择含义

当前“清空”省略KB header，意思是“不额外收窄端点权限范围”，所以可能检索所有已授权库。不是“没有知识库”。建议普通问答界面未选KB时禁用发送；若提供“全部可用库”，做成单独明确选项。

## 四、Standards：独立的正确性与安全发现

本轮没有找到适用于这些路径的额外AGENTS/CONTRIBUTING规则，以下是具体行为问题，不是泛化风格要求。

### [P1] 真实响应头超时不会进入新增重试

`internal/models/api/endpoint.go:176` 排除了 `errors.Is(err, context.DeadlineExceeded)`。Go真实ResponseHeaderTimeout也满足该判断，即使请求context尚未过期。新增mock timeoutErr没有实现相同行为，因此单测通过不能证明修复了真实故障。

本轮使用真实httptest服务器与10ms响应头超时复现：

```text
transport_timeout=true
errors_is_deadline=true
caller_context_alive=true
current_retry_predicate=false
```

修复应区分请求总deadline和transport首包超时，并用真实HTTP测试；同时不能假定“没收到响应头=服务端没收到POST”。对模型生成自动重放需处理重复执行/计费问题，不能简单删除一个if就算完成。

### [P1] 读取本机凭证的接口缺少来源限制

Agent `webapp.py` 的 `local_credential`（约1100行）无鉴权、Host/Origin门禁，读取本机JWT返回给HTTP请求，并返回本地源路径。只绑定127.0.0.1不能替代Host校验；审查者以合成凭证验证，Host为attacker.invalid时仍返回合成Token。

建议默认保留手工JSON导入；若本地Demo保留便利入口，显式启用、校验本机Host和同源请求、禁止缓存、不回传绝对路径，并增加反例测试。当前默认相邻WeKnora目录也使“拷走Agent即可独立运行”的说法存在额外配置前提。

### [P2] 全局60秒首包上限改变所有非流式模型的超时语义

`internal/models/api/transport.go:68` 固定ResponseHeaderTimeout=60s。非流式模型可能完成生成才发头，因此会在仍有总预算时被截断。应可配置或按流式/非流式调用设置，并测试合法慢响应；不能仅以本机几次快请求成功归因到keep-alive代理。

本轴：2项P1、1项P2。Spec轴：2项P1、2项P2，另有空选择与引用呈现设计问题。两个轴均有未解决问题。

## 五、工具“多”的处理：减少前台负担，不盲删后端能力

当前连接的是一个WeKnora MCP端点，暴露20个tools，不是20个独立MCP服务。

推荐分层：

| 层次 | 保留内容 | 呈现方式 |
|---|---|---|
| 主流程 | 选库、上传、解析选项、状态、问答模式、答案引用 | 资料/问答两个主页面 |
| 主流程底层MCP | list_knowledge_bases、上传/导入/状态、ask、原文回读；补解析引擎发现 | UI封装操作，不要求用户手填tools/call参数 |
| 原生核心扩展 | 检索、Wiki、图谱 | 保留服务工具；在模式或高级能力中使用 |
| 验证诊断 | 工具schema编辑、原始JSON、上传幂等key、批量清单、Markdown增改删 | 默认隐藏的开发诊断入口，不放材料页主流程 |
| 独立标准Agent | LangChain只读MCP编排 | 可选开发验证入口，避免冒充WeKnora原生模式 |

get_upload_status与get_document_status分别管理暂存回执和已导入文档，不能仅因名字相近就删除。工具是否保留，按职责和实际调用决定；不为凑工具数量合并成包罗万象的超级工具。

## 六、最小修改顺序与契约

### P0-1：先拆开知识库状态

修改Agent app/views/index：删除全局可编辑KB顶栏，或只保留当前页面范围的只读摘要。

- 资料页单选uploadTargetKbId；上传/导入/状态请求仅携带这个KB范围。
- 问答页单选或明确多选qaKbIds；显式传给ask的knowledge_base_ids，同时使用相同header收窄，避免两套状态冲突。
- 改qaKbIds/mode时清会话、失效旧请求、显示新范围。未选禁用发送；“全部可用”需单独表达。
- 负例必须包含范围切换期间旧响应迟到、上传目标与QA范围不同、清空不偷偷扩展检索范围。

### P0-2：贯通原生解析配置

WeKnora新增只读 `list_parser_engines` MCP工具，复用现有reader.ListEngines及工作空间配置，返回安全的name/file_types/available/unavailable_reason，不返回配置密钥、内部服务地址或凭据。用户只应看到本部署真实可用的PDF引擎。

扩展现有import工具，加入经校验的process_config，复用KnowledgeProcessOverrides并传给CreateKnowledgeFromFile最后参数。示意：

```json
{
  "knowledge_base_id": "用户所选库ID",
  "upload_id": "本次标准上传回执ID",
  "process_config": {
    "parser_engine_rules": [{"file_types": ["pdf"], "engine": "服务端发现的可用引擎名"}]
  }
}
```

前端缺省为“使用知识库默认”；明确选择引擎时不可静默回退。不把system REST调用搬到Agent；MCP服务内部复用服务接口。

按当前正式配置约束限制可用选项，不允许MCP传入任意服务地址、云密钥或扩展部署权限。若提供OCR开关，仅映射现有允许的parser_engine_overrides字段。

解析配置应持久化，状态可核验requested/applied引擎。相同upload重试不能切换处理配置而悄悄返回旧结果；同PDF不同解析器命中dedup时，明确返回“复用旧解析，当前选项未重新应用”，或走现有正式重解析流程并记录新任务。不能靠下拉框变色证明生效。

### P0-3：贯通原生问答模式

扩展ask参数mode（保留不传时的现有端点默认行为），只开放本端点明确允许的原生 `quick-answer` / `smart-reasoning`，根据已有Builtin常量或受控配置解析。不同端点可以只开放其中一个；服务端强制校验。

不能让客户端传任意内部agent_id。原生Agent模式依然要继承工具/KB范围/写能力限制，不能借此开启shell、联网、写库或内部修复Agent。

问答链必须变为：页面模式与qaKbIds → 工作台请求 → MCP ask参数 → WeKnora原生QARequest。返回实际mode与知识库范围，用于验收；引用用结构化数据渲染，不能用未经处理的原始标签充当可点击出处。

LangChain验证入口独立，不混在上述原生模式选择器中。

### P1：诊断收起与缺陷收尾

将当前材料页的幂等键、清单、Markdown写工具和MCP工具台放到开发开关下。先保留后端能力和回归，不批量删除20工具。

独立修正HTTP超时策略与凭证便利接口问题，再运行相应反例测试；不扩大本次前端简化为全仓重构。

### 最后用新PDF执行矩阵

1. 同一个新PDF：上传A → 解析完成 → A新会话回答Q1/Q3/Q4/Q8。
2. B未上传该PDF：B新会话问Q1应说明资料不足；回A新会话应恢复。
3. 支持OCR/图像的解析方式：Q7能回读真实图像字段；纯文本方式应说明能力边界，不猜测。
4. 不同问答模式：模式参数和实际服务器模式一致，同一事实准确、引用可回读；不要求两个模式必须生成不同措辞。
5. 实际引擎不可用、模型超时、解析失败都明确失败；不写成空结果或通过。

## 七、测试文件

- 上传用：`E:\GoLandCode\WeKnora\output\pdf\WeKnora-PDF验收样本-青岚计划.pdf`
- 只供人工核对、不要上传：`E:\GoLandCode\WeKnora\output\pdf\测试问题与预期答案-不要上传.md`
- 可重建源：同目录 `generate_weknora_acceptance_fixture.py`。

已用ReportLab生成4页、嵌入中文字体；Poppler渲染全4页并检查。pypdf确认普通文字可提取、第3页图像内的口令与签收人不在文字层。因此可以区分文字解析与OCR/图像理解。

样本含8题：随机识别标记、有效版本、表格计算、双条件判断、跨页职责、图像字段和无答案问题。标准答案另存，避免直接索引答案说明。单PDF不能证明多文档找齐率或全部MCP封装完整。
