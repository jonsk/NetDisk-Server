package condreq_test

import (
	"net/http"
	"testing"

	"github.com/netdisk/netdisk/internal/condreq"
)

// HTTP 条件请求的语义测试(6.6 / 6.12 缺口 #1)。
//
// 这组用例的价值:条件请求**写错的典型后果是"放行"** —— 不匹配本该 412 却
// 返回 200,于是客户端的覆盖写静默覆盖了别人的修改。而这类"少报错"的错误
// 在正常路径上完全看不出来。

const etag = `"00000002-deadbeef"`

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func mustFail(t *testing.T, res condreq.Result, reason string) {
	t.Helper()
	if !res.Fail {
		t.Fatalf("应判定为条件失败(reason=%s),实际放行", reason)
	}
	if reason != "" && res.Reason != reason {
		t.Fatalf("reason 应为 %q,实际 %q", reason, res.Reason)
	}
}

func mustPass(t *testing.T, res condreq.Result) {
	t.Helper()
	if res.Fail {
		t.Fatalf("应放行,实际失败(reason=%s, msg=%s)", res.Reason, res.Message)
	}
}

// ---- If-Match ----

func TestIfMatch(t *testing.T) {
	cur := condreq.Current{ETag: etag, Exists: true}

	// 匹配 → 放行
	mustPass(t, condreq.Evaluate(hdr("If-Match", etag), cur))
	// 不匹配 → 412
	mustFail(t, condreq.Evaluate(hdr("If-Match", `"00000003-cafebabe"`), cur), "precondition_failed")
	// 多个候选里有一个匹配 → 放行
	mustPass(t, condreq.Evaluate(hdr("If-Match", `"00000001-aaaa", `+etag), cur))
	// * → 资源存在即放行
	mustPass(t, condreq.Evaluate(hdr("If-Match", "*"), cur))
	// * 但资源不存在 → 412
	mustFail(t, condreq.Evaluate(hdr("If-Match", "*"), condreq.Current{Exists: false}), "precondition_failed")
	// 具体 ETag 但资源不存在 → 412
	mustFail(t, condreq.Evaluate(hdr("If-Match", etag), condreq.Current{Exists: false}), "precondition_failed")
}

// 资源有 If-Match 但服务端给不出 ETag(例如目录)→ 必须拒绝,不能放行。
// 放行等于把"无法校验"当成"校验通过"。
func TestIfMatchWithoutServerETagIsRejected(t *testing.T) {
	res := condreq.Evaluate(hdr("If-Match", etag), condreq.Current{ETag: "", Exists: true})
	mustFail(t, res, "precondition_failed")
	if res.Message == "" {
		t.Error("应给出可诊断的说明")
	}
}

// ---- If-None-Match ----

func TestIfNoneMatch(t *testing.T) {
	cur := condreq.Current{ETag: etag, Exists: true}

	// * 且资源存在 → 失败(仅新建语义)
	res := condreq.Evaluate(hdr("If-None-Match", "*"), cur)
	mustFail(t, res, "precondition_failed")
	// * 且资源不存在 → 放行
	mustPass(t, condreq.Evaluate(hdr("If-None-Match", "*"), condreq.Current{Exists: false}))
	// ETag 匹配 → 失败,但 reason 是 not_modified(读场景走 304)
	mustFail(t, condreq.Evaluate(hdr("If-None-Match", etag), cur), "not_modified")
	// ETag 不匹配 → 放行
	mustPass(t, condreq.Evaluate(hdr("If-None-Match", `"00000009-zzzz"`), cur))
}

// ---- 两个头同时出现 ----

// RFC 未定义优先级 → 显式拒绝。猜错会放过一次覆盖冲突。
func TestBothConditionalHeadersRejected(t *testing.T) {
	res := condreq.Evaluate(hdr("If-Match", etag, "If-None-Match", "*"),
		condreq.Current{ETag: etag, Exists: true})
	mustFail(t, res, "precondition_failed")
}

// ---- 弱比较前缀 ----

// 客户端可能把别处学来的 `W/"..."` 发回来:按字面比较会永远不匹配
// (症状是"明明拿了 ETag 却总是 412")。去掉前缀按强比较。
func TestWeakPrefixTolerated(t *testing.T) {
	cur := condreq.Current{ETag: etag, Exists: true}
	mustPass(t, condreq.Evaluate(hdr("If-Match", "W/"+etag), cur))
	mustPass(t, condreq.Evaluate(hdr("If-Match", `W/`+etag), cur))
}

// 但没有引号的裸值不匹配(服务端发的是带引号形式,比较口径必须一致)
func TestBareETagDoesNotMatch(t *testing.T) {
	cur := condreq.Current{ETag: etag, Exists: true}
	res := condreq.Evaluate(hdr("If-Match", "00000002-deadbeef"), cur)
	mustFail(t, res, "precondition_failed")
}

// ---- If-Range ----

func TestIfRangeAllows(t *testing.T) {
	if !condreq.IfRangeAllows("", etag) {
		t.Error("未带 If-Range 应允许 Range")
	}
	if !condreq.IfRangeAllows(etag, etag) {
		t.Error("If-Range 与当前 ETag 匹配应允许 Range")
	}
	if condreq.IfRangeAllows(`"00000001-other"`, etag) {
		t.Error("If-Range 不匹配必须退化为完整响应(不允许 Range)")
	}
	// 日期形式的 If-Range:本项目一律按不匹配处理(安全降级为完整下载)
	if condreq.IfRangeAllows("Wed, 21 Oct 2026 07:28:00 GMT", etag) {
		t.Error("日期形式 If-Range 应退化为完整响应")
	}
}

// 如果两个头都没有 → 不干预
func TestNoConditionalHeadersPasses(t *testing.T) {
	mustPass(t, condreq.Evaluate(http.Header{}, condreq.Current{ETag: etag, Exists: true}))
	mustPass(t, condreq.Evaluate(http.Header{}, condreq.Current{Exists: false}))
}

// ListContains 与 Evaluate 必须同口径(避免"两处解析"分叉)
func TestListContainsMatchesEvaluate(t *testing.T) {
	cur := condreq.Current{ETag: etag, Exists: true}
	h := hdr("If-Match", `"a", `+etag+`, "b"`)
	if !condreq.ListContains(h.Get("If-Match"), etag) {
		t.Fatal("ListContains 应找到列表中的 ETag")
	}
	mustPass(t, condreq.Evaluate(h, cur))
	if condreq.ListContains(h.Get("If-Match"), `"zzz"`) {
		t.Fatal("ListContains 不应命中不存在的 ETag")
	}
}
