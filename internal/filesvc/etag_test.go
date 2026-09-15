package filesvc_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

// R-21 契约:ETag 的**唯一**加引号出口是 filesvc.QuoteETag。
//
// 这组用例的价值在于:三个出口(list/detail 响应体、HTTP 头、WebDAV PROPFIND)
// 各自单独看都"没错",只有把它们放在一起比字节才会发现不一致 ——
// 而不一致的后果是客户端条件请求**恒 412**,且现象离原因极远。

func TestQuoteETag(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"裸值加引号", "00000001-abcdef01", `"00000001-abcdef01"`},
		{"幂等:已带引号不重复加", `"00000001-abcdef01"`, `"00000001-abcdef01"`},
		{"去空白", "  00000001-abcdef01  ", `"00000001-abcdef01"`},
		{"空值返回空(调用方应跳过该头)", "", ""},
		{"纯空白返回空", "   ", ""},
		{"目录(version-00000000)也合法", "00000003-00000000", `"00000003-00000000"`},
		{"单引号不视为已引号", "'abc'", `"'abc'"`},
		{"只有一个引号不算已引号", `"abc`, `""abc"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := filesvc.QuoteETag(c.in); got != c.want {
				t.Fatalf("QuoteETag(%q) = %q,期望 %q", c.in, got, c.want)
			}
		})
	}
}

// 生成列产出的是**裸值**;QuoteETag 绝不能把数据库里的值当成已引号。
func TestQuoteETagOnGeneratedColumnValue(t *testing.T) {
	// files.etag = lpad(to_hex(version),8,'0') || '-' || substr(hash,1,8)
	raw := "00000002-deadbeef"
	got := filesvc.QuoteETag(raw)
	if strings.Contains(got, `""`) {
		t.Fatalf("不得出现双引号叠加: %q", got)
	}
	if got != `"`+raw+`"` {
		t.Fatalf("期望 %q,实际 %q", `"`+raw+`"`, got)
	}
}

func TestWriteETagHeader(t *testing.T) {
	h := http.Header{}
	filesvc.WriteETagHeader(h.Set, "00000001-abcdef01")
	if got := h.Get("ETag"); got != `"00000001-abcdef01"` {
		t.Fatalf("应写带引号的 ETag,got %q", got)
	}

	// 空值必须**完全不写头**,而不是写 `ETag: ""`
	// (后者是合法但永不匹配的值,会让条件请求陷入永远 412)
	h2 := http.Header{}
	filesvc.WriteETagHeader(h2.Set, "")
	if _, ok := h2["Etag"]; ok {
		t.Fatalf("空 etag 不应写响应头,实际 %v", h2)
	}
}

// View 产出的 etag 字段与响应头出口必须**字节一致**。
func TestViewAndHeaderETagAgree(t *testing.T) {
	f := &model.File{
		ID: "f-1", Name: "a.txt", Size: 10, MimeType: "text/plain",
		Version: 7, Etag: "00000007-11223344", IsDir: false,
	}
	view := filesvc.View(f)
	rec := httptest.NewRecorder()
	filesvc.WriteETagHeader(rec.Header().Set, view.ETag)

	if view.ETag != `"00000007-11223344"` {
		t.Fatalf("视图 etag 应为带引号形式,实际 %q", view.ETag)
	}
	if hdr := rec.Header().Get("ETag"); hdr != view.ETag {
		t.Fatalf("响应头 ETag(%q) 与响应体 etag(%q) 不一致 —— 客户端条件请求会恒 412", hdr, view.ETag)
	}
}

// 版本变化 → etag 必须变化(它是强校验器,不能标成弱 ETag)。
func TestETagChangesWithVersion(t *testing.T) {
	base := &model.File{ID: "f", Name: "a.txt", Etag: "00000001-abcdef01"}
	bumped := &model.File{ID: "f", Name: "a.txt", Etag: "00000002-abcdef01"}
	if filesvc.View(base).ETag == filesvc.View(bumped).ETag {
		t.Fatal("版本变化后 ETag 必须变化")
	}
}

// 空 etag 的文件(数据异常)不应产出 `""`
func TestViewEmptyETag(t *testing.T) {
	v := filesvc.View(&model.File{ID: "f", Name: "a.txt"})
	if v.ETag != "" {
		t.Fatalf("空 etag 应保持空,实际 %q", v.ETag)
	}
}

// ---- 表驱动:生成列的 ETag 契约(R-20 / R-21)----

// 用例要的是"hash 前 8 位真的不同",而 mkFile 造的 hash 是 %064x(len(name)+size)
// —— 小数值补零到 64 位后前 8 位恒为 00000000,区分不出"内容不同 → ETag 不同"。
// 所以这里直接指定完整 hash。
const (
	testHashA = "deadbeef" + "00000000000000000000000000000000000000000000000000000000"
	testHashB = "11223344" + "00000000000000000000000000000000000000000000000000000000"
	testHashC = "cafebabe" + "00000000000000000000000000000000000000000000000000000000"
)

// mkFileWithHash 直接插一行文件并指定 hash(绕开 finalize:本组用例只测生成列口径)。
func mkFileWithHash(t *testing.T, f *fixture, name, hash string, size, version int64) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, version, depth, hash_sha256, mime_type)
VALUES ($1, $2, $3, $4, false, $5, $6, 1, $7, 'application/octet-stream')
RETURNING id`, f.spaceID, f.rootID, f.userID, name, size, version, hash).Scan(&id); err != nil {
		t.Fatalf("插文件 %s 失败: %v", name, err)
	}
	return id
}

// TestETagGeneratedColumnContractTable 钉住 ETag 的**生成列契约**:
//
//	files.etag = lpad(to_hex(version), 8, '0') || '-' || coalesce(substr(hash_sha256,1,8), '00000000')
//	对外值      = QuoteETag(生成列裸值)   ← 唯一加引号出口,且刻意不加 W/ 弱标记
//
// 为什么必须表驱动:各分支(版本进制与补零、内容 hash、目录无 hash、同内容同版本
// 幂等)只差一两个字段;生成列一旦被改,没人知道该跑哪几个用例。期望值在测试里
// **按契约独立拼出来**(不复用实现),这样 SQL 被改坏时断言才会失败。
func TestETagGeneratedColumnContractTable(t *testing.T) {
	cases := []struct {
		name string
		// dir 为真时造目录(hash_sha256 为 NULL —— 生成列必须靠 coalesce 兜住),
		// 此时 version 必须是建行时的 1
		dir bool
		// fileName/size/hash 决定生成列的后半段(hash 前 8 位)
		fileName string
		size     int64
		hash     string
		version  int64
		// bumps 是造行后的版本自增次数(期望前半段变成 version+bumps)
		bumps int
		// twin 非空时再造一行"同版本"的孪生文件:
		// twinSame 为真 → hash 也相同 → ETag 必须**逐字节相同**;
		// 为假 → hash 不同 → 同版本但内容不同 → ETag 必须**不同**。
		twin     string
		twinSame bool
	}{
		{
			name:     "文件:8 位十六进制版本 + '-' + hash 前 8 位",
			fileName: "one.bin", size: 3, hash: testHashA, version: 1,
		},
		{
			// 能失败的关键用例:版本按**十六进制**编码并左补零。若哪天改成十进制
			// (或忘了 lpad),这里立刻变成 00004096 / 1000。
			name:     "文件:版本 4096 编成 00001000(十六进制,不是十进制)",
			fileName: "hex.bin", size: 7, hash: testHashA, version: 4096,
		},
		{
			// 能失败的关键用例:ETag 必须是**派生**的。若实现只在插入时算一次
			// (应用层自己维护 etag),版本自增后这里会仍是旧值。
			name:     "文件:版本自增后 ETag 必变(派生列,应用层不需要维护)",
			fileName: "bump.bin", size: 5, hash: testHashB, version: 1, bumps: 1,
		},
		{
			// 能失败的关键用例:同内容同版本必须得到同一个值。若 etag 里混入
			// updated_at / 随机盐,两行就不再相等 —— 客户端条件请求会恒 412。
			name:     "文件:同内容同版本 → ETag 逐字节相同",
			fileName: "dup-a", size: 5, hash: testHashA, version: 2,
			twin: "dup-b", twinSame: true,
		},
		{
			name:     "文件:同版本但内容 hash 不同 → ETag 必须不同",
			fileName: "dif-a", size: 5, hash: testHashA, version: 2,
			twin: "dif-b", twinSame: false,
		},
		{
			// 能失败的关键用例:目录的 hash_sha256 是 NULL,生成列靠 coalesce(...,'00000000')
			// 兜住,所以 ETag **不为空**。去掉 coalesce 会得到 NULL → 对外变成空串,
			// 目录从此没有强校验器。
			// 注:migrations/00001_init.sql 的行内注释写作"目录行为 NULL",与生成列
			// 表达式及 filesvc.QuoteETag 的注释不一致 —— 这里以**代码**为准。
			name: "目录:hash 为 NULL → 后半段 00000000,ETag 不为空",
			dir:  true, fileName: "docs", version: 1, bumps: 2,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()

			var id, wantPrefix string
			if c.dir {
				id = f.mkDir(t, f.rootID, c.fileName)
				wantPrefix = "00000000"
			} else {
				id = mkFileWithHash(t, f, c.fileName, c.hash, c.size, c.version)
				wantPrefix = c.hash[:8]
			}
			// 按 c.version 造行,再自增 bumps 次 —— 期望值跟着变成 version+bumps
			for i := 0; i < c.bumps; i++ {
				cur := c.version + int64(i)
				// 注意:Go 不允许在 if 的初始化处对复合字面量的方法调用做多值接收
				// (与 service_test.go 里的同一个坑),先取到变量再判断。
				_, terr := repo.FileRepo{}.TouchVersion(ctx, f.pool, id, cur)
				if terr != nil {
					t.Fatalf("第 %d 次版本自增失败(期望版本 %d): %v", i+1, cur, terr)
				}
			}
			wantRaw := fmt.Sprintf("%08x-%s", c.version+int64(c.bumps), wantPrefix)

			var raw string
			if err := f.pool.QueryRow(ctx, `SELECT etag FROM files WHERE id = $1`, id).Scan(&raw); err != nil {
				t.Fatalf("读生成列 etag 失败: %v", err)
			}
			if raw != wantRaw {
				t.Fatalf("生成列 etag 应为 %q(8 位十六进制版本 + '-' + hash 前 8 位),实际 %q", wantRaw, raw)
			}
			if strings.Contains(raw, `"`) {
				t.Fatalf("生成列必须是不含引号的裸值(R-21),实际 %q", raw)
			}

			// 对外出口:View(列表/详情)与 HTTP 头必须**字节一致**,且不带 W/ 弱标记
			view, err := f.svc.Get(ctx, f.userID, id)
			if err != nil {
				t.Fatalf("Get 失败: %v", err)
			}
			want := `"` + wantRaw + `"`
			if view.ETag != want {
				t.Fatalf("对外 ETag 应为 %q,实际 %q", want, view.ETag)
			}
			if strings.HasPrefix(view.ETag, "W/") {
				t.Fatalf("ETag 不能标成弱校验器(If-Match 要求强校验),实际 %q", view.ETag)
			}
			h := http.Header{}
			filesvc.WriteETagHeader(h.Set, raw)
			if got := h.Get("ETag"); got != view.ETag {
				t.Fatalf("响应头 ETag(%q)与响应体 etag(%q)不一致 —— 客户端条件请求会恒 412", got, view.ETag)
			}

			if c.twin == "" {
				return
			}
			twinHash := testHashC
			if c.twinSame {
				twinHash = c.hash
			}
			twinID := mkFileWithHash(t, f, c.twin, twinHash, c.size, c.version+int64(c.bumps))
			var twinRaw string
			if err := f.pool.QueryRow(ctx, `SELECT etag FROM files WHERE id = $1`, twinID).Scan(&twinRaw); err != nil {
				t.Fatalf("读孪生行 etag 失败: %v", err)
			}
			if c.twinSame && twinRaw != raw {
				t.Fatalf("同内容同版本必须得到同一个 ETag,实际 %q 与 %q", raw, twinRaw)
			}
			if !c.twinSame && twinRaw == raw {
				t.Fatalf("同版本但内容 hash 不同,ETag 必须不同,实际两行都是 %q", raw)
			}
		})
	}
}
