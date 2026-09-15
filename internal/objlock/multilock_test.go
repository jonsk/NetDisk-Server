package objlock_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// 多 hash 场景的锁序与互斥(BE-S5-02 验收④⑤ / 6.6 全局锁序)。
//
// 这组用例针对的是**经典死锁**:多对象路径(COPY 目录逐行引用+1、多文件共享)
// 若按"客户端给的顺序"取锁,两个并发请求可能以相反顺序取同两个 hash → 死锁。
// 统一按 hash 升序后,任何两个请求的取锁序列都是同一全序的子序列,不存在环。

// LockManyXact 必须**去重 + 升序**取锁,且在其中任一 hash 上互斥。
func TestLockManyXactAscendingAndMutuallyExclusive(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	hashes := []string{"ffff0000", "0000aaaa", "ffff0000", "", "8888cccc"}

	// 第一条事务持锁不退出
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = inTx(ctx, pool, func(tx pgx.Tx) error {
			if err := lockMany(t, ctx, tx, hashes); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	// 第二条事务尝试取**同一批** hash(顺序不同)—— 必须被阻塞
	secondStarted := make(chan struct{})
	go func() {
		_ = inTx(ctx, pool, func(tx pgx.Tx) error {
			// 故意用相反顺序:若实现不排序,这正是死锁的触发方式
			rev := []string{"8888cccc", "0000aaaa", "ffff0000"}
			if err := lockMany(t, ctx, tx, rev); err != nil {
				return err
			}
			close(secondStarted)
			return nil
		})
	}()

	select {
	case <-secondStarted:
		t.Fatal("同键多锁未互斥:第二条事务在第一条持锁期间就拿到了锁")
	case <-time.After(300 * time.Millisecond):
		// 正确:被阻塞
	}
	close(release)
	select {
	case <-secondStarted:
		// 正确:释放后拿到
	case <-time.After(5 * time.Second):
		t.Fatal("释放后第二条仍拿不到锁(锁未随事务提交释放?)")
	}
}

// 两个并发事务各自对**同一对** hash 取锁(顺序相反)→ 必须都能完成(不死锁)。
//
// 这是验收⑤的死锁回归用例。若实现不排序,两条事务会互相等待对方的第二把锁,
// 直到上下文超时 —— 用例会以超时失败。
func TestLockManyXactNoDeadlockOnReversedOrder(t *testing.T) {
	pool := openPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a := []string{"aaaa1111", "bbbb2222"}
	b := []string{"bbbb2222", "aaaa1111"} // 相反顺序

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, hs := range [][]string{a, b} {
		wg.Add(1)
		go func(i int, hs []string) {
			defer wg.Done()
			errs[i] = inTx(ctx, pool, func(tx pgx.Tx) error {
				if err := lockMany(t, ctx, tx, hs); err != nil {
					return err
				}
				// 模拟拿到锁后的工作(极短,让两条事务真正交错)
				time.Sleep(30 * time.Millisecond)
				return nil
			})
		}(i, hs)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("事务 #%d 失败(可能死锁): %v", i, err)
		}
	}
}

// 去重后同一 hash 只取一次锁(重复项不应导致重复取锁,更不应自锁)
func TestLockManyXactDedupes(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	err := inTx(ctx, pool, func(tx pgx.Tx) error {
		// 同一 hash 出现 4 次:PG 的 advisory 锁可重入(同一事务内重复取不阻塞),
		// 但去重后语义更清晰,也少几次往返
		return lockMany(t, ctx, tx, []string{"cccc3333", "cccc3333", "cccc3333", "cccc3333"})
	})
	if err != nil {
		t.Fatalf("重复 hash 不应报错: %v", err)
	}
}

// 空列表:不取任何锁,直接成功(不 panic)
func TestLockManyXactEmpty(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	if err := inTx(ctx, pool, func(tx pgx.Tx) error {
		return lockMany(t, ctx, tx, nil)
	}); err != nil {
		t.Fatalf("空列表应直接成功: %v", err)
	}
	if err := inTx(ctx, pool, func(tx pgx.Tx) error {
		return lockMany(t, ctx, tx, []string{"", ""})
	}); err != nil {
		t.Fatalf("全空串应被剔除并成功: %v", err)
	}
}

// TestLockManyXactOrderingTable 用**可观测结果**(此刻谁被锁住、谁仍空闲、多快
// 完成、是否报错)钉住 6.6 的全局锁序纪律,而不是去断言 normalizeHashes 之类的
// 内部实现 —— 内部实现可以变,"两个方向的请求不会互等"这条不能变。
//
// 为什么必须表驱动:锁序的每条规则都只在"并发此刻的状态"里可见,散成一堆用例后
// 没人说得清"请求顺序无关"这条到底被哪个用例钉住了。
func TestLockManyXactOrderingTable(t *testing.T) {
	cases := []struct {
		name string
		// give 是第一个事务请求的 hash 顺序(故意乱序/重复/含空串)
		give []string
		// holder 是"另一条事务"先占住的 hash(模拟并发请求已抢到其中一把)
		holder []string
		// second 非空时改为"两个并发事务"场景:它给出第二个事务的请求顺序。
		// 为什么要有它:单事务只能看阻塞位置,看不出"两个方向互等"的环。
		second []string
		// wantBlockedOn 非空表示:请求必须**先锁住更小的 hash**,再阻塞在它上面
		wantBlockedOn string
		// wantLockedWhileBlocked 是被阻塞期间必须已锁住的 hash —— 规范序的直接证据
		// (按调用方顺序取锁时,这些一把都拿不到)
		wantLockedWhileBlocked []string
		// wantFreeWhileBlocked 是被阻塞期间必须仍空闲的 hash(比阻塞点大的那些)
		wantFreeWhileBlocked []string
		// wantInsideLocks 是事务成功执行期间**本连接**持有的 advisory 锁数
		wantInsideLocks int
	}{
		{
			// **能失败的关键用例**:不排序的实现会一上来就阻塞在 bbbb,aaaa 一把都
			// 拿不到 —— 而 aaaa 正是并发请求可能持有的那把,"你等我、我等你"的环
			// 就是这么形成的(注释掉 normalizeHashes 里的排序即可复现)。
			name:                   "请求乱序时先锁住更小的 hash,再阻塞在更大的 hash 上",
			give:                   []string{"bbbb2222", "aaaa1111", "cccc3333"},
			holder:                 []string{"bbbb2222"},
			wantBlockedOn:          "bbbb2222",
			wantLockedWhileBlocked: []string{"aaaa1111"},
			wantFreeWhileBlocked:   []string{"cccc3333"},
		},
		{
			// 锁被占用时若"静默授予"(例如实现改用 try-lock 却不检查返回值),
			// 两个请求会同时进入临界区 —— 这里以"超时必须报错"钉住。
			name:          "锁已被他人持有:不得静默授予(超时必须以错误结束)",
			give:          []string{"dddd4444"},
			holder:        []string{"dddd4444"},
			wantBlockedOn: "dddd4444",
		},
		{
			// 去重后可观测的契约:事务内恰好一把锁,且能正常提交(不自锁)。
			// 若实现改成"每个重复项各开一条连接",同键就会互相等到超时。
			name:            "同一对象重复请求:幂等(事务内恰好一把锁)",
			give:            []string{"eeee5555", "eeee5555", "eeee5555"},
			wantInsideLocks: 1,
		},
		{
			name:            "空串被剔除:不取任何锁",
			give:            []string{"", ""},
			wantInsideLocks: 0,
		},
		{
			name:            "nil 列表:不取任何锁(不 panic)",
			give:            nil,
			wantInsideLocks: 0,
		},
		{
			// 为什么:6.6 死锁回归的正面形态 —— 两条事务对同一对 hash 的请求顺序
			// 相反;不排序时它们会各握一把、互等对方的第二把,直到上下文超时。
			name:   "两个事务以相反顺序请求同一对 hash:都必须完成(死锁回归)",
			give:   []string{"aaaa1111", "bbbb2222"},
			second: []string{"bbbb2222", "aaaa1111"},
		},
		{
			// 为什么:除顺序相反外,还让其中一组带重复项 —— 去重、踢空串必须与排序
			// 一起生效,否则"两条事务的取锁序列是同一全序的子序列"就不成立。
			name:   "两个事务请求同一组 3 个 hash(含重复项):仍按同一全序取锁,不死锁",
			give:   []string{"aaaa1111", "bbbb2222", "cccc3333"},
			second: []string{"cccc3333", "cccc3333", "bbbb2222", "aaaa1111"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := openPool(t)
			ctx := context.Background()

			// 两个并发事务:表里给的是**相反/不同的请求顺序**,只有按同一升序取锁
			// 才可能都完成(否则互相等对方的第二把锁 → 死锁到上下文超时)。
			//
			// 说明:这条用例是**概率性**的回归网 —— 死锁需要两条事务"各握一把、
			// 再同时去要对方那把",所以用起跑线让它们尽可能同时发出请求;
			// 确定性的顺序证据在下面"先锁住更小的 hash"那条用例里(它能对着
			// 未排序的实现稳定失败)。
			if len(c.second) > 0 {
				orders := [][]string{c.give, c.second}
				reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				var wg sync.WaitGroup
				ready := sync.WaitGroup{}
				errs := make([]error, len(orders))
				start := make(chan struct{})
				ready.Add(len(orders))
				for i, hs := range orders {
					wg.Add(1)
					go func(i int, hs []string) {
						defer wg.Done()
						ready.Done()
						<-start // 等两条事务都就位再一起发请求
						errs[i] = inTx(reqCtx, pool, func(tx pgx.Tx) error {
							if err := lockMany(t, reqCtx, tx, hs); err != nil {
								return err
							}
							// 持锁做一点工作,让两条事务真正交错:不交错就退化成串行,
							// 死锁根本测不出来
							time.Sleep(30 * time.Millisecond)
							return nil
						})
					}(i, hs)
				}
				ready.Wait()
				close(start)
				wg.Wait()
				for i, err := range errs {
					if err != nil {
						t.Fatalf("并发事务 #%d(请求顺序 %v)失败: %v —— 反向取锁也必须不死锁",
							i, orders[i], err)
					}
				}
				return
			}

			// 先由"另一条事务"占住 holder(模拟并发请求已持有其中一把)
			var releaseHolder func()
			if len(c.holder) > 0 {
				held := make(chan struct{})
				release := make(chan struct{})
				holderErr := make(chan error, 1)
				go func() {
					holderErr <- inTx(ctx, pool, func(tx pgx.Tx) error {
						if err := lockMany(t, ctx, tx, c.holder); err != nil {
							return err
						}
						close(held)
						<-release
						return nil
					})
				}()
				<-held
				released := false
				releaseHolder = func() {
					if released {
						return
					}
					released = true
					close(release)
					if err := <-holderErr; err != nil {
						t.Fatalf("持锁事务失败: %v", err)
					}
				}
			}
			defer func() {
				if releaseHolder != nil {
					releaseHolder()
				}
			}()

			// 目标事务:先把后端 pid 报出来 —— 一旦阻塞在锁上,它自己就没法再报告
			// "我现在锁住了哪些 hash",只能从 pg_locks 旁观。
			reqCtx, cancel := context.WithTimeout(ctx, 900*time.Millisecond)
			defer cancel()
			pidCh := make(chan int, 1)
			insideLocks := make(chan int, 1)
			doneCh := make(chan error, 1)
			go func() {
				doneCh <- inTx(reqCtx, pool, func(tx pgx.Tx) error {
					var pid int
					if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
						return err
					}
					pidCh <- pid
					if err := lockMany(t, reqCtx, tx, c.give); err != nil {
						return err
					}
					// 在事务内数自己持有的 advisory 锁:提交后就数不到了,而
					// "重复项只取一把"只有这个时刻可观测。
					var n int
					if err := tx.QueryRow(reqCtx,
						`SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND granted AND pid = pg_backend_pid()`).Scan(&n); err != nil {
						return err
					}
					insideLocks <- n
					return nil
				})
			}()

			if c.wantBlockedOn == "" {
				// 不该被阻塞的用例:必须很快成功,并断言事务内的锁数
				if err := <-doneCh; err != nil {
					t.Fatalf("请求 %v 应成功,实际 %v", c.give, err)
				}
				if got := <-insideLocks; got != c.wantInsideLocks {
					t.Fatalf("请求 %v:事务内应恰好持有 %d 把 advisory 锁,实际 %d",
						c.give, c.wantInsideLocks, got)
				}
				// 提交即释放:不能留下挂死的锁(留锁的后果是同 hash 的操作被无限阻塞)
				for _, h := range c.give {
					if h != "" && lockHeld(t, pool, h) {
						t.Fatalf("事务提交后 %q 应已释放,实际仍被锁住", h)
					}
				}
				return
			}

			// 被阻塞的请求:它必须**先锁住更小的 hash**,才可能阻塞在更大的那个上
			pid := <-pidCh
			waitUntilLocked(t, pool, c.wantLockedWhileBlocked, 3*time.Second)
			if got, want := grantedLocksOf(t, pool, pid), len(c.wantLockedWhileBlocked); got != want {
				t.Fatalf("被阻塞时后端 %d 应持有 %d 把 advisory 锁(%v),实际 %d —— 未按升序取锁",
					pid, want, c.wantLockedWhileBlocked, got)
			}
			for _, h := range c.wantLockedWhileBlocked {
				if !lockHeld(t, pool, h) {
					t.Fatalf("被阻塞时应已锁住 %q(规范序要求先取更小的 hash),实际未锁住", h)
				}
			}
			for _, h := range c.wantFreeWhileBlocked {
				if lockHeld(t, pool, h) {
					t.Fatalf("被阻塞时 %q 应仍空闲(它是比阻塞点更大的 hash),实际已被锁住", h)
				}
			}

			// holder 一直没释放 → 这次请求必须以错误结束,而不是被静默授予
			if err := <-doneCh; err == nil {
				t.Fatalf("请求 %v 在 %q 被他人持有期间不应成功(锁被静默授予?)", c.give, c.wantBlockedOn)
			}

			// 释放后同键必须能再拿到 —— 超时中止不能留下挂死的锁
			releaseHolder()
			retryCtx, retryCancel := context.WithTimeout(ctx, 5*time.Second)
			defer retryCancel()
			retryDone := make(chan error, 1)
			go func() {
				retryDone <- inTx(retryCtx, pool, func(tx pgx.Tx) error {
					return lockMany(t, retryCtx, tx, c.give)
				})
			}()
			select {
			case err := <-retryDone:
				if err != nil {
					t.Fatalf("持有者释放后同键请求 %v 应成功,实际 %v(锁泄漏?)", c.give, err)
				}
			case <-time.After(6 * time.Second):
				t.Fatalf("持有者释放后同键请求 %v 仍拿不到锁(锁泄漏?)", c.give)
			}
		})
	}
}
