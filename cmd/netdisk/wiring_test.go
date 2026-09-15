package main

import (
	"os"
	"strings"
	"testing"
)

// 后台 worker **接线门禁**:每个"后台巡视"包都必须在 `main.go` 里既被构造、
// 又被真的驱动起来。
//
// 为什么需要这条(2026-09-13 真机缺陷):`internal/lifecycle`(对象延迟删除)
// 有完整实现 —— 四态状态机、对象级会话锁、看护复位、12 项真实 PG 集成用例 ——
// 但 `main.go` 从未构造它。于是"删除文件"只把对象置成 `pending_delete` 就停了,
// 物理文件永远留在盘上,`policy.object_delete_delay_hours` 形同虚设。
//
// 这个缺口**在常规测试里完全看不见**:包自己的用例全绿(它们手工构造 Service),
// 对象巡检也看不出(它比的是 `state <> 'deleted'` 的字节和,卡住的 pending_delete
// 仍在其中)。教训很直接:**"有实现 + 有用例"不等于"在跑"**,而"在跑"这件事
// 只能靠"生产入口里确实启动了它"来保证。
//
// 判据刻意做成"源码级、零依赖":不引入任何测试专用导出、不依赖运行进程。
// 代价是断言的是文本而非行为 —— 所以每条同时要求"构造"与"驱动"两个串都在,
// 只写一半(构造了但没调用)同样失败。
func TestEveryBackgroundWorkerIsWiredInMain(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go 失败: %v", err)
	}
	src := string(raw)

	cases := []struct {
		name string
		// construct 是"worker 被构造"的判据
		construct string
		// drive 是"worker 被真的跑起来"的判据
		drive string
	}{
		{
			name:      "对象延迟删除(lifecycle)",
			construct: "&lifecycle.Service{",
			drive:     "lifecycleWorker.RunOnce(",
		},
		{
			name:      "暂存区回收(uploadsvc.StageCleaner)",
			construct: "&uploadsvc.StageCleaner{",
			drive:     "stageCleaner.RunOnce(",
		},
		{
			name:      "对象巡检(patrol)",
			construct: "&patrol.Service{",
			drive:     "patrolService.RunOnce(",
		},
		{
			name:      "配额对账(quotareconcile)",
			construct: "&quotareconcile.Service{",
			drive:     "reconciler.RunOnce(",
		},
		{
			name:      "目录级异步任务(dirops.Worker)",
			construct: "&dirops.Worker{",
			drive:     "dirOpWorker.RunOnce(",
		},
	}
	for _, c := range cases {
		if !strings.Contains(src, c.construct) {
			t.Errorf("%s 没有在 main.go 里被构造:包实现了但**没接线**,"+
				"表现为功能静默失效(用例仍全绿)", c.name)
		}
		if !strings.Contains(src, c.drive) {
			t.Errorf("%s 被构造了但没有被驱动(找不到 %q):"+
				"同样等于没接线", c.name, c.drive)
		}
	}
}

// 后端选择与"库/后端一致性自检"必须在生产入口里**真的接上**(BE-S5-13 / V2.79)。
//
// 为什么用源码级断言:`Validate()` 会拦住未实现的后端(有用例),但那是**第一层**;
// main.go 里的守卫是第二层(防止有人绕过校验直接进 run),它没有运行期信号 ——
// 删掉它所有单测照样全绿,而一旦真的有人绕过校验,服务就会带着未实现的后端起来。
// 与上面"worker 接线"同一思路:接线这件事必须有一条机械断言盯着。
func TestStorageBackendGuardIsWiredInMain(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go 失败: %v", err)
	}
	src := string(raw)
	for _, want := range []string{
		"config.StorageBackendImplemented(cfg.Storage.Backend)", // 未实现 → 拒绝装配
		"config.CheckBackendMismatch(",                          // 库/后端一致性 → 拒绝启动或 WARN
		"file_objects WHERE storage_backend <> $1",              // 一致性判断的输入来自真实查询
	} {
		if !strings.Contains(src, want) {
			t.Errorf("main.go 里找不到 %q:后端选择/一致性自检没有接线,"+
				"配了未实现的后端也可能带着 fs 起来(数据会写到别处且无报错)", want)
		}
	}
}
