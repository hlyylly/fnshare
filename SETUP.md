# 飞牛 fnOS 安装指南

> 两条路：
> - **`.fpk` 应用包**（推荐，"飞牛原生"体验）：见 [`fpk/INSTALL.md`](./fpk/INSTALL.md)
> - **Docker Compose 手动部署**（这份文档）

把 fnshare 跑在你的飞牛 NAS 上，3 步搞定。

## 0. 准备

- **路由器**：4001 端口（TCP + UDP）映射到飞牛 LAN IP；或者 IPv6 直连（推荐）
- **DDNS**：建议给飞牛配一个域名（飞牛自带的 DDNS 服务即可）；邀请链接里要用
- **存储**：在飞牛文件管理器里建个目录，比如 `/vol1/fnshare/data`

## 1. 装应用

### 方式 A：飞牛「Docker」应用（推荐）

1. 飞牛桌面 → 打开「Docker」
2. 「镜像」→ 输入 `fnshare:latest` 拉取（或先 SSH `docker build` 出本地镜像）
3. 「Compose」→ 新建 → 把 [`deploy/fnos-compose.yml`](./deploy/fnos-compose.yml) 内容粘进去
4. 调整 volume 路径（默认 `/vol1/fnshare/data`，按你的 NAS 实际改）
5. 启动

### 方式 B：SSH 命令行

```bash
ssh admin@your-nas
sudo mkdir -p /vol1/fnshare/data
sudo docker run -d \
  --name fnshare \
  --network host \
  --restart unless-stopped \
  -v /vol1/fnshare/data:/data \
  fnshare:latest
```

> 还没发布镜像时，先在本地 build：
> `git clone <repo> && cd fnshare && docker build -t fnshare:latest .`
> 然后 `docker save fnshare:latest | ssh admin@nas docker load`

## 2. 初始化 + 建群 / 入群

打开浏览器访问 `http://<飞牛-LAN-IP>:4101`，会看到 fnshare 控制台。第一次需要 SSH 进容器初始化：

```bash
docker exec -it fnshare sh
fnshare init --nickname your-name --contribute-gb 100
exit
docker restart fnshare
```

然后回到浏览器：

- **当群主**：在容器里 `fnshare group-create --name "我的朋友圈"`，然后浏览器「邀请」页输入你的 DDNS multiaddr 生成邀请链接
- **被邀请方**：在容器里 `fnshare group-join "<群主发来的链接>"`，然后 `docker restart fnshare`

> 这两个命令为什么不在 UI 里？BadgerDB 写群组状态需要短暂独占 daemon 的数据库锁，目前最干净的做法是 daemon 停一下做完再起来。M3.5 会把它们也搬到 UI。

## 3. 用起来

回到浏览器，应该能看到：
- **概览**：你和朋友们的节点列表、贡献容量
- **文件**：上传文件 → 自动 EC 分片到群组成员，列表点「下载」拿回来
- **账本**：和每个 peer 的流量收支，超贡献的人正数（绿色），超消耗的人负数（红色）
- **邀请**（仅群主）：生成邀请链接给新朋友

## DDNS multiaddr 怎么写？

格式：`/dns4/<your-ddns-domain>/tcp/4001/p2p/<your-peer-id>`

- `<your-ddns-domain>`：飞牛 DDNS 给你的域名，比如 `xxx.fnos.cn`
- `<your-peer-id>`：浏览器「概览」页能看到，形如 `12D3KooW...`

完整例子：
```
/dns4/abc123.fnos.cn/tcp/4001/p2p/12D3KooWB2uMX2MMbcj3pY7Mns688V6jgvpykv1trmEQEU8ExfVa
```

## 故障排查

| 现象 | 检查 |
|---|---|
| Web UI 打不开 | 确认 `docker logs fnshare` 没崩；4101 端口被占？|
| 节点连不上 | 4001 TCP/UDP 路由器端口转发到位？飞牛防火墙放行？IPv6 通不通？|
| put 报错 "need at least N members" | 群组成员数少于 EC 要求；默认 2+1 需要 3 个成员（含群主） |
| 文件 ls 看不到别人上传的 | 同步是按需的：你 `get` 的时候自动从其他节点拉 manifest |

## 数据放哪儿？

`<data>/` 目录里：
- `config.yaml` — 你的本机配置
- `node.key` — 节点身份（**不要泄露**，泄露相当于身份被盗）
- `node.key.enc` — 用于私有文件加密的 X25519 私钥（**也不要泄露**）
- `db/` — BadgerDB（群组、成员、manifest、账本、信誉）
- `blocks/` — 实际的分片数据（按内容寻址，重复上传不会占双份）
- `fuse-cache/` — FUSE 读取时的解密缓存

## 让飞牛影视看到 fnshare 文件夹

容器跑起来后，`/vol1/fnshare/library` 就是 fnshare 资源库的统一视图：
```
/vol1/fnshare/library/
├── 我的朋友圈/        ← 群名作为子目录
│   ├── photo.jpg     ← 共享文件
│   ├── movie.mp4
│   └── .private/     ← 我自己的私有文件（其他人看不到）
└── 老王的群/
    └── ...
```

**配置飞牛影视 / Plex / Jellyfin**：把 `/vol1/fnshare/library` 添加为媒体源即可。文件按需自动解密，元数据扫描和播放都能正常工作。

> **注意**：FUSE 挂载是 **只读** 的（M6）。要上传新内容请用 Web UI 或 `fnshare put`。
>
> **首次启动后**，`mountpoint /vol1/fnshare/library` 应该返回 `is a mountpoint`。如果不是，检查 compose 里的 bind 是不是用了 `propagation: rshared` —— 没有这一项的话 FUSE 挂载会被困在容器里，宿主机看不到。
