# fnshare —— 朋友圈分布式存储 · 飞牛 NAS 安装指南

把你的飞牛 NAS 加入朋友圈，几个人各贡献一些空间，凑成一个端到端加密的统一资源库（飞牛影视等本机应用可以像扫普通磁盘一样直接读）。

## 你需要

- 一台飞牛 NAS（fnOS）
- SSH 能登录（飞牛桌面 → 终端机也行）
- 路由器把 **4001 TCP + 4001 UDP** 转发到飞牛的内网 IP（或者飞牛有公网 IPv6）
- 飞牛的 DDNS 域名（飞牛设置 → 网络 → DDNS，免费的）

## 安装（3 步）

### 1. SSH 进飞牛，准备目录

```bash
ssh admin@<你的飞牛 IP>
sudo mkdir -p /vol1/@appdata/fnshare
```

### 2. 下载 compose 文件

```bash
cd /vol1/@appdata/fnshare
sudo curl -o fnshare.yaml https://raw.githubusercontent.com/<repo>/main/deploy/fnshare-friend.yaml
# 或者把群主发给你的 fnshare-friend.yaml 文件传到这里
```

### 3. 改两个字段，启动

用 `sudo vi fnshare.yaml`（或 nano）改：
- `FNSHARE_NICKNAME` —— 你的昵称（朋友们看到的）
- `FNSHARE_QUOTA_GB` —— 你愿意贡献多少 GB 空间

然后启动：
```bash
sudo docker compose -f fnshare.yaml up -d
```

第一次会从 Docker Hub 拉镜像（约 60 MB），3-5 分钟。

## 验证安装成功

```bash
# 看容器状态
sudo docker ps --filter name=fnshare
# 应该看到 STATUS = Up X seconds

# 看启动日志
sudo docker logs fnshare | tail -20
# 应该看到 "FUSE mounted" + "api listening 0.0.0.0:4101"
# 还会看到一行 peer id : 12D3KooW...（这是你的节点身份，记下来）
```

浏览器打开 `http://<飞牛 IP>:4101` 看到 fnshare 控制台 → 装完了。

## 加入群主的群

群主会发给你一段 `fnshare://join#...` 邀请链接。

在控制台「概览」tab 最下面 →「用邀请链接加入」框 → 粘贴 → 点「加入」。

之后看「文件」tab，群里其他人上传的内容都会出现。

## 让飞牛影视看到资源库

飞牛影视添加媒体源 → 选择路径 `/vol1/@appdata/fnshare/library`

之后群里任何人上传的电影、视频，飞牛影视会自动扫到。

## 端口转发

让朋友能从公网连到你的 NAS（P2P 需要双向连接）：

**路由器** → 端口转发 → 添加：
- 4001 TCP → 飞牛内网 IP : 4001
- 4001 UDP → 飞牛内网 IP : 4001

如果你家是 IPv6（中国家宽 IPv6 普及率高），通常**不需要**端口转发，IPv6 直连就行。检查方法：
```bash
ip -6 addr show | grep "inet6 2"
# 如果有看起来像 2409:xxx 这样的全球 IPv6 地址 → 通了
```

## 升级到新版本

群主推了新镜像后，你这样升级（不丢数据）：
```bash
cd /vol1/@appdata/fnshare
sudo docker compose -f fnshare.yaml pull
sudo docker compose -f fnshare.yaml up -d
```

## 卸载

```bash
cd /vol1/@appdata/fnshare
sudo docker compose -f fnshare.yaml down
# 数据保留（在 /vol1/@appdata/fnshare/data 和 .../library）
# 要彻底删：sudo rm -rf /vol1/@appdata/fnshare
```

## 故障排查

| 现象 | 怎么修 |
|---|---|
| 拉镜像慢/超时 | 加 docker hub 国内镜像源（DaoCloud / 中科大 / 阿里云）|
| 容器启动后立刻 Restart | `sudo docker logs fnshare` 看错误，多半是 FUSE 没加载 → `sudo modprobe fuse` |
| Web UI 4101 打不开 | `sudo ss -lntp \| grep 4101` 看端口在不在；防火墙放行 |
| 加入群报 "connect failed" | 群主 NAS 的 4001 没转发，或 IPv6 不通 |

---

有问题去群里 @ 群主，或看完整文档 https://github.com/<repo>
