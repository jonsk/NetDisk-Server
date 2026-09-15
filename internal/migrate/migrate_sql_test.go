package migrate

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/netdisk/netdisk/internal/db"
)

// 本文件防的是**一类真实事故**(2026-09 生产踩到):
//
//	00013 把 `ALTER TABLE uploads ADD COLUMN allow_overwrite ...` 写在了
//	`-- +goose Down` **之后**,Up 段只剩一对空的 StatementBegin/StatementEnd。
//	goose 只按"文件是否执行过"记账,于是版本号照样推进到 13、日志照样打印
//	"successfully migrated database to version: 13",而库里根本没有这一列。
//	后果:服务端 INSERT uploads 带上新列名 → 500,建上传任务全挂;最阴的是
//	**测试库当时被人工 psql 补过列,所以测试全绿,只有生产是坏的**。
//
// 结论:迁移的**记账**不能当作**结构已生效**的证据。这里用两种独立方式兜住:
//	R1~R3 纯静态检查(无需数据库,CI 永远跑):
//	  R1 Up 段必须至少有一条可执行语句(空的 Up = 静默空操作);
//	  R2 StatementBegin/End 必须配对且块内非空;
//	  R3 Down 段不得出现只可能属于 Up 的语句(ADD COLUMN/CREATE ...)。
//	R4 连真实数据库,断言列**真的存在**(而不是 goose 说存在)。

// upExecutableLine 判断一行(已去注释、已 trim)是否是可执行语句的一部分。
func isComment(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "--")
}

// executableSQL 去掉空行、注释行与 goose 标记行,返回剩下的 SQL 文本。
func executableSQL(section string) string {
	var kept []string
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || isComment(trimmed) {
			continue
		}
		if strings.HasPrefix(trimmed, "+goose StatementBegin") ||
			strings.HasPrefix(trimmed, "+goose StatementEnd") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// TestMigrationUpSectionsHaveStatements 静态检查每条迁移的 Up 段(R1~R3)。
func TestMigrationUpSectionsHaveStatements(t *testing.T) {
	entries, err := sqlFS.ReadDir("sql")
	if err != nil {
		t.Fatalf("读取内嵌迁移目录失败: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatal("没有找到任何迁移文件(embed 路径 sql/*.sql 是否变了?)")
	}
	sort.Strings(names)

	// Down 段里出现这些,几乎只能是"把 Up 的语句写错了位置"。
	upOnlyInDown := regexp.MustCompile(`(?i)^\s*(ALTER\s+TABLE\s+\S+\s+ADD\s+COLUMN|CREATE\s+(TABLE|INDEX|UNIQUE|TYPE|SCHEMA|EXTENSION))\b`)

	for _, name := range names {
		name := name
		t.Run(name, func(t *testing.T) {
			raw, err := sqlFS.ReadFile("sql/" + name)
			if err != nil {
				t.Fatalf("读取失败: %v", err)
			}
			text := string(raw)

			upIdx := strings.Index(text, "+goose Up")
			if upIdx < 0 {
				t.Fatalf("缺少 `-- +goose Up` 标记;goose 需要它来界定向上迁移")
			}
			downIdx := strings.Index(text, "+goose Down")
			upSection, downSection := text[upIdx:], ""
			if downIdx > upIdx {
				upSection, downSection = text[upIdx:downIdx], text[downIdx:]
			}

			// R1:Up 段必须有可执行语句。
			if body := executableSQL(upSection); !strings.Contains(body, ";") {
				t.Errorf("R1 不通过:Up 段没有任何可执行语句(只有注释/空块)。"+
					"这种迁移会被 goose 记账为「已应用」却什么都不做,库里结构与版本号长期不一致。Up 段实际内容=%q",
					strings.TrimSpace(body))
			}

			// R2:StatementBegin/End 配对,且块内非空。
			beginCount := strings.Count(upSection, "+goose StatementBegin")
			endCount := strings.Count(upSection, "+goose StatementEnd")
			if beginCount != endCount {
				t.Errorf("R2 不通过:StatementBegin=%d 与 StatementEnd=%d 不配对", beginCount, endCount)
			}
			if beginCount > 0 {
				blocks := strings.Split(upSection, "+goose StatementBegin")
				for i, blk := range blocks[1:] {
					if end := strings.Index(blk, "+goose StatementEnd"); end >= 0 {
						blk = blk[:end]
					}
					if executableSQL(blk) == "" {
						t.Errorf("R2 不通过:第 %d 个 StatementBegin/End 块内没有任何 SQL(空块会被 goose 跳过,DDL 就「消失」了)", i+1)
					}
				}
			}

			// R3:Down 段不得出现只可能属于 Up 的语句。
			if downSection != "" {
				for _, line := range strings.Split(downSection, "\n") {
					if isComment(line) {
						continue
					}
					if upOnlyInDown.MatchString(line) {
						t.Errorf("R3 不通过:Down 段出现只可能属于 Up 的语句 %q —— "+
							"几乎可以肯定 Up 的语句被写到了 `-- +goose Down` 之后", strings.TrimSpace(line))
					}
				}
			}
		})
	}
}

// TestUploadsAllowOverwriteColumnApplied 断言 v13 的列在真实库里**真的存在**(R4)。
//
// 只看 goose 版本号是不够的:上面那次事故里版本号=13 而列不存在。
func TestUploadsAllowOverwriteColumnApplied(t *testing.T) {
	dsn := os.Getenv("NETDISK_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NETDISK_TEST_DSN,跳过需要数据库的测试")
	}
	ctx := context.Background()
	database, err := db.OpenWithDSN(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(database.Close)

	if err := Up(ctx, database.Pool); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	// **必须限定 schema**:`information_schema.columns` 是**全库**视图。别的用例
	// (迁移探针会把 12 条迁移整套跑在一个全新 schema 上)在并行运行时会建出
	// 第二张 `uploads`,于是这里查到 2 行 → 在"列其实好着"的情况下报 R4 失败。
	// 实测(`go test ./...` 整跑时命中,单独跑该包永远通过)—— 与仓内多次记录的
	// "共享表上的断言必须限定到自己那一份"是同一个教训。
	const q = `SELECT count(*) FROM information_schema.columns
	           WHERE table_schema = current_schema()
	             AND table_name = 'uploads' AND column_name = 'allow_overwrite'
	             AND is_nullable = 'NO' AND column_default LIKE '%false%'`
	var n int
	if err := database.Pool.QueryRow(ctx, q).Scan(&n); err != nil {
		t.Fatalf("查询列信息失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("R4 不通过:uploads.allow_overwrite 不存在或不是 NOT NULL DEFAULT false(命中行数=%d)。"+
			"注意 goose 版本号可能仍显示 13 —— 记账 ≠ 结构生效,请检查 00013 的 Up 段是否真的建了列", n)
	}
}
