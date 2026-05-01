# fnshare

**朋友圈式分布式存储** —— 几个朋友各自贡献一部分硬盘空间，凑成一块更大的私有虚拟磁盘；端到端加密、纠删码冗余、自动 FUSE 挂载，本机任意应用（飞牛影视 / Plex / Jellyfin / 文件管理器 / …）像扫普通本地目录一样直接读。

> 不会被平台删档，不像云盘有版权扫描。资源在你和朋友的设备之间 P2P 同步，加密后再分片，谁存了什么、文件名是什么，存储节点都看不到。

适用任何能跑 Docker 的 Linux 机器（飞牛 fnOS、群晖 DSM 7+、QNAP、Unraid、Proxmox、自建 Debian/Ubuntu/Alpine、树莓派…）—— 一份 compose 文件搞定。

## 特性

| 模块 | 实现 |
|---|---|
| 🔒 端到端加密 | AES-256-GCM 内容 + NaCl box 包裹文件密钥 |
| 🧩 EC 冗余 | Reed-Solomon（默认 2+1，可配；任一节点离线仍能读） |
| 🌐 P2P | libp2p TCP+QUIC，IPv6 直连，IPv4 NAT 穿透 + DCUtR |
| 👥 多群组 | 一个节点可同时是 A 群主 + B/C 群成员，文件统一展示 |
| 🔐 资源分类 | Shared（群内可读）/ Private（仅 owner 可解密） |
| 📁 FUSE 挂载 | 本机其他应用直接读，无需任何插件 |
| ⚡ 流式上传 | 4 MiB stripe，RAM 占用与文件大小无关，不会 OOM |
| 🔁 离线兜底 | 任一朋友设备暂时下线不阻塞上传，恢复后自动补传 |
| ❤️ 心跳与信誉 | 探活 + 离线扣信誉 + lazy repair |

## 快速安装

任意能跑 Docker 27+ + Compose v2 的 Linux 机器。SSH 上去：

```bash
# 选一个有充足空间的目录（飞牛默认 /vol1/@appdata，群晖默认 /volume1/docker，
# 自建 Linux 可用 /opt 或 /srv）
INSTALL=/vol1/@appdata/fnshare
sudo mkdir -p $INSTALL
sudo tee $INSTALL/fnshare.yaml > /dev/null << 'EOF'
services:
  fnshare:
    image: mn4940128/fnshare:latest
    container_name: fnshare
    restart: unless-stopped
    network_mode: host
    cap_add: [SYS_ADMIN]
    devices: [/dev/fuse]
    security_opt: [apparmor:unconfined]
    volumes:
      - type: bind
        source: /vol1/@appdata/fnshare/data        # 改成你的安装目录/data
        target: /data
        bind: { create_host_path: true }
      - type: bind
        source: /vol1/@appdata/fnshare/library     # 改成你的安装目录/library
        target: /mnt/fnshare
        bind: { create_host_path: true, propagation: rshared }
    environment:
      FNSHARE_NICKNAME: "改成你自己的昵称"
      FNSHARE_QUOTA_GB: "100"
    entrypoint: ["/sbin/tini", "--", "/bin/sh", "-c"]
    command:
      - |
        [ -f /data/config.yaml ] || fnshare init --nickname "$$FNSHARE_NICKNAME" --contribute-gb "$$FNSHARE_QUOTA_GB"
        exec fnshare daemon
EOF

sudo vi $INSTALL/fnshare.yaml      # 改昵称、容量、source 路径
sudo docker compose -f $INSTALL/fnshare.yaml up -d
```

浏览器打开 `http://<本机 IP>:4101` 进控制台。

## 用起来

**概览** tab：建一个新群（你是群主），或粘贴邀请链接加入朋友的群。

**文件** tab：上传 → 自动加密 → EC 切片 → 分发到群组成员。Shared / Private 模式可选。

**账本** tab：你和每个 peer 的流量收支（贡献 vs 消耗），透明可见，防有人吸血。

**邀请** tab：群主生成 `fnshare://join#...` 邀请链接，发微信 / Telegram 给朋友（链接中的敏感内容在 URL fragment 里，不会被中间服务器看到）。

## 让本机其他应用看到资源库

飞牛影视 / Plex / Jellyfin / 文件管理器 → 添加媒体源 → 指向 `<安装目录>/library`。

群里任何人上传的视频、照片、文件自动出现，本机应用像扫普通磁盘一样使用。

## 端口

| 端口 | 协议 | 用途 | 公网 |
|---|---|---|---|
| 4001 | TCP + UDP | libp2p P2P | **必须**（路由器转发或 IPv6 直连） |
| 4101 | TCP | Web UI + 本地 API | LAN 即可 |

## 标签

- `latest` — 最新版
- `0.1.x` — 固定版本（推荐生产用，避免不兼容更新）

## 升级

```bash
cd <安装目录>
sudo docker compose -f fnshare.yaml pull
sudo docker compose -f fnshare.yaml up -d
```

数据保留在 `<安装目录>/{data,library}`，升级不丢。

## 卸载

```bash
sudo docker compose -f <安装目录>/fnshare.yaml down
# 数据目录默认保留；要彻底删 sudo rm -rf <安装目录>
```

## 系统要求

- Linux 内核 ≥ 5.x，**支持 FUSE**（飞牛 / 群晖 / Ubuntu 默认有；Alpine 装 `fuse3` 包；树莓派 OS 默认有）
- Docker ≥ 27 + Compose v2
- 推荐 IPv6 公网（中国家宽 IPv6 普及率高），或路由器把 4001 转发到本机
- 任意架构 —— 本镜像目前是 `linux/amd64`，ARM 用户提 issue 说一下我加 buildx

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

每个节点本质上就是一个 Docker 容器，运行 Go 写的 fnshare daemon。节点之间通过 libp2p 加密 P2P 通信，没有任何中心服务器。

## 项目状态

⚠️ **alpha** — 数据安全（加密 / EC / 流式 IO）已经端到端验证过，但功能仍在迭代。`latest` 可能有不兼容更新，长期部署用 `:0.1.0` 这种固定版本。

发现 bug 或想要新特性，欢迎 issue / PR。
