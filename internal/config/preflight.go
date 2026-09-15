package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// Preflight 是**启动前置检查**:数据/暂存目录可写、监听端口可用。
//
// 为什么与 Validate 分开:Validate 是纯函数(只读配置,无副作用,可随意在单测里跑),
// 而下面这些检查必须真的碰文件系统与网络。混在一起会让"校验配置"变成一件会
// 建目录、占端口的事,单测与 `-check-config` 这类用法立刻变得不可用。
//
// 两条纪律:
//
//  1. **一次报全部问题**,不是遇到第一个就返回:运维在 3 点重启服务时,
//     最不想经历的是"修一个→再起→再报下一个"。这与 Validate 的聚合行为一致。
//  2. 失败信息要写**具体路径与 errno 语义**(不可写/不是目录/端口被占),
//     而不是"配置错误":后者会让人去翻 yaml 找一个其实没写错的项。
//
// 与 6.8 纪律 3 的对应:实现层已经各自做了一次防御(NewFS 会探测可写、
// stage 目录在首次写入时创建),但那两处的失败发生在**启动流程中更晚的位置**
// (甚至晚到第一次上传),而这里的目的是把它们提前到"拒绝启动"这一步。
func Preflight(c *Config) error {
	if c == nil {
		return fmt.Errorf("配置为空")
	}
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	// 1) 对象根目录与暂存目录都要可写。
	//
	// 暂存目录必须单独查:它与 objects 在**同一 Root 下**,但生产建议分盘
	// (3.1 职责 4),分盘后"objects 可写而 tus-tmp 不可写"是完全可能的
	// ——那种配置下服务能起来,直到用户上传第一个分片才 500。
	root := strings.TrimSpace(c.Storage.Root)
	if root == "" {
		add("storage.root 为空")
	} else {
		if err := ensureWritableDir(root, "storage.root"); err != nil {
			add("%s", err.Error())
		}
		if err := ensureWritableDir(filepath.Join(root, "objects"), "storage.root/objects"); err != nil {
			add("%s", err.Error())
		}
		if err := ensureWritableDir(filepath.Join(root, "tus-tmp"), "storage.root/tus-tmp"); err != nil {
			add("%s", err.Error())
		}
	}

	// 3) 监听端口可绑。
	if err := checkAddrAvailable(c.Server.HTTPAddr); err != nil {
		add("%s", err.Error())
	}

	if len(problems) > 0 {
		return fmt.Errorf("启动前置检查未通过(%d 项):\n - %s",
			len(problems), strings.Join(problems, "\n - "))
	}
	return nil
}

// ensureWritableDir 确保路径是一个可写目录(不存在则创建)。
//
// 刻意**走真实的写探针**而不是看权限位:Windows 上只看 `os.Stat` 的 mode
// 几乎全是可写(ACL 才决定实际权限,而 mode 位在 Windows 上大部分是合成的),
// 于是"权限检查通过、写入失败"——探针文件是这个平台上唯一可信的判据。
func ensureWritableDir(dir, label string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("%s 不可用(%s): %v", label, dir, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("%s 不可访问(%s): %v", label, dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s 不是目录(%s)", label, dir)
	}
	probe := filepath.Join(dir, ".netdisk-preflight")
	if err := os.WriteFile(probe, []byte("ok"), 0o640); err != nil {
		return fmt.Errorf("%s 不可写(%s): %v", label, dir, err)
	}
	if err := os.Remove(probe); err != nil {
		// 写得进删不掉:磁盘满了或目录权限异常,同样必须拦下(残留探针文件还行,
		// 但它说明这个目录的状态不对,越早暴露越好)
		return fmt.Errorf("%s 写入后无法清理探针文件(%s): %v", label, dir, err)
	}
	return nil
}

// checkAddrAvailable 检查监听地址可绑。
//
// 做法是**真的绑一次再放开**,而不是解析配置:
// "端口被占"只有内核知道,任何配置层面的推断都会漏掉
// (另一个进程、TIME_WAIT、权限不足、地址写错)。
//
// 放开与正式绑定之间有窗口,理论上可能被别人抢走 —— 这里不追求原子占位:
// 本检查的目的不是"占住",而是**把失败提前到启动最开始并给出可读原因**;
// 真被抢走时 `http.Serve` 会以同样的原因失败,行为并没有变差。
func checkAddrAvailable(addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return fmt.Errorf("http_addr 为空")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// 提示两种读法:ExecStartPre 场景下"端口被占"= 另一个实例还在跑(该拦);
		// 运维在服务运行期间手工跑 `-check` 时,同一个提示是**预期的**而非故障。
		// 少了这半句话,运维会去翻 yaml 找一个其实没写错的项。
		return fmt.Errorf("监听地址不可用(%s): %v"+
			"(端口已被占用:若本机服务正在运行,这是预期的;否则说明另一实例仍占着该端口)", addr, err)
	}
	return ln.Close()
}
