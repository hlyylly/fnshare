# 飞牛 .fpk 安装指南

`fnshare_0.1.0_x86.fpk`（16 MB）是**完全自包含**的飞牛应用包，docker 镜像、配置、生命周期脚本全打在一起。**用户只需要一个文件**。

## 给朋友三步装好

### Step 1：把 .fpk 传到飞牛

任选一种方式：

**方式 A（推荐）：直接通过飞牛文件管理器上传**
1. 浏览器打开飞牛桌面 `http://<飞牛-IP>:5666`
2. 「文件管理」→ 拖拽 `fnshare_0.1.0_x86.fpk` 到任意目录（比如 `/vol1/Downloads/`）

**方式 B：scp 命令行**
```bash
scp fnshare_0.1.0_x86.fpk admin@飞牛-IP:/tmp/
```

### Step 2：飞牛应用中心一键装

1. 飞牛桌面 → 「**应用中心**」
2. 右上角 ⋯ → 「**手动安装**」
3. 选刚才上传的 `fnshare_0.1.0_x86.fpk`
4. 弹出 **权限提示**（SYS_ADMIN + /dev/fuse + host 网络）→ 同意
5. 等约 30 秒（自动 load docker 镜像 + 拉起容器）

> **首次安装**会从 .fpk 里 `docker load` 出 fnshare:latest 镜像（约 60 MB 解压后）。后续升级只装新 .fpk，旧镜像复用。

### Step 3：浏览器打开控制台

`http://<飞牛-IP>:4101`

## 第一次用：初始化身份 + 建群/入群

第一次装完，daemon 已经在跑但还没有节点身份。SSH 进去跑一次 init：

```bash
ssh admin@飞牛-IP
sudo docker exec -it fnshare fnshare init --nickname 你的昵称 --contribute-gb 100
sudo docker restart fnshare
```

之后两条路：

### 当群主（建一个新群）
```bash
sudo docker exec -it fnshare fnshare group-create --name "我的朋友圈"

# 拿到 peer id（在 status 输出里）
sudo docker exec fnshare fnshare status

# 生成邀请链接（替换 <你的-飞牛-DDNS-域名> 和 <你的-peer-id>）
sudo docker exec fnshare fnshare invite-create \
  --bootstrap "/dns4/<你的-飞牛-DDNS-域名>/tcp/4001/p2p/<你的-peer-id>" \
  --ttl-hours 72
```

把输出的 `fnshare://join#...` 链接发给朋友（微信、telegram 都行 —— 链接不会被中间服务器看到敏感内容）。

### 加入朋友的群
```bash
sudo docker exec fnshare fnshare group-join "fnshare://join#<朋友发来的链接>"
sudo docker restart fnshare
```

> M9 之后这两步会搬到 Web UI，目前仍需 SSH 一次。

## 让飞牛影视看到 fnshare 文件夹

容器跑起来后，统一资源库自动出现在 `/vol1/@appdata/fnshare/shares/library`（实际路径可能不同，看 `docker inspect fnshare | grep -A2 Source` 确认）。

打开飞牛影视 → 添加媒体源 → 选 fnshare 的 library 目录。

之后朋友们上传的电影 / 视频自动出现，飞牛影视像扫普通本地磁盘一样能用。

## 端口转发（必做，一次性）

每台飞牛的路由器：
- **4001 TCP + UDP** 转发到飞牛 LAN IP（libp2p 通信端口）
- 或者 **IPv6 直连**（家宽 IPv6 普及率挺高，飞牛默认开）

`4101`（Web UI + API）保持 LAN 内网就行，不要对公网开放。

## 卸载

飞牛应用中心 → 找到 fnshare → 卸载。

**用户数据保留**：BadgerDB / 身份密钥 / 库文件都在 `/vol1/@appdata/fnshare/var` 和 `.../shares`，重装能续上。要彻底清掉手动 `rm -rf` 这两个目录。

## 升级到新版本

1. 在 Mac 上重 build 镜像 + .fpk：
   ```bash
   cd /Users/miaorunze/fnshare
   docker build -t fnshare:latest .
   # 改 fpk/manifest 里的 version
   ./fpk/build-fpk.sh
   ```
2. 飞牛应用中心 → 手动安装新 .fpk → 覆盖升级
3. 数据完全保留

## 故障排查

| 现象 | 原因 / 修法 |
|---|---|
| 应用中心装包卡住 / 报权限错 | 权限提示弹出时漏点了同意；卸载重装一遍 |
| 装完 docker ps 看不到 fnshare 容器 | `docker logs $(docker ps -aq -f name=fnshare)` 看错误；多半是 SYS_ADMIN/dev/fuse 没批 |
| /vol1/@appdata/fnshare/shares/library 不是 mountpoint | 飞牛宿主机内核没 fuse 模块；`sudo modprobe fuse` 一次，加到 `/etc/modules` 持久化 |
| Web UI :4101 打不开 | 飞牛防火墙挡住了；或 4101 端口被占；`sudo ss -ltnp | grep 4101` |
| 朋友 join 报 "connect failed" | 你的 DDNS 没解析 / 4001 路由没转发；先 `nc -vz 你的DDNS 4001` 确认 |
| `fnshare put` 报 "need at least 3 members" | 群里成员少于 3；EC(2+1) 至少要 3 节点（含群主） |
| 私有文件名 ls 显示是 `<id>.bin` | 你不是 owner，私有文件名只对 owner 解密 |

## .fpk 是怎么打的（开发者）

源码在 `fpk/`，重打 .fpk：

```bash
cd /Users/miaorunze/fnshare
docker build -t fnshare:latest .   # 先确保镜像最新
./fpk/build-fpk.sh                  # 自动 docker save + tar.gz
ls -lh fpk/fnshare_*.fpk
```
