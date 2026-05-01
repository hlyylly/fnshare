# fnshare

**朋友圈式分布式存储** —— 几个朋友各自贡献一部分硬盘空间，凑成一块更大的私有虚拟磁盘。端到端加密、纠删码冗余、自动 FUSE 挂载，本机任意应用（飞牛影视 / Plex / Jellyfin / 文件管理器…）像扫普通本地目录一样直接读。

> 不像云盘有版权扫描，不会被平台删档。资源在你和朋友的设备之间 P2P 同步，加密后再分片，谁存了什么、文件名是什么，存储节点都看不到。

适用任何能跑 Docker 的 Linux 机器：飞牛 fnOS、群晖 DSM 7+、QNAP、Unraid、Proxmox、自建 Debian/Ubuntu/Alpine、树莓派…

[![Docker Hub](https://img.shields.io/badge/Docker_Hub-mn4940128%2Ffnshare-blue)](https://hub.docker.com/r/mn4940128/fnshare)
[![License](https://img.shields.io/badge/License-MIT-green)](./LICENSE)

## 特性

| 模块 | 实现 |
|---|---|
| 🔒 端到端加密 | AES-256-GCM 内容 + NaCl box 包裹文件密钥 |
| 🧩 EC 冗余 | Reed-Solomon（默认 2+1，可配；任一节点离线仍能读） |
| 🌐 P2P | libp2p TCP+QUIC，IPv6 直连，IPv4 NAT 穿透 + DCUtR |
| 👥 多群组 | 一个节点可同时是 A 群主 + B/C 群成员，文件统一展示 |
| 🔐 资源分类 | Shared（群内可读）/ Private（仅 owner 可解密） |
| 📁 FUSE 挂载 | 本机其他应用直接读，无需任何插件 |
| ⚡ 流式上传 | 4 MiB stripe，RAM 占用与文件大小无关 |
| 🔁 离线兜底 | 任一朋友设备暂时下线不阻塞上传，恢复后自动补传 |
| ❤️ 心跳 + 信誉 | 探活 + 离线扣信誉 + lazy repair |

## 快速安装

任意能跑 Docker 27+ + Compose v2 的 Linux。SSH 上去：

```bash
INSTALL=/vol1/@appdata/fnshare        # 飞牛默认；群晖用 /volume1/docker/fnshare；自建 Linux 用 /opt/fnshare
sudo mkdir -p $INSTALL
sudo curl -L -o $INSTALL/fnshare.yaml \
  https://github.com/hlyylly/fnshare/raw/main/deploy/fnshare-friend.yaml
sudo vi $INSTALL/fnshare.yaml         # 改昵称、容量、source 路径
sudo docker compose -f $INSTALL/fnshare.yaml up -d
```

浏览器打开 `http://<本机 IP>:4101` 进控制台。镜像自动从 [Docker Hub](https://hub.docker.com/r/mn4940128/fnshare) 拉，无需手工传文件。

完整安装指南：[`deploy/INSTALL-FRIEND.md`](./deploy/INSTALL-FRIEND.md)

## Web UI

| Tab | 干什么 |
|---|---|
| **概览** | 节点信息、群组成员表、建群 / 用邀请链接加群 |
| **文件** | 上传（Shared / Private 模式可选）/ 下载 / 列表 |
| **账本** | 你和每个 peer 的流量收支（贡献 vs 消耗） |
| **邀请** | 群主生成 `fnshare://join#...` 链接发给朋友 |

## 让本机其他应用看到资源库

```
飞牛影视 / Plex / Jellyfin / 文件管理器 → 添加媒体源 → <安装目录>/library
```

群里任何人上传的视频、照片、文件自动出现，本机应用像扫普通磁盘一样使用。

## 端口

| 端口 | 协议 | 用途 | 公网 |
|---|---|---|---|
| 4001 | TCP + UDP | libp2p P2P | **必须**（路由器转发或 IPv6 直连） |
| 4101 | TCP | Web UI + 本地 API | LAN 即可 |

## 架构

```
   节点 A  ←→  节点 B  ←→  节点 C  ←→  ……  ←→  节点 N
                  ⇅
       libp2p P2P (TCP+QUIC, IPv6 优先)
                  ⇅
   每个文件 → AES-256-GCM 加密
            → Reed-Solomon EC 切片 (k+m)
            → rendezvous hashing 选址，均匀分担到所有节点
                  ⇅
            ┌─── 统一资源库 ───┐
            │   FUSE 挂载到    │
            │   本机文件系统    │
            └─────────┬────────┘
                      │
       本机任何应用都能像读普通目录一样读
                      ⇅
   飞牛影视 │ Plex │ Jellyfin │ 文件管理器 │ rsync │ ……
```

每个节点 = 一个 Docker 容器跑 Go 写的 fnshare daemon。节点之间 libp2p 加密 P2P 通信，没有任何中心服务器。

## 邀请链接格式

```
fnshare://join#<base64url(cbor)>
```

CBOR 负载（群主 Ed25519 签名）：群组 ID / 群组共享密钥 / bootstrap 多地址 / nonce / 过期时间 / 配额上限。

URL fragment（`#` 后面）不会被中间服务器看到 —— 可以放心通过微信、Telegram 转发。

## 项目状态

| 里程碑 | 内容 | 状态 |
|---|---|---|
| M1 | P2P + 邀请加入 | ✅ |
| M2 | EC 分片 + put/get | ✅ |
| M3 | Web UI + 本地账本 | ✅ |
| M4 | 端到端加密 + 多群组 | ✅ |
| M5 | 心跳 + 信誉 + lazy repair | ✅ |
| M6 | FUSE 挂载 | ✅ |
| M7 | 流式 IO + rendezvous 选址 | ✅ |
| M8 | 写缓冲 + 私有文件名索引 | ✅ |
| M9 | 配额强制、加权选址、WebSocket | ⏭ |

详细路线图：[`ROADMAP.md`](./ROADMAP.md)

⚠️ **alpha** —— 数据安全（加密 / EC / 流式 IO）已端到端验证过，但功能仍在迭代。`latest` 可能有不兼容更新，长期部署用 `:0.1.0` 这种固定版本。

## 协议版本

| 协议 ID | 用途 |
|---|---|
| `/fnshare/join/1.0.0` | 入群握手 |
| `/fnshare/members/1.0.0` | 成员列表同步 |
| `/fnshare/blocks/1.0.0` | 分片 + manifest 存取 |
| `/fnshare/peers/1.0.0` | 节点地址簿交换 |
| `/fnshare/ping/1.0.0` | 心跳探活 |

## 项目结构

```
cmd/fnshare/         Cobra CLI 入口
internal/
  api/               HTTP API + 嵌入式 Web UI（go:embed）
  blockstore/        分片存储（filesystem）
  config/            ~/.fnshare/config.yaml
  crypto/            AES-GCM + NaCl box
  ec/                Reed-Solomon
  file/              put/get + 流式 + StripeCache
  fuse/              FUSE 文件系统（挂载到本机）
  group/             群组、成员、bootstrap
  heartbeat/         心跳探活
  holders/           rendezvous hashing 选址
  invite/            邀请链接编/解码
  keys/              Ed25519 + X25519 双身份
  ledger/            本地流量账本 + 信誉
  manifest/          多 stripe 文件清单
  node/              libp2p host + 各协议 handler
  repair/            离线触发 lazy repair
  spool/             写缓冲（holder 离线时本地暂存）
  store/             BadgerDB 封装
deploy/              docker-compose + 朋友安装文档
fpk/                 飞牛 .fpk 应用包源（备选安装方式）
scripts/             各里程碑的端到端冒烟测试
```

## 自己 build / 改

```bash
git clone https://github.com/hlyylly/fnshare && cd fnshare
docker build -t fnshare:latest .              # 编译 + 打镜像
./scripts/test-m7-streaming.sh                # 跑 200 MiB 文件流式上传测试
```

测试脚本会拉起本地 3 节点 Docker compose，演示完整的「建群 → EC 分片 → FUSE 读」流程。

## License

[MIT](./LICENSE)
