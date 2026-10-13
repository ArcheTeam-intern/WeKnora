# WeKnora MCP 服务验证资料

此目录属于 WeKnora。部署、测试语料和业务评估不进入 WeKnora-Agent 的运行依赖。

`deploy/compose.prototype.yml` 只管理 WeKnora、其原有 UI、Redis、Neo4j 及 Go 检查镜像。保持既有项目名、容器名与卷名，复用已有原型数据。Agent 后端、演示前端和 agent-check 服务已移除。

从本目录执行（本机已迁移 `.env.prototype`，其他环境从 example 配置）：

```powershell
$composeArgs = @('-p', 'weknora-agent-prototype', '-f', 'deploy/compose.prototype.yml', '--env-file', 'deploy/.env.prototype')
docker compose @composeArgs config --quiet
docker compose @composeArgs up -d redis neo4j weknora weknora-ui
docker compose @composeArgs run --rm --build weknora-check
```

本次裁剪只验证 Compose 配置，未重建 WeKnora 镜像或再次运行全量 Go 测试。既有知识服务继续运行。`up` 前检查构建镜像版本；不要使用 `down -v` 删除既有测试数据。

`scripts/make_corpus.py` 使用标准库生成十份测试文档，`eval/questions/demo-12.json` 保留十二题及所需证据。已有 manifest 文档 ID 映射一并保留。旧评估器调用被裁剪的 Agent HTTP API，不能用于当前 CLI，因此旧评估器及上传／登录适配脚本仅保存在项目外归档，未作为新链路交付。

后续评估通过标准 Agent 的 `ainvoke`／`astream` 读取工具消息，在本目录离线生成报告，不给 Agent 增加 API、证据加工或业务模块。评估应记录：

| 项目 | 标准 |
| --- | --- |
| 原文证据召回率 | 实际读取的标注证据数 / 题目全部必需证据数；只发现文档 ID 不算读到证据 |
| 相关文档找齐率 | 找齐全部标注相关文档的可回答题数 / 可回答题数；同时记录每题文档 recall |
| 事实准确率 | 人工核验为正确的事实数 / 输出的可核验事实数 |
| 事实完整率 | 答案覆盖的必要事实数 / 标注必要事实数 |
| 引用支持率 | 被原始证据支持的引用数 / 全部引用数；ID 有效性不能替代支持率 |
| 能力边界 | 无答案、冲突、截断、分页、权限失败、超时和工具错误分别记录；错误不能算空检索或成功拒答 |

初期门槛：每题保留原始工具轨迹及最终答案；十道可回答题文档 recall ≥ 90%、找齐率 ≥ 80%；人工事实准确率和引用支持率 ≥ 95%；两道无答案题均不得编造事实／引用；权限失败不得泄露未授权内容。报告必须显示原始分子／分母。十二题结果只用于原型检查，不代表通用性能结论。先对比单次检索基线与标准 Agent，再扩大语料和题库。
