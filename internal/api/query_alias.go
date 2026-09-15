package api

import "net/url"

// queryAlias 取查询参数,主名优先、别名兜底。
//
// 为什么需要它:同一个概念在两个接口上写成了两种拼法
// (`GET /api/v1/files` 历史上只认 `space_id`/`parent_id`,而
// `/api/v1/changes`、`/api/v1/changes/head` 与架构文档用的是 `space`)。
// 只认一种的后果**不会报错**:参数被忽略 → `/files` 静默返回个人空间根目录,
// 前端显示一个"看起来正常、位置不对"的列表,服务端日志只有一行 200。
// 这个缺陷在 BE-S7-02 的探针里被抓到(契约按 `space` 发、实现按 `space_id` 读)。
//
// 两种拼法都收,契约里只声明**一个**规范名(见 docs/api/openapi.yaml):
// 声明多个同义参数会让生成出来的客户端类型出现两个字段,调用方无从选择。
//
// 同名同时给值时以主名为准(不做"冲突报错"):两者语义完全相同,
// 拒绝只会让一个能工作的请求失败,而没有任何正确性收益。
func queryAlias(q url.Values, primary, alias string) string {
	if v := q.Get(primary); v != "" {
		return v
	}
	return q.Get(alias)
}
