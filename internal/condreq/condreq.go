// Package condreq 实现 **HTTP 条件请求**(6.6 / 6.12 缺口 #1)。
//
// 为什么需要自研:`golang.org/x/net/webdav` 的 handlePut 里 If-Match/If-None-Match
// **只有 TODO 注释**,ETag 只写响应头、不参与校验(6.12 表 #1 源码取证)。
// 不补这一课,WebDAV 客户端与 REST 客户端就都能用"盲写"覆盖别人的修改 ——
// 而 6.6 的并发写矩阵、6.11 的"覆盖失败显式裁决 412"全都建立在条件请求之上。
//
// # 语义(按 RFC 9110 收窄到本项目实际用到的部分)
//
//	If-Match: <etag>   → 不匹配则 412(要求资源**当前是**这个版本)
//	If-Match: *        → 资源不存在则 412(要求资源**存在**)
//	If-None-Match: *   → 资源存在则 412(要求资源**不存在**,即"仅新建")
//	If-None-Match: <etag> → 匹配则 412
//
// 刻意**不做** `W/` 弱比较:本项目的 etag 是 version+内容哈希派生的强校验器,
// 按弱比较会让"内容相同但版本不同"被当成匹配,从而放过一次覆盖冲突。
//
// 比较用**已加引号的完整 ETag 字面量**(`"00000002-deadbeef"`),即
// filesvc.QuoteETag 的输出 —— 保证三条出口(响应体/响应头/PROPFIND)与
// 这里的比较口径完全一致;任何一处自己拼引号都会造成"客户端拿到的 ETag
// 发回来永远不匹配"。
package condreq

import (
	"fmt"
	"net/http"
	"strings"
)

// Current 描述资源当前状态;`Exists=false` 表示资源不存在。
type Current struct {
	// ETag 是**已加引号**的 ETag(空串表示没有可用 ETag,例如目录)
	ETag string
	// Exists 表示资源当前存在
	Exists bool
}

// Result 是判定结果。
type Result struct {
	// Fail 为 true 表示应拒绝请求(客户端应收到 412)
	Fail bool
	// Reason 是给客户端的原因码(S 类:not_modified / precondition_failed)
	Reason string
	// Message 是人类可读说明(用于错误体)
	Message string
}

// Evaluate 判定条件头是否满足。
//
// 返回 Fail=true 时调用方必须回 **412 Precondition Failed**,并把当前
// ETag 放进响应头 —— 客户端据此更新本地状态后重试。
func Evaluate(h http.Header, cur Current) Result {
	ifMatch := h.Get("If-Match")
	ifNoneMatch := h.Get("If-None-Match")

	// If-Match 与 If-None-Match 同时出现是客户端错误(RFC 未定义优先级),
	// 显式拒绝比"任选其一"更安全:猜错会放过一次覆盖冲突。
	if ifMatch != "" && ifNoneMatch != "" {
		return Result{Fail: true, Reason: "precondition_failed",
			Message: "If-Match 与 If-None-Match 不能同时出现"}
	}

	if ifMatch != "" {
		return evalIfMatch(ifMatch, cur)
	}
	if ifNoneMatch != "" {
		return evalIfNoneMatch(ifNoneMatch, cur)
	}
	return Result{}
}

func evalIfMatch(hdr string, cur Current) Result {
	if isStar(hdr) {
		if !cur.Exists {
			return Result{Fail: true, Reason: "precondition_failed",
				Message: "If-Match: * 要求资源存在,但资源不存在"}
		}
		return Result{}
	}
	if !cur.Exists {
		return Result{Fail: true, Reason: "precondition_failed",
			Message: "If-Match 要求资源存在,但资源不存在"}
	}
	if cur.ETag == "" {
		// 有 If-Match 但服务端给不出 ETag(例如目录):无法判定 → 拒绝。
		// 放行等于把"无法校验"当成"校验通过",而条件请求的全部价值就是校验。
		return Result{Fail: true, Reason: "precondition_failed",
			Message: "该资源不支持条件请求(无 ETag)"}
	}
	if !matchAny(hdr, cur.ETag) {
		return Result{Fail: true, Reason: "precondition_failed",
			Message: fmt.Sprintf("资源已被他人修改(当前 ETag %s)", cur.ETag)}
	}
	return Result{}
}

func evalIfNoneMatch(hdr string, cur Current) Result {
	// If-None-Match 的语义与 If-Match **相反**:匹配即失败
	// (GET 场景下匹配应回 304,但本项目该分支由 handler 决定,这里统一报条件失败)
	if isStar(hdr) {
		if cur.Exists {
			return Result{Fail: true, Reason: "precondition_failed",
				Message: "If-None-Match: * 要求资源不存在,但资源已存在"}
		}
		return Result{}
	}
	if !cur.Exists {
		return Result{}
	}
	if cur.ETag != "" && matchAny(hdr, cur.ETag) {
		// GET/HEAD 场景语义是"未修改"→ 304;写场景是"已存在"→ 412。
		// 这里用 Reason 区分,由 handler 决定状态码(读走 304、写走 412)。
		return Result{Fail: true, Reason: "not_modified",
			Message: "资源未被修改(ETag 匹配 If-None-Match)"}
	}
	return Result{}
}

// ListContains 供 GET 场景判断"命中 If-None-Match 但语义是 304 而非 412"。
//
// 单独导出而不是让 handler 自己解析:头解析的边界(空白、多个值、`W/` 前缀、
// 大小写)只应有一份实现 —— 两处解析必然在某天对某个边界产生分歧。
func ListContains(hdr string, etag string) bool {
	return matchAny(hdr, etag)
}

// matchAny 在逗号分隔的 ETag 列表里找精确匹配。
//
// 标 `W/` 的候选会被**去掉前缀后按强比较**:本项目自己从不发弱 ETag,
// 但客户端可能把别处学来的 `W/"..."` 形式发过来;此时若直接按字面比较就永远
// 不匹配(客户端"明明拿了 ETag 却总是 412")。去掉前缀再比是更宽容且不放松
// 安全性的做法 —— 因为服务端发出的值本身是强的,匹配成功即代表版本一致。
func matchAny(hdr, etag string) bool {
	for _, part := range strings.Split(hdr, ",") {
		cand := strings.TrimSpace(part)
		if cand == "" {
			continue
		}
		if strings.HasPrefix(cand, "W/") || strings.HasPrefix(cand, "w/") {
			cand = strings.TrimSpace(cand[2:])
		}
		if cand == etag {
			return true
		}
	}
	return false
}

func isStar(hdr string) bool {
	return strings.TrimSpace(hdr) == "*"
}

// IfRangeAllows 判定 `If-Range` 是否允许按 Range 返回部分内容。
//
// 语义:If-Range 与当前 ETag 匹配 → 允许 206;不匹配 → 必须回**完整 200**。
// 客户端用它避免"下载一半时文件被改了,续传拼出损坏文件"。
//
// 与 If-Match 的区别:不匹配**不是错误**(不报 412),只是退化为完整响应。
func IfRangeAllows(hdr, currentETag string) bool {
	v := strings.TrimSpace(hdr)
	if v == "" {
		return true // 没带 If-Range → 允许 Range
	}
	// 也允许客户端直接放日期(弱校验)。本项目不依赖日期精度,
	// 一律按"不匹配"处理会退化为完整下载,是安全的降级。
	if strings.HasPrefix(v, "\"") || strings.HasPrefix(v, "W/") || strings.HasPrefix(v, "w/") {
		return matchAny(v, currentETag)
	}
	return false
}
