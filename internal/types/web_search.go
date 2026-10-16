package types

import (
	"database/sql/driver"
	"encoding/json"
)

// Public web search was retired from this build: the provider registry, the
// provider service/repository and its credential routes, the tenant KV key
// that wrote this config, the WEB_FETCH pipeline stage and both agent tools are
// gone. What stays is the persisted shape, because existing rows still carry
// it and a code change must not make stored data unreadable:
//
//   - tenants.web_search_config (WebSearchConfig, still returned redacted by
//     the tenant read DTO)
//   - web_search_providers rows (no Go type any more; the table is untouched)
//
// It is a data shape plus the masking helpers the tenant DTO needs. Nothing
// starts a search, resolves a provider or acts on any field.
type WebSearchConfig struct {
	// Deprecated: the old single-provider fields, kept so stored rows decode.
	Provider string `json:"provider,omitempty"`
	APIKey   string `json:"api_key,omitempty"`

	MaxResults        int      `json:"max_results"`        // 最大搜索结果数
	IncludeDate       bool     `json:"include_date"`       // 是否包含日期
	CompressionMethod string   `json:"compression_method"` // 压缩方法：none, summary, extract, rag
	Blacklist         []string `json:"blacklist"`          // 黑名单规则列表
	// RAG压缩相关配置
	EmbeddingModelID   string `json:"embedding_model_id,omitempty"`  // 嵌入模型ID（用于RAG压缩）
	EmbeddingDimension int    `json:"embedding_dimension,omitempty"` // 嵌入维度（用于RAG压缩）
	RerankModelID      string `json:"rerank_model_id,omitempty"`     // 重排模型ID（用于RAG压缩）
	DocumentFragments  int    `json:"document_fragments,omitempty"`  // 文档片段数量（用于RAG压缩）
	ProxyURL           string `json:"proxy_url,omitempty"`           // 旧版每请求代理覆盖，随功能退役
}

const DefaultWebSearchCompressionMethod = "none"

// DefaultWebSearchConfig returns the stored-shape defaults. It exists only so
// the read DTO renders a complete object for a row that never set the column.
func DefaultWebSearchConfig() *WebSearchConfig {
	return &WebSearchConfig{
		MaxResults:        10,
		IncludeDate:       false,
		CompressionMethod: DefaultWebSearchCompressionMethod,
		Blacklist:         []string{},
	}
}

// EffectiveWebSearchConfig normalizes a possibly empty stored config for
// rendering. No runtime path acts on the result any more.
func EffectiveWebSearchConfig(cfg *WebSearchConfig) *WebSearchConfig {
	if cfg == nil {
		return DefaultWebSearchConfig()
	}

	normalized := *cfg
	if normalized.MaxResults <= 0 {
		normalized.MaxResults = 10
	}
	if normalized.CompressionMethod == "" {
		normalized.CompressionMethod = DefaultWebSearchCompressionMethod
	}
	if normalized.Blacklist == nil {
		normalized.Blacklist = []string{}
	}

	return &normalized
}

// Value implements driver.Valuer interface for WebSearchConfig
func (c WebSearchConfig) Value() (driver.Value, error) {
	return json.Marshal(c)
}

// Scan implements sql.Scanner interface for WebSearchConfig
func (c *WebSearchConfig) Scan(value interface{}) error {
	if value == nil {
		return nil
	}
	b, ok := value.([]byte)
	if !ok {
		return nil
	}
	return json.Unmarshal(b, c)
}
