# Roadmap

## M1 — P2P 连通 + 邀请加入 ✅

**已交付**：
- libp2p host（TCP + QUIC，IPv4/IPv6，AutoNAT、DCUtR、Relay）
- Ed25519 节点身份 + Ed25519 群组密钥
- 邀请链接：CBOR + base64url，群主签名 + 过期时间 + nonce
- `/fnshare/join/1.0.0` 握手协议
- BadgerDB 持久化（群组描述符 + 成员表）
- Cobra CLI（init / group-create / invite-create / group-join / daemon / status）
- Dockerfile + 3 节点 docker-compose 冒烟测试

**已知限制**：
- 非群主节点处理 join 时，先本地写入再异步转发给群主（M2 用 gossip 修正）
- 邀请链接的 nonce 还没存账本去重（一次性属性靠 expiry 兜底）

## M1.5 — 本地 HTTP API ✅

- daemon 在 `127.0.0.1:4101` 暴露 `/v1/{status,invite,files,...}`
- CLI（status / invite-create）优先走 API，daemon 不在时回退到直接读 BadgerDB
- 解决了 BadgerDB 单进程独占锁导致的 "daemon 跑着就不能 status" 问题

## M2 — 分片存储 ✅

**已交付**：
- Reed-Solomon EC（默认 2+1，3 节点测试床上即用；可配）
- 文件 manifest（CBOR，BadgerDB 持久化），hash 可寻址（FileID = SHA-256(plaintext)）
- 本地 blockstore：分片落地到 `data/blocks/<aa>/<full-hex>`，原子写
- `/fnshare/blocks/1.0.0` 协议：`PUT_SHARD / GET_SHARD / PUT_MANIFEST / GET_MANIFEST`，长度前缀 + CBOR 帧
- `/fnshare/peers/1.0.0` 协议：节点互相交换地址簿，让通过同一 admin 入群的成员能找到彼此
- 节点选择策略：按 peer-id 排序取前 k+m（确定性，方便所有节点对一致）
- HTTP API：`POST/GET /v1/files`，`GET /v1/files/{id}/content`
- CLI：`fnshare put / get / ls`
- 验证：1MB 文件 alice→put，bob 和 carol 都能 get；杀掉 alice 后 bob 仍能从 carol 重建（EC 容错）

**已知限制**（推到 M-x）：
- 单 stripe = 整个文件一次进 RAM（适合几百 MB 内）；大文件需要 multi-stripe
- 写路径同步等所有 holder ack；任一 holder 离线就失败（M5 加缓冲 + lazy repair 修）
- 节点 join 后，老节点要等下一次 5s ticker 才知道新成员的地址；M3 用 push gossip 替换

## M3 — Web UI + 账本 + 飞牛打包 ✅

**已交付**：
- 嵌入式 Web UI（vanilla HTML/JS/CSS，`go:embed`，零外部依赖）
  - 概览（节点信息 + 群组成员表）
  - 文件（上传 / 列表 / 下载）
  - 账本（与每个 peer 的 4 向流量收支 + 净额）
  - 邀请（仅群主，DDNS multiaddr → 邀请链接）
- 本地账本（ledger）：4 维流量计数（StoredForThem / ServedToThem / StoredOnThem / DownloadedFrom）
  - block 协议处理时自动累加
  - 10s ticker 写盘，shutdown 时 flush
- API 默认绑 `0.0.0.0:4101`（LAN 浏览器可访问）
- 飞牛部署包：`deploy/fnos-compose.yml`（host 网络模式，单节点）
- `SETUP.md` 详细安装文档（飞牛 Docker 应用 / SSH 两种路径）

**已知限制**（推到 M3.5）：
- 账本只是本地视角；跨节点 gossip + 签名（防篡改）放到 M3.5
- group-create / group-join 还需要进容器执行 CLI；要全 UI 化得加 IPC 让 daemon 持有 DB 锁的同时也允许"先停一下做完再起"

## M3.5 — UI 完整化 + 账本 gossip ⏭

- 把 group-create / group-join 搬到 UI（daemon 把 DB 操作内联，不再需要"停 daemon 才能用 CLI"）
- 账本 `/fnshare/ledger/1.0.0` gossip：每个节点签名后周期广播自己的视角，其他节点聚合
- 全局排行榜（不再只是"我"的视角）
- WebSocket 推送，UI 不再轮询

- 内嵌 Web UI（SvelteKit，编译成静态资源塞进二进制）
- 透明账本：每个节点本地记账 + gossip + 签名校验
- 排行榜 / 我的收支视图
- 打成 `.fnpkg`，提交飞牛应用中心

## M4 — 资源分类 + 端到端加密 ✅

**已交付**：
- AES-256-GCM 加密文件内容；NaCl anonymous box 加密 owner-only 的文件密钥
- 节点身份新增独立 X25519 加密密钥对（与 libp2p 的 Ed25519 wire 身份分离）
- 群组生成时分配 32B 共享密钥，通过邀请链接 + join handshake 分发
- 两种模式：
  - **shared**：文件密钥用群组 SharedKey 包裹，群内任何人可读；holder 看不到明文
  - **private**：文件密钥用上传者的 EncPub anonymously 包裹，且文件名也加密；只有上传者本人能解密
- file_id = SHA-256(ciphertext)，避免 convergent encryption 指纹
- CLI: `fnshare put --private`；UI: 上传表单的「私有」勾选 + 文件列表 🔒 标签
- 端到端测试：3 节点上 grep 全部 blocks 目录，零明文泄露

## M4.5 — 多群组架构 ✅

**已交付**：
- 一个节点可同时加入多个群组（既可以是 A 群群主，又可以是 B/C 群成员）
- 数据模型：`group/<gid>` / `member/<gid>/<pid>` / `bootstrap/<gid>` 全部按群隔离
- Manifest 增加 `GroupID`，决定使用哪个群的 SharedKey 解密
- 文件视图统一：UI 资源库展示所有群组的文件，每行带「群组」列，下载/上传按群路由
- CLI 新增 `fnshare groups`；`put --group` / `invite-create --group`
- 端到端测试：3 节点都加入两个群组，跨群文件相互可读，统一资源库展示正确

## M5 — 心跳 + 信誉 + lazy repair ✅

**已交付**：
- `/fnshare/ping/1.0.0` 极简探活协议
- 心跳 goroutine（默认 30s 探一次，3 次失败 = offline；测试用 5s 间隔加快验证）
- 信誉机制：每个 peer 0..100 分；失败 -5 / 次（越久越低），成功 +1 / 次（缓慢回升）
- 状态转换：peer 从 online→offline 自动触发 `repair.ScanForOfflinePeer`
- Lazy repair：扫描所有 manifest，找出离线 peer 持有分片的文件；如果存活 holder ≤ k，从同群在线 spare 节点迁移分片，更新 manifest 并广播
- UI：成员列表的在线状态点（绿/红） + 信誉进度条 + 离线时长

**已知限制**（M6 处理）：
- 心跳没签名 → 一个恶意节点可以谎称别人离线（仅本地决策，不传播）
- 信誉只是本地视角，没 gossip
- Repair 选 spare 是按 peer-id 排序取首个；M6 改成「按贡献容量加权」

## M6 — FUSE 挂载（资源库当成本地文件夹）✅

**已交付**：
- 嵌入式 FUSE 文件系统（`github.com/hanwen/go-fuse/v2`）
- 路径布局：`<mount>/<group-name>/<filename>`；私有文件在 `<group>/.private/`，仅 owner 可见
- 按需解密 + tempfile LRU 缓存（默认缓存 16 个文件），支持媒体应用的随机访问 / scrubbing
- daemon 通过 `FNSHARE_MOUNT` 环境变量 / config 启用，挂载路径在容器内
- Dockerfile 装 fuse3，compose 加 `cap_add: SYS_ADMIN` + `devices: /dev/fuse`
- fnos-compose.yml 用 `bind` + `propagation: rshared` 让宿主机的飞牛影视等应用直接看到 fnshare 资源库

**已知限制**（M6.5）：
- 私有文件在 FUSE 视图里显示为 `<file_id>.bin`（filename 是加密的，需要解密成本）；M6.5 在 owner 节点本地保存一份明文 filename 索引解决
- 只读；写入还得走 UI / CLI（写时正确处理"半完成上传"复杂度高）
- 缓存只按数量上限，没字节数上限；大文件可能撑爆 disk

## M7 — 流式 put/get + rendezvous 选址 + 字节级 stripe 缓存 ✅

**已交付**：
- **rendezvous hashing**（HRW，`internal/holders`）：每个文件按 hash(peer ‖ file_id) 选 k+m 个 holder，所有 N 个成员均匀分担存储；新加 / 退群只迁移 ~(k+m)/N 的文件
- **多 stripe manifest**：文件按 4 MiB plaintext 切 stripe，每个 stripe 独立 AES-256-GCM 加密 + EC 编码；Holders 在文件级（k+m 个 peer 共享所有 stripe 的 shard 槽位）
- **流式 Put**：`io.ReadFull` 一次读 4 MiB → 加密 → EC → 直接 ship 到 holder；daemon RAM 与文件大小**无关**（200 MiB 上传时 RSS 仅 25→168 MiB，旧代码会到 ~550 MiB 至 OOM）
- **`file.Reader`**：实现 `io.ReaderAt`，按需 fetch + decrypt 单个 stripe；FUSE 现在支持随机读（媒体 scrubbing 不会触发全文件下载）
- **`file.StripeCache`**：进程级共享缓存，按字节上限 LRU（默认 1 GiB），跨所有打开的 Reader 共用
- **API `/v1/files/{id}/content`** 改成真流式（`io.Copy(w, reader)`），不再 buffer 整个文件
- **repair 适配多 stripe**：每个 stripe 独立 EC reconstruct + push spare（保留 ciphertext，无需文件密钥）

**端到端测试通过**（200 MiB 文件）：流式上传、3 节点全均匀分担、bob 全文件下载 hash 完全一致、FUSE `head -c 1024` 和 offset 80 MiB 的 4 KB 随机读都只 fetch 必要的 stripe。

## M8 — 写缓冲 + 私有文件名 ✅

**已交付**：
- **Spool（写缓冲）**: `internal/spool` 包，按 peer 分目录的 filesystem queue；上传时 holder 离线 → shard/manifest 落到本地 `<data>/spool/<peer>/{s,m}_<key>` 而非失败；后台 Worker 每 10s 扫描，对 ledger.IsOnline 的 peer 批量 flush（先 shard 后 manifest），成功后删除条目
- 上传只要 **k of (k+m)** 个 holder 在线就立即成功（剩余的异步补齐），文件立即可读
- **私有文件名索引**：owner 节点本地 BadgerDB key `pname/<file_id>` 存明文文件名（仅 owner 持有）；`Service.List` 自动 enrich，CLI / FUSE / UI 看到真名而不是 `<id>.bin`；非 owner 仍只看到加密 ciphertext，隐私不受损

**端到端测试通过**：
- 杀掉 bob 的 daemon → heartbeat 标 offline → alice 上传成功（spool 里 1 shard + 1 manifest）→ carol 立即可读 → 重启 bob → 25s 内 worker 自动补传，spool 清空
- alice 私有上传 `tax-2026.pdf`：alice 的 ls 和 FUSE 都看到真名；bob 看到加密 ciphertext

## M9 — 配额 + 加权选址 + WebSocket ⏭

- **配额强制**：每个 holder 拒收超过 peer 自报 contribution 的 shard
- **加权 spare 选择**：按贡献容量 + 信誉权重 rendezvous（让贡献多的、稳定的节点承担更多）
- WebSocket 推送替换 5s UI 轮询
- FUSE 写支持（mount 里直接 `cp` 也能上传）
