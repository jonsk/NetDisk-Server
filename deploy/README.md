# deploy/ —— 部署物与运维脚本

对应清单 **第 5 卷(DP-01~DP-04)**;本仓概览见 **`DOC/功能清单与开发任务清单.md`**,
操作步骤见 **`deploy/`** 下各子目录(provision 安装与部署 / backup 备份与恢复 / verify 故障与演练)。

```
deploy/
├── build-release.sh            前端 build → 拷 dist → 交叉编译单二进制(DP-02 ①)
├── install.sh                  目标机上装机:用户/目录/secrets/二进制/配置/systemd/自检/启动
├── config/config.example.yaml  配置示例(默认值在 Go 里;secret 只走 env)
├── systemd/
│   ├── netdisk.service         通用生产基线(ProtectSystem=strict + ReadWritePaths)
│   ├── 10-small-box.conf       drop-in:小内存机覆盖 MemoryMax=320M(机器差异留在机器上)
│   └── secrets.env.example     有哪些键(不是键的值)
├── provision/                  依赖安装与系统准备(PG 18 / Redis / WAL 归档路径)
├── nginx/                      DP-01:站点 + 代理头 + http 调优 + apply.sh + verify.sh
├── backup/                     DP-04:备份/演练脚本 + systemd 单元与定时器
├── prometheus/                 DP-04:抓取配置 + 15 条告警规则
└── verify/                     可复现的验证脚本(自检反向验证、备份+演练)
```

## 一次性部署(新机器)

```sh
# 1) 依赖
scp deploy/provision/*.sh root@<host>:/root/ && ssh root@<host> 'sh /root/01-install-packages.sh'
# 2) 系统准备(用户/目录/secrets/PG/Redis)
ssh root@<host> 'sh /root/02-provision-base.sh && sh /root/03-provision-postgresql.sh && sh /root/04-provision-redis.sh'
# 3) 产物 + 装机
deploy/build-release.sh 1.0.0
scp deploy/dist/netdisk-1.0.0-linux-amd64 root@<host>:/tmp/
scp deploy/config/config.example.yaml deploy/systemd/* root@<host>:/root/
ssh root@<host> 'sh install.sh /tmp/netdisk-1.0.0-linux-amd64 http://<host>'
# 4) 反代
scp -r deploy/nginx root@<host>:/root/ && ssh root@<host> 'sh /root/nginx/apply.sh /root/nginx && sh /root/nginx/verify.sh'
# 5) 备份与告警
scp -r deploy/backup deploy/prometheus root@<host>:/root/ && ssh root@<host> 'sh /root/setup-dp04.sh'
```

## 验收(全部实测通过)

```sh
sh deploy/nginx/verify.sh                 # DP-01:七项职责 + 验证器自检(15/15)
sh deploy/verify/01-negative-selfcheck.sh # DP-02②:坏环境必须被自检拦住(三个场景)
sh deploy/verify/02-backup-and-drill.sh   # DP-04:备份 + 恢复演练(含对象 sha256 比对)
/opt/netdisk/bin/netdisk-pitr-drill.sh    # DP-04:PITR 时间旅行(断言「目标时刻之后的数据不在」)
promtool check rules /etc/prometheus/netdisk-alerts.yml   # DP-04:15 条规则语法与加载

# SSE 真的在流式(Nginx 不缓冲)
TOK=$(curl -s -X POST http://127.0.0.1:8080/api/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"login":"admin","password":"***","audience":"web"}' | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
timeout 8 curl -sN -H "Authorization: Bearer $TOK" http://<host>/api/v1/events | head -3

# 真实客户端 IP 透传(Go 日志里应是客户端 IP,而不是 127.0.0.1)
journalctl -u netdisk -n 50 --no-pager | grep 'path=/admin/' | tail -2
```

## 部署时踩到并已写进注释的坑(别再踩)

| # | 坑 | 后果 / 真相 |
|---|---|---|
| 1 | 把 `netdisk -migrate` 当前台命令等待 | **永不返回**(它的语义是"迁移后继续服务")→ 部署脚本挂死;迁移其实已成功 |
| 2 | 先 `go build` 再拷 `web dist` | embed 进旧前端:能编译、页面是上一版 |
| 3 | `objects/`、`tus-tmp/` 归 root | 服务以 netdisk 启动时自检报"不可写"并拒绝启动 |
| 4 | `ReadWritePaths` 漏了 `/var/lib/netdisk` | `ProtectSystem=strict` 下写备份状态失败 → 自检一次报 3 项 |
| 5 | WAL 归档目录放在 `/var/lib/netdisk` 下 | postgres 穿不过 0750 的父目录 → 归档连续失败(实测 33 次) |
| 6 | `failed_count != 0` 当判据 | 累计值永不复位 → 修好归档后备份仍永远失败(改为看**增量**) |
| 7 | `server_tokens` 在 `conf.d` 里重复写 | Debian 13 stock 已有 → `nginx -t` 直接失败 |
| 8 | 部署脚本里 `su - postgres -c "psql …" <db>` | 库名成了 su 的参数 → psql 连错库、表不存在 |
| 9 | 对象路径再拼一次 `objects/` | `object_key` 自带该前缀 → 误报"恢复出来是空的" |
| 10 | 崩溃循环后直接 `systemctl start` | `StartLimitBurst` 拒启 → 先 `systemctl reset-failed netdisk` |
| 11 | 探针经反代跑 SSE 断言 | Nginx **会消费** `X-Accel-Buffering` → 该断言必须直连应用验证 |
| 12 | `Documentation=file:///…中文名.md` | systemd 认定非法 URL 并忽略(改指 ASCII 目录) |

## 与文档基线的偏离(如实记录)

1. **Redis 8.0.2**(Debian 13 仓库版本)而非 7.x:E-06 三条要求全满足并实测,协议兼容。
2. **`MemoryMax=320M`**(drop-in 覆盖单元里的 2G):目标机内存小,单元本体保留生产基线。
