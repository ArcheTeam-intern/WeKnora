# 云雀平台 MCP 接口使用指南

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
