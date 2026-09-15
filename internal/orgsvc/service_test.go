package orgsvc_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/orgsvc"
	"github.com/netdisk/netdisk/internal/repo"
)

// 部门树闭包表的真实 PG 集成测试(BE-S3-01)。
// 未设置 NETDISK_TEST_DSN 时全部 skip。

type fixture struct {
	svc      *orgsvc.Service
	database *db.DB
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("NETDISK_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NETDISK_TEST_DSN,跳过需要数据库的测试")
	}
	ctx := context.Background()
	database, err := db.OpenWithDSN(ctx, dsn, 8)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(database.Close)
	if err := migrate.Up(ctx, database.Pool); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	// 与 api 包的部门接口用例串行化(闭包表重建是全库操作,见 testlock_test.go)
	lockDeptTests(t, dsn)
	_ = snapshotCounts

	// 每个用例在自己的根下建树,避免互相污染。
	// 用一个带时间戳的标识做部门名前缀,清理时按前缀删。
	prefix := "org_it_" + time.Now().Format("150405.000000000")
	t.Cleanup(func() {
		_, _ = database.Pool.Exec(context.Background(),
			`DELETE FROM departments WHERE name LIKE $1`, prefix+"%")
	})

	// 清掉本套用例此前遗留的部门(含之前运行崩溃留下的)。
	//
	// 闭包表重建是**全库**操作(TRUNCATE + 全量 INSERT),所以测试库必须保证
	// 没有其它用例/残留数据制造的"数据环",否则 Rebuild 的自洽性校验会如实报错 ——
	// 那不是代码 bug,而是共用库缺少"回到干净状态"的手段。
	//
	// 依赖迁移 00004:user_departments.department_id 已改为 ON DELETE CASCADE,
	// 因此这里可以一次性清空全部部门,不必先手工拆关联。
	if _, err := database.Pool.Exec(ctx, `DELETE FROM user_departments`); err != nil {
		t.Fatalf("清理用户部门关联失败: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `DELETE FROM department_closure`); err != nil {
		t.Fatalf("清理闭包表失败: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `DELETE FROM departments`); err != nil {
		t.Fatalf("清理历史测试部门失败: %v", err)
	}

	svc := &orgsvc.Service{Depts: repo.DeptRepo{}, DB: db.AsQuerier(database)}
	return &fixture{svc: svc, database: database}
}

// create 建部门并返回 id(名前缀由 t.Name 保证可清理)。
func (f *fixture) create(t *testing.T, parent, name string) string {
	t.Helper()
	d, err := f.svc.Create(context.Background(), orgsvc.CreateInput{ParentID: parent, Name: name})
	if err != nil {
		t.Fatalf("建部门 %s 失败: %v", name, err)
	}
	if d.ID == "" {
		t.Fatalf("建部门 %s 未返回 id", name)
	}
	return d.ID
}

// closureRows 读回某部门在闭包表中的 (ancestor, depth) 集合。
func (f *fixture) closureRows(t *testing.T, deptID string) map[string]int {
	t.Helper()
	rows, err := f.database.Pool.Query(context.Background(),
		`SELECT ancestor_id::text, depth FROM department_closure WHERE descendant_id = $1`, deptID)
	if err != nil {
		t.Fatalf("读闭包行失败: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var a string
		var d int
		if err := rows.Scan(&a, &d); err != nil {
			t.Fatalf("扫描闭包行失败: %v", err)
		}
		out[a] = d
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历闭包行失败: %v", err)
	}
	return out
}

func (f *fixture) mustSubtree(t *testing.T, deptID string) *repo.Subtree {
	t.Helper()
	st, err := f.svc.Subtree(context.Background(), deptID)
	if err != nil {
		t.Fatalf("取子树失败: %v", err)
	}
	return st
}

// ---- 基本闭包语义 ----

// 建三层树后,每个节点的闭包行必须是"自己 + 全部祖先",且 depth 正确。
func TestCreateMaintainsClosure(t *testing.T) {
	f := setup(t)
	root := f.create(t, "", "org_it_总部")
	mid := f.create(t, root, "org_it_研发中心")
	leaf := f.create(t, mid, "org_it_平台组")

	got := f.closureRows(t, leaf)
	want := map[string]int{leaf: 0, mid: 1, root: 2}
	if len(got) != len(want) {
		t.Fatalf("叶子应有 3 条闭包行,实际 %d: %v", len(got), got)
	}
	for id, depth := range want {
		if got[id] != depth {
			t.Errorf("闭包 depth 不对: %s 期望 %d 实际 %d", id, depth, got[id])
		}
	}

	// 根只有自指行
	if got := f.closureRows(t, root); len(got) != 1 || got[root] != 0 {
		t.Fatalf("根应只有 depth=0 自指行,实际 %v", got)
	}
}

// 子树查询必须恰好包含后代(不多不少),且 depth 相对根计算。
func TestSubtreeQuery(t *testing.T) {
	f := setup(t)
	root := f.create(t, "", "org_it_根")
	a := f.create(t, root, "org_it_A")
	b := f.create(t, a, "org_it_B")
	c := f.create(t, a, "org_it_C")
	other := f.create(t, "", "org_it_无关根")
	f.create(t, other, "org_it_无关子")

	st := f.mustSubtree(t, a)
	ids := map[string]bool{}
	for _, n := range st.Nodes {
		ids[n.ID] = true
	}
	if len(ids) != 3 {
		t.Fatalf("A 的子树应有 3 个节点(A/B/C),实际 %d: %v", len(ids), ids)
	}
	for _, want := range []string{a, b, c} {
		if !ids[want] {
			t.Errorf("子树缺少节点 %s", want)
		}
	}
	if st.Depth[a] != 0 || st.Depth[b] != 1 || st.Depth[c] != 1 {
		t.Errorf("相对深度不对: a=%d b=%d c=%d", st.Depth[a], st.Depth[b], st.Depth[c])
	}
	if ids[other] {
		t.Error("子树不得包含无关分支的节点")
	}

	// 祖先链:从直接上级到根
	anc, err := repo.DeptRepo{}.AncestorsOf(context.Background(), f.database.Pool, c)
	if err != nil {
		t.Fatalf("取祖先链失败: %v", err)
	}
	if len(anc) != 2 || anc[0].ID != a || anc[1].ID != root {
		t.Fatalf("祖先链应为 [A, root],实际 %v", deptIDs(anc))
	}
}

func deptIDs(ds []*model.Department) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Name)
	}
	return out
}

// ---- 全量重建的幂等性(BE-S3-01 核心验收)----

// 破坏闭包表后重建,内容必须完全恢复;再重建一次结果不变(幂等)。
func TestRebuildIsIdempotentAndRepairs(t *testing.T) {
	f := setup(t)
	root := f.create(t, "", "org_it_根")
	a := f.create(t, root, "org_it_A")
	b := f.create(t, a, "org_it_B")

	first, err := f.svc.Rebuild(context.Background())
	if err != nil {
		t.Fatalf("首次重建失败: %v", err)
	}
	if !first.Rebuilt {
		t.Fatal("重建应成功")
	}
	if first.MaxDepth < 2 {
		t.Fatalf("最大深度应 ≥2,实际 %d", first.MaxDepth)
	}
	baseline := f.closureRows(t, b)
	if len(baseline) != 3 {
		t.Fatalf("A→B→root 链上 B 应有 3 条闭包行(自己/父亲/祖父),实际 %d: %v", len(baseline), baseline)
	}

	// 人为破坏:删掉 b 的祖父闭包行(depth=2),剩下的应比 baseline 少一行
	if _, err := f.database.Pool.Exec(context.Background(),
		`DELETE FROM department_closure WHERE descendant_id = $1 AND depth = 2`, b); err != nil {
		t.Fatalf("破坏闭包表失败: %v", err)
	}
	broken := f.closureRows(t, b)
	if len(broken) != len(baseline)-1 {
		t.Fatalf("破坏后应比 baseline 少一行(期望 %d,实际 %d): %v",
			len(baseline)-1, len(broken), broken)
	}

	second, err := f.svc.Rebuild(context.Background())
	if err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	if !second.Rebuilt {
		t.Fatal("重建应成功")
	}
	restored := f.closureRows(t, b)
	if len(restored) != len(baseline) {
		t.Fatalf("重建未恢复闭包行: 期望 %d 条,实际 %d: %v", len(baseline), len(restored), restored)
	}
	for id, depth := range baseline {
		if restored[id] != depth {
			t.Errorf("重建后 depth 不一致: %s 期望 %d 实际 %d", id, depth, restored[id])
		}
	}

	// 幂等:第三次重建行数不变
	third, err := f.svc.Rebuild(context.Background())
	if err != nil {
		t.Fatalf("第三次重建失败: %v", err)
	}
	if third.ClosureRows != second.ClosureRows {
		t.Fatalf("重建不幂等: 第二次 %d 行,第三次 %d 行", second.ClosureRows, third.ClosureRows)
	}
}

// 空树(无部门)重建必须成功且不报错。
func TestRebuildEmpty(t *testing.T) {
	f := setup(t)
	// 清空全部部门(测试库专用;有成员/空间引用时跳过)
	if _, err := f.database.Pool.Exec(context.Background(), `DELETE FROM departments`); err != nil {
		t.Skipf("清空部门失败(可能有引用): %v", err)
	}
	res, err := f.svc.Rebuild(context.Background())
	if err != nil {
		t.Fatalf("空树重建应成功: %v", err)
	}
	if res.ClosureRows != 0 || res.MaxDepth != 0 {
		t.Fatalf("空树应为 0 行 0 深度,实际 %+v", res)
	}
}

// 回归:重建必须为**每个**节点写自指行,且行数等于"节点数 + 祖先关系总数"。
//
// 这条用例专门盯住一个曾经真实存在的 bug:重建的递归方向写反成
// "anchor=根、沿 parent_id 向下展开",产出的行只有 `(根, 后代, 深度)`。
// 对**链状**树它只少行、深度看着还对;只有在**多分支**树上才明显缺行
// (某些兄弟节点的自指行与中间祖先行永远不产生)。
//
// 因此这里刻意造两个分支,并用"期望行数公式"断言,而不是只查几个点。
func TestRebuildWritesSelfRowsForEveryNode(t *testing.T) {
	f := setup(t)
	//    root
	//    ├── a ── a1 ── a1x
	//    └── b ── b1
	root := f.create(t, "", "org_it_根")
	a := f.create(t, root, "org_it_a")
	a1 := f.create(t, a, "org_it_a1")
	a1x := f.create(t, a1, "org_it_a1x")
	b := f.create(t, root, "org_it_b")
	b1 := f.create(t, b, "org_it_b1")

	ctx := context.Background()
	if _, err := f.svc.Rebuild(ctx); err != nil {
		t.Fatalf("重建失败: %v", err)
	}

	// 1) 每个节点都必须有 depth=0 的自指行
	selfRows, err := f.database.Pool.Query(ctx,
		`SELECT descendant_id::text FROM department_closure WHERE depth = 0`)
	if err != nil {
		t.Fatalf("查自指行失败: %v", err)
	}
	got := map[string]bool{}
	for selfRows.Next() {
		var id string
		if err := selfRows.Scan(&id); err != nil {
			selfRows.Close()
			t.Fatalf("扫描失败: %v", err)
		}
		got[id] = true
	}
	selfRows.Close()
	for _, id := range []string{root, a, a1, a1x, b, b1} {
		if !got[id] {
			t.Errorf("节点 %s 缺少 depth=0 自指行(重建递归方向可能写反)", id[:8])
		}
	}
	if len(got) != 6 {
		t.Errorf("自指行应恰好 6 条,实际 %d", len(got))
	}

	// 2) 行数公式:Σ(每个节点的祖先数 + 1)
	//    root:0+1=1  a:1+1=2  a1:2+1=3  a1x:3+1=4  b:1+1=2  b1:2+1=3  → 15
	var total int
	if err := f.database.Pool.QueryRow(ctx,
		`SELECT count(*) FROM department_closure`).Scan(&total); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if total != 15 {
		t.Fatalf("闭包行数应为 15(=Σ(祖先数+1)),实际 %d —— 说明重建漏行", total)
	}

	// 3) 每个节点的 (自己,自己,0) 与最深祖先关系都要对
	for _, tc := range []struct {
		id    string
		name  string
		depth int
	}{{root, "根", 0}, {a, "a", 1}, {a1, "a1", 2}, {a1x, "a1x", 3}, {b, "b", 1}, {b1, "b1", 2}} {
		rows := f.closureRows(t, tc.id)
		if rows[tc.id] != 0 {
			t.Errorf("%s 自指行 depth 应为 0,实际 %d", tc.name, rows[tc.id])
		}
		if rows[root] != tc.depth {
			t.Errorf("%s 到根的 depth 应为 %d,实际 %d", tc.name, tc.depth, rows[root])
		}
		if len(rows) != tc.depth+1 {
			t.Errorf("%s 应有 %d 条闭包行,实际 %d: %v", tc.name, tc.depth+1, len(rows), rows)
		}
	}
}

// ---- 环防护 ----

// 把部门移到自己下面必须被拒(否则闭包表出现自环)。
func TestMoveToSelfRejected(t *testing.T) {
	f := setup(t)
	root := f.create(t, "", "org_it_根")
	a := f.create(t, root, "org_it_A")

	_, err := f.svc.Move(context.Background(), a, a)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 400 {
		t.Fatalf("移到自己下面应 400,得到 %v", err)
	}
	// 闭包表不得被污染
	if got := f.closureRows(t, a); got[a] != 0 || len(got) != 2 {
		t.Fatalf("拒绝移动后闭包表不应改变,实际 %v", got)
	}
}

// 把父部门移到自己的后代下必须被拒(数据环的主要来源)。
func TestMoveToOwnDescendantRejected(t *testing.T) {
	f := setup(t)
	root := f.create(t, "", "org_it_根")
	a := f.create(t, root, "org_it_A")
	b := f.create(t, a, "org_it_B")
	c := f.create(t, b, "org_it_C")

	// 把 A 移到 C(自己的孙节点)下面 → 环
	_, err := f.svc.Move(context.Background(), a, c)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 400 {
		t.Fatalf("移到自己的后代下应 400,得到 %v", err)
	}
	if !strings.Contains(ae.Message, "成环") {
		t.Errorf("错误文案应说明会成环,实际 %q", ae.Message)
	}

	// 原树结构必须完好
	st := f.mustSubtree(t, a)
	if len(st.Nodes) != 3 {
		t.Fatalf("拒绝移动后 A 子树应仍为 3 个节点,实际 %d", len(st.Nodes))
	}
	// 校验器必须认为树是自洽的
	problems, err := repo.DeptRepo{}.ValidateTree(context.Background(), f.database.Pool)
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("拒绝环移动后不应有自洽性问题: %+v", problems)
	}
}

// 合法移动:闭包表必须整体更新(旧祖先关系清除、新祖先关系建立)。
func TestMoveRebuildsClosure(t *testing.T) {
	f := setup(t)
	root := f.create(t, "", "org_it_根")
	left := f.create(t, root, "org_it_左")
	right := f.create(t, root, "org_it_右")
	sub := f.create(t, left, "org_it_子")

	// 移动前:sub 的祖先 = {left, root}
	before := f.closureRows(t, sub)
	if _, ok := before[left]; !ok {
		t.Fatal("移动前 sub 应在 left 下")
	}

	if _, err := f.svc.Move(context.Background(), sub, right); err != nil {
		t.Fatalf("合法移动失败: %v", err)
	}

	after := f.closureRows(t, sub)
	if after[sub] != 0 || after[right] != 1 || after[root] != 2 {
		t.Fatalf("移动后闭包不对: %v", after)
	}
	if _, stale := after[left]; stale {
		t.Fatalf("移动后不得残留旧祖先 left: %v", after)
	}
	if len(after) != 3 {
		t.Fatalf("移动后应恰好 3 条闭包行,实际 %d: %v", len(after), after)
	}

	// 两棵子树都自洽
	if n := len(f.mustSubtree(t, left).Nodes); n != 1 {
		t.Errorf("left 子树应只剩自己,实际 %d", n)
	}
	if n := len(f.mustSubtree(t, right).Nodes); n != 2 {
		t.Errorf("right 子树应为自己+sub,实际 %d", n)
	}
}

// ---- 删除 ----

// 有子部门时拒绝删除(不级联),并说明数量。
func TestDeleteRejectsNonLeaf(t *testing.T) {
	f := setup(t)
	root := f.create(t, "", "org_it_根")
	f.create(t, root, "org_it_子1")
	f.create(t, root, "org_it_子2")

	err := f.svc.Delete(context.Background(), root)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 409 {
		t.Fatalf("有子部门应 409,得到 %v", err)
	}
	if !strings.Contains(ae.Message, "2") {
		t.Errorf("应说明子部门数量,实际 %q", ae.Message)
	}
}

// 删叶子后:该节点的闭包行必须随之消失(ON DELETE CASCADE)。
func TestDeleteLeafRemovesClosure(t *testing.T) {
	f := setup(t)
	root := f.create(t, "", "org_it_根")
	leaf := f.create(t, root, "org_it_叶")

	if err := f.svc.Delete(context.Background(), leaf); err != nil {
		t.Fatalf("删叶子失败: %v", err)
	}
	var n int
	if err := f.database.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM department_closure WHERE descendant_id = $1 OR ancestor_id = $1`, leaf).Scan(&n); err != nil {
		t.Fatalf("统计闭包行失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("删节点后闭包行应清零,实际 %d", n)
	}
	// 删除不存在的部门 → 404
	if err := f.svc.Delete(context.Background(), leaf); err == nil {
		t.Fatal("重复删除应报错")
	}
}

// ---- 用户与部门关联 ----

func (f *fixture) newUser(t *testing.T, name string) string {
	t.Helper()
	var id string
	uname := name + "_" + time.Now().Format("150405.000000000")
	err := f.database.InTx(context.Background(), func(tx pgx.Tx) error {
		u, err := repo.UserRepo{}.Create(context.Background(), tx, repo.CreateInput{
			Username: uname, Email: uname + "@example.com", DisplayName: name, Role: model.RoleUser,
		})
		if err != nil {
			return err
		}
		id = u.ID
		return nil
	})
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.database.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// SetUserDepartments 是覆盖式:第二次设置必须清掉第一次的关联。
func TestSetUserDepartmentsIsReplaceAll(t *testing.T) {
	f := setup(t)
	root := f.create(t, "", "org_it_根")
	d1 := f.create(t, root, "org_it_部门1")
	d2 := f.create(t, root, "org_it_部门2")
	d3 := f.create(t, root, "org_it_部门3")
	uid := f.newUser(t, "org_it_用户")

	ctx := context.Background()
	if err := f.svc.SetUserDepartments(ctx, uid, []string{d1, d2, d3}, d2); err != nil {
		t.Fatalf("首次设置失败: %v", err)
	}
	depts, err := repo.DeptRepo{}.DepartmentsOfUser(ctx, f.database.Pool, uid)
	if err != nil {
		t.Fatalf("读用户部门失败: %v", err)
	}
	if len(depts) != 3 {
		t.Fatalf("应有 3 个部门,实际 %d", len(depts))
	}
	if depts[0].ID != d2 {
		t.Errorf("主部门应排在最前,实际首个为 %s", depts[0].Name)
	}

	// 覆盖式:只留 d3
	if err := f.svc.SetUserDepartments(ctx, uid, []string{d3}, d3); err != nil {
		t.Fatalf("第二次设置失败: %v", err)
	}
	depts, err = repo.DeptRepo{}.DepartmentsOfUser(ctx, f.database.Pool, uid)
	if err != nil {
		t.Fatalf("读用户部门失败: %v", err)
	}
	if len(depts) != 1 || depts[0].ID != d3 {
		t.Fatalf("覆盖式设置后应只剩 d3,实际 %v", deptIDs(depts))
	}
}

// 主部门不在列表内必须被拒(否则会写入一个指向未关联部门的 is_primary)。
func TestSetUserDepartmentsValidatesPrimary(t *testing.T) {
	f := setup(t)
	root := f.create(t, "", "org_it_根")
	d1 := f.create(t, root, "org_it_部门1")
	d2 := f.create(t, root, "org_it_部门2")
	uid := f.newUser(t, "org_it_用户2")

	err := f.svc.SetUserDepartments(context.Background(), uid, []string{d1}, d2)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 400 {
		t.Fatalf("主部门不在列表应 400,得到 %v", err)
	}
}

// UsersInSubtree 是"按部门授权"的基础:必须包含子树内全部成员,且去重。
func TestUsersInSubtree(t *testing.T) {
	f := setup(t)
	root := f.create(t, "", "org_it_根")
	child := f.create(t, root, "org_it_子")
	grand := f.create(t, child, "org_it_孙")
	sibling := f.create(t, root, "org_it_旁支")

	u1 := f.newUser(t, "org_it_u1")
	u2 := f.newUser(t, "org_it_u2")
	u3 := f.newUser(t, "org_it_u3")

	ctx := context.Background()
	// u1 在 child,同时(重复)在 grand;u2 只在 grand;u3 在旁支
	if err := f.svc.SetUserDepartments(ctx, u1, []string{child, grand}, child); err != nil {
		t.Fatalf("设置 u1 失败: %v", err)
	}
	if err := f.svc.SetUserDepartments(ctx, u2, []string{grand}, grand); err != nil {
		t.Fatalf("设置 u2 失败: %v", err)
	}
	if err := f.svc.SetUserDepartments(ctx, u3, []string{sibling}, sibling); err != nil {
		t.Fatalf("设置 u3 失败: %v", err)
	}

	got, err := repo.DeptRepo{}.UsersInSubtree(ctx, f.database.Pool, root)
	if err != nil {
		t.Fatalf("查子树用户失败: %v", err)
	}
	set := map[string]bool{}
	for _, id := range got {
		if set[id] {
			t.Fatalf("结果应去重,出现重复 %s", id)
		}
		set[id] = true
	}
	if !set[u1] || !set[u2] || !set[u3] {
		t.Fatalf("根子树应含 u1/u2/u3,实际 %v", got)
	}

	// 只看 child 子树:应含 u1(直接成员)、u2(孙节点成员),不含 u3
	got2, err := repo.DeptRepo{}.UsersInSubtree(ctx, f.database.Pool, child)
	if err != nil {
		t.Fatalf("查 child 子树用户失败: %v", err)
	}
	set2 := map[string]bool{}
	for _, id := range got2 {
		set2[id] = true
	}
	if !set2[u1] || !set2[u2] {
		t.Fatalf("child 子树应含 u1/u2,实际 %v", got2)
	}
	if set2[u3] {
		t.Fatal("child 子树不得包含旁支成员")
	}

	// 只看 grand:只应有 u1/u2
	got3, err := repo.DeptRepo{}.UsersInSubtree(ctx, f.database.Pool, grand)
	if err != nil {
		t.Fatalf("查 grand 子树用户失败: %v", err)
	}
	if len(got3) != 2 {
		t.Fatalf("grand 子树应恰好 2 人,实际 %d: %v", len(got3), got3)
	}
}

// ---- 命名与校验 ----

func TestCreateValidatesName(t *testing.T) {
	f := setup(t)
	_, err := f.svc.Create(context.Background(), orgsvc.CreateInput{Name: ""})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 400 {
		t.Fatalf("空名应 400,得到 %v", err)
	}
	// 部门名允许含 `/`(与文件名规则刻意不同)
	if _, err := f.svc.Create(context.Background(), orgsvc.CreateInput{Name: "org_it_研发/平台组"}); err != nil {
		t.Fatalf("含 `/` 的部门名应被接受(与 namepolicy 无关): %v", err)
	}
}

func TestCreateRejectsMissingParent(t *testing.T) {
	f := setup(t)
	_, err := f.svc.Create(context.Background(), orgsvc.CreateInput{
		ParentID: "00000000-0000-7000-8000-000000000000", Name: "org_it_孤儿",
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("父不存在应 404,得到 %v", err)
	}
}
