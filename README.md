# fnshare

朋友圈式的分布式存储 —— 给飞牛 fnOS 用户。

几个朋友各自贡献一部分 NAS 容量，凑成一块更大的"虚拟磁盘"。
资源加密分片，多副本/纠删码冗余存储；私密、可控、不会被平台删档。

> 当前进度：**M8 完成** — 写缓冲（任一朋友 NAS 离线不阻塞上传）+ 私有文件名 owner 端索引。**到这里整个系统已经能在朋友圈里"真用"了**。M9 见 ROADMAP。
>
> 飞牛安装：
> - **`.fpk` 应用包**（推荐，单文件 16 MB，docker 镜像内置）：[`fpk/INSTALL.md`](./fpk/INSTALL.md)
> - **Docker Compose 手动部署**（开发 / 调试）：[`SETUP.md`](./SETUP.md)

## 它解决什么

- 网盘会删资源 → 自己人之间的私有网络，谁都管不着
- 单台 NAS 容易丢数据 → 多副本分布在朋友的 NAS 上，硬件故障不丢数据
- 公网穿透麻烦 → libp2p 自带 IPv6 直连 + UDP 打洞 + relay 兜底
- 朋友吸血 → 透明账本，每个人贡献/消耗都看得见

## 架构概览

- **拓扑**：全对等 mesh（几十人量级，不需要 tracker / DHT 引入新中心）
- **身份**：每个节点一对 Ed25519 密钥；群组本身也有一对 Ed25519 密钥（群主控制）
- **入群**：群主签发邀请链接 → 新节点拿邀请连任意现有成员 → 现成员验证签名后写入本地成员表
- **网络层**：go-libp2p（TCP + QUIC，IPv4/IPv6，AutoNAT、DCUtR 打洞、Circuit Relay v2）
- **存储**：BadgerDB 存元数据（M2 起加分片存储）

## 快速开始

### 在飞牛上跑（推荐）

> 飞牛应用中心打包发布前，先用 Docker 部署。

```bash
# 飞牛 SSH 进去，或者用「Docker Compose」应用粘贴下面内容
mkdir -p /vol1/fnshare/data
docker run -d --name fnshare \
  --restart unless-stopped \
  -v /vol1/fnshare/data:/data \
  -p 4001:4001/tcp -p 4001:4001/udp \
  fnshare:dev   # 暂时本地构建，后续推 Docker Hub

# 初始化身份（昵称 + 贡献 100GB）
docker exec fnshare fnshare init --nickname alice --contribute-gb 100

# 当群主：建群
docker exec fnshare fnshare group-create --name "我的朋友圈"

# 生成邀请链接（必须填一个外部可达的多地址，飞牛 DDNS 推荐）
PEER=$(docker exec fnshare fnshare status | awk '/^node/ {print $NF}' | tr -d '()')
docker exec fnshare fnshare invite-create \
  --bootstrap "/dns4/your-nas.example.com/tcp/4001/p2p/${PEER}" \
  --ttl-hours 72
# → 输出 fnshare://join#... 微信发给朋友

# 当被邀请方：加入
docker exec fnshare fnshare init --nickname bob --contribute-gb 100
docker exec fnshare fnshare group-join "fnshare://join#..."

# 启动后台守护
docker exec -d fnshare fnshare daemon
```

### 本地 3 节点冒烟测试

```bash
./scripts/test-3-nodes.sh
```

会拉起 3 个容器，跑完整的「建群 → 邀请 → 加入」流程，最后打印每个节点的成员表。

## CLI 命令

| 命令 | 用途 |
|---|---|
| `fnshare init` | 生成节点身份和默认配置 |
| `fnshare group-create --name <NAME>` | 建群（本节点变成群主） |
| `fnshare invite-create --bootstrap <multiaddr>` | 群主签发邀请链接 |
| `fnshare group-join <invite-link>` | 用邀请链接入群 |
| `fnshare daemon` | 跑后台节点（libp2p + blockstore + HTTP API） |
| `fnshare status` | 查看本地状态和成员列表 |
| `fnshare put <file>` | 上传文件，EC 编码后分发到群组成员 |
| `fnshare get <file-id> <out-path>` | 用 file id 下载，自动从可用 holder 凑齐分片重建 |
| `fnshare ls` | 列出本节点已知的文件 |

`status / invite-create` 在 daemon 跑着时走 HTTP API；daemon 不在时回退到直接读 DB。
`put / get / ls` 必须 daemon 在跑（需要 libp2p host 做实际传输）。

数据目录默认 `~/.fnshare`，Docker 镜像里是 `/data`。

## 邀请链接格式

```
fnshare://join#<base64url(cbor)>
```

负载内容（CBOR 编码后签名）：

```
gid    群组公钥指纹（hex）
name   群组显示名
apub   群组 Ed25519 公钥（32B，离线验证用）
boot   bootstrap 节点的 multiaddr 列表
nonce  16 字节随机数（防重放，未来 server 端记账）
iat    签发时间
exp    过期时间
quota  受邀人配额上限（0 = 不限）
sig    群主私钥对以上字段的 Ed25519 签名
```

链接里所有内容放在 URL 的 fragment（`#` 后面），浏览器和反代都不会上传——可以放心通过微信、Telegram 转发。

## 网络要求

- **必备**：节点之间至少有一个可达路径
  - IPv6 直连（推荐，飞牛多数家宽支持）
  - 或 IPv4 + 端口转发（4001 TCP & UDP）
  - 或 NAT-NAT 之间靠 libp2p hole punching（DCUtR）+ relay 节点兜底
- **DDNS**：邀请链接里必须有至少一个外部可达的 multiaddr，飞牛自带的 DDNS 域名最方便

## 路线图

- **M1 ✅** — P2P 连通 + 邀请入群（当前）
- **M2** — 文件分片（Reed-Solomon 6+3 EC）+ 节点间存取协议
- **M3** — Web UI + 透明账本 + 飞牛 .fnpkg 应用包
- **M4** — 资源分类（自有 / 共享）+ 端到端加密的 capability 模型
- **M5** — 离线节点的修复策略（软离线阈值、惰性 repair）

## 项目结构

```
cmd/fnshare/         CLI 入口（cobra）
internal/
  config/            ~/.fnshare/config.yaml
  keys/              Ed25519 节点身份
  group/             群组状态、成员表（持久化到 BadgerDB）
  invite/            邀请链接编/解码 + 签名
  node/              libp2p host + /fnshare/join/1.0.0 协议
  store/             BadgerDB 封装
deploy/              docker-compose（3 节点测试）
scripts/             冒烟测试脚本
```

## 协议版本

| 协议 ID | 用途 | 状态 |
|---|---|---|
| `/fnshare/join/1.0.0` | 入群握手 | ✅ M1 |
| `/fnshare/members/1.0.0` | 成员列表全量同步 | ✅ M1 |
| `/fnshare/blocks/1.0.0` | 分片 + manifest 存取（PUT/GET） | ✅ M2 |
| `/fnshare/peers/1.0.0` | 节点地址簿交换（让间接联系的成员能直连） | ✅ M2 |
| `/fnshare/ledger/1.0.0` | 账本 gossip | ⏭ M3 |
