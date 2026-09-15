package storage

// MismatchedObjectKeys 找出"入库的 object_key 与当前布局算出来的不一致"的行(BE-S5-11)。
//
// 背景:`file_objects.object_key` 一直**只写不读**(读取/删除路径都是按 hash 重算),
// 于是布局一旦变化(或历史上某个版本写错了),老行会与真实落点不一致而**没人发现** ——
// 等到真要用它(接远端后端、做迁移、排障)时才发现记录是错的。
//
// 抽成纯函数的原因:它要能被单测与反向验证盯住(改一行布局规则,断言必须红),
// 而调用方(main.go 的自检)只负责把库里的行喂进来。
//
// 语义细节(刻意选"宁可漏报不可误报"):
//   - `object_key` 为空的行**不算不一致**:历史上允许不写(空 = 没有记录),报出来只会制造噪声;
//   - 比较是**逐字节**的(不做归一化):key 是拼进路径的东西,大小写/斜杠差异都是真差异。
func MismatchedObjectKeys(keyFor func(string) string, rows [][2]string) []string {
	var bad []string
	for _, r := range rows {
		hash, stored := r[0], r[1]
		if stored == "" {
			continue
		}
		if want := keyFor(hash); want != "" && want != stored {
			bad = append(bad, hash)
		}
	}
	return bad
}
