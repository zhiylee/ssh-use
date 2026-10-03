# Docker Compose 部署

适用于单台 Linux 服务器上的单实例集中服务端。需要 Docker Engine 和 Docker Compose 插件（v2 或后续版本）；镜像在本机从当前源码构建，运行时使用非 root 用户。构建不需要宿主机安装 Go。

## 首次初始化

在仓库根目录执行，建议使用可访问 Docker 的专用非 root 账户：

```bash
./deploy/init.sh ssh-gateway.example.com
```

参数是客户端实际访问的域名或 IP，不带协议、端口或 IPv6 方括号。脚本会：

1. 从 `.env.example` 生成 `.env`，非 root 用户使用当前 UID/GID，root 使用 `10001:10001`。已有 `.env` 会保留，以 Compose 实际解析出的 UID/GID 为准。
2. 创建 `deploy/state/`，复制默认主机/策略配置，准备空的 `known_hosts`。
3. 构建镜像，通过离线 `admin` 辅助容器生成 TLS 凭据及 admin/agent 客户端配置目录。

脚本拒绝覆盖已有 `deploy/state/server/`。初始化后若构建失败，可修复后重试；凭据生成过程中失败留下的目录应先检查并单独移走，再重新初始化。升级时直接构建和重建服务，保留现有凭据。

默认发布地址是 `127.0.0.1:7443`。编辑 `.env`，将 `SSH_USE_BIND_ADDR` 设为服务端 LAN/VPN IP 或 `0.0.0.0`，并配置允许客户端访问的 TCP 端口。容器内始终监听 `0.0.0.0:7443`；远程协议是 gRPC + Protobuf + TLS 1.3（HTTP/2）。反向代理需要支持 gRPC、HTTP/2 和长流，也可使用 TCP 透传。

需要其他外部端口时，在**初始化前**复制并编辑 `.env`：

```bash
cp .env.example .env
# 非 root 用户将 SSH_USE_UID/GID 改成 id -u / id -g 的结果
# 修改 SSH_USE_PORT，例如 8443；同时设置 SSH_USE_BIND_ADDR
./deploy/init.sh ssh-gateway.example.com
```

初始化生成的 `client.yaml` 会使用该外部端口。部署后修改外部端口，还需更新各设备 `client.yaml` 的 `endpoint`，再运行 `docker compose up -d` 应用端口映射。

## SSH 凭据与主机配置

使用服务端专用、未加密的 SSH 私钥，并将公钥登记到目标机。将私钥和经核实的目标机公钥清单安装到宿主机挂载目录：

```bash
install -m 600 /path/id_ed25519_ssh_use deploy/state/ssh/id_ed25519_ssh_use
install -m 600 /path/verified_known_hosts deploy/state/ssh/known_hosts
```

root 初始化或安装文件时，按照 `.env` 中的 UID/GID 设置文件所有权；默认值对应：

```bash
sudo chown 10001:10001 deploy/state/ssh/id_ed25519_ssh_use deploy/state/ssh/known_hosts
```

`known_hosts` 必须覆盖主机配置中实际使用的目标地址和端口；非 22 端口使用 `[地址]:端口` 格式。初始化创建的空文件只是占位，未知主机仍会被拒绝。服务端不支持加密 SSH 私钥或 ssh-agent。

编辑 `deploy/state/config/config.yaml` 可提前登记主机；也可启动后从管理客户端执行：

```bash
ssh-use hosts add prod --addr 10.0.0.10 --user deploy \
  --key /home/ssh-use/.ssh/id_ed25519_ssh_use --tags prod,app
```

`--key` 是容器内路径。示例配置继承现有默认策略：普通命令允许执行，敏感操作按规则请求审批。管理员可以通过客户端 TUI 修改策略和审批。

## 启动和连接

```bash
docker compose up -d
docker compose ps
docker compose logs --tail=100 ssh-use
```

将 `deploy/state/server/admin/` 完整复制到管理设备，将 `agent/` 完整复制到执行设备。在对应设备安装与服务端对应版本的 CLI 后配置：

```bash
export SSH_USE_CLIENT_CONFIG=/path/admin/client.yaml
ssh-use hosts list
ssh-use tui
```

`hosts list` 同时验证网络、TLS 信任和 token 认证；登记目标主机后，可运行 `ssh-use exec prod -- uptime` 验证 SSH 连通性。Agent 使用自己的客户端配置目录。

## 独立设备凭据

`admin` 服务仅在显式运行时启用，无网络、无重启策略，以相同 UID/GID 对挂载目录执行凭据管理。新增设备：

```bash
docker compose run --rm --no-deps admin server client add mac-admin \
  --dir /bootstrap/server --role admin \
  --endpoint ssh-gateway.example.com:7443 --out /bootstrap/mac-admin
```

将生成的 `deploy/state/mac-admin/` 完整复制到对应设备。`--out` 目录必须尚不存在。撤销设备：

```bash
docker compose run --rm --no-deps admin server client remove mac-admin --dir /bootstrap/server
```

服务端只读挂载整个凭据目录，辅助容器原子替换 `auth.yaml` 后，已有连接上的新 RPC 立即使用更新后的文件。已开始的流和任务不会自动终止；需要关闭全部现有会话时执行 `docker compose restart ssh-use`。

## 持久化与权限

| 宿主机路径 | 容器路径 | 常驻服务权限 |
| --- | --- | --- |
| `deploy/state/config/` | `/etc/ssh-use/` | 读写，配置保存与热加载 |
| `deploy/state/data/` | `/var/lib/ssh-use/` | 读写，SQLite 与实例锁 |
| `deploy/state/server/` | `/run/ssh-use-server/` | 只读，TLS 与身份凭据 |
| `deploy/state/ssh/` | `/home/ssh-use/.ssh/` | 只读，专用 SSH 私钥与 known_hosts |

配置和凭据使用目录挂载，支持原子替换文件。Bind mount 设置 `create_host_path: false`，路径缺失会报错，避免 Docker 自动创建 root 所有的空目录。宿主机目录权限为 `0700`，敏感文件为 `0600`，所有者须与 `.env` 中 UID/GID 一致；更换运行账户需同步调整现有数据所有权。

镜像根文件系统只读，`/tmp` 使用临时内存挂载。Compose 配置自动重启和日志轮转；同一数据目录仅允许一个服务实例。`.env`、`deploy/state/` 已从 Git 排除，Docker 构建上下文仅包含构建所需源码，凭据不进入镜像。

## 升级、备份和恢复

先确认运行中和待审批的任务已经处理完。重启会取消运行中的操作，文件传输会失败；未完成的历史任务可能标记为 `UNKNOWN`，需核对目标机状态。Compose 重建服务不提供无中断升级。

升级前停服备份，保留 SQLite 和持久化请求 ID，避免丢失重复请求保护：

```bash
docker compose stop ssh-use
umask 077
sudo tar -C . -czf - .env deploy/state > /path/ssh-use-backup.tar.gz
```

备份包含私钥和客户端 token，应按凭据管理；建议使用绝对备份路径。停服后备份整个数据目录，保证数据库一致性。然后更新当前 checkout 的源码，在 `.env` 中设置新的 `SSH_USE_IMAGE` 版本标签，例如 `ssh-use:20261003`：

```bash
docker compose build --pull ssh-use
docker compose up -d ssh-use
docker compose logs --tail=100 ssh-use
```

恢复时先停止服务，在仓库根目录恢复 `.env` 和 `deploy/state/`，保持文件权限与 UID/GID 一致，使用对应版本镜像，再运行 `docker compose up -d ssh-use`。回滚旧镜像前需检查数据库格式兼容性；必要时在停服状态下恢复相应备份。

`docker compose down` 保留这些宿主机目录。TLS 初始化证书有效期一年，更换证书后需要更新客户端信任文件并重启服务；修改 `auth.yaml` 无需重启即可影响新 RPC。

## 远程协议 v2 升级

新版客户端和服务端都使用 gRPC，旧版 TLS JSON Lines 远程协议不兼容。先更新镜像和各设备 CLI，再重新创建服务容器；配置、证书、token 和审计数据目录保留。升级会中断现有流，应先处理仍在运行的任务。不同进程之间不共享客户端连接，本地 daemon 模式继续兼容原 Unix socket 协议。
