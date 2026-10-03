# ssh-use

面向 AI Agent 的 SSH 命令执行和文件传输工具，提供连接复用、策略检查、TUI 人工审批和审计。支持本地 daemon，以及多台设备共用的远程服务端。

```bash
go install github.com/zhiylee/ssh-use/cmd/ssh-use@latest
# 从当前源码构建
go build -o ssh-use ./cmd/ssh-use
```

## 给 AI Agent 安装 Skill

Skill 是 Agent 的使用入口：先发现并确认主机，再通过 CLI 执行命令或传输文件，等待人工审批，并正确处理失败、取消和断线。Skill 随二进制内置，安装无需克隆仓库或访问网络。

```bash
# 安装到当前用户；选择正在使用的 Agent
ssh-use skill install --agent claude
ssh-use skill install --agent opencode
ssh-use skill install --agent codex

# 也可安装到当前项目，供团队提交和共享
ssh-use skill install --agent claude --scope project
```

在 Claude Code 中用 `/ssh-use` 调用；OpenCode 可要求「使用 ssh-use skill」；Codex 可用 `$ssh-use`。也可直接提出远程操作请求，由 Agent 根据 Skill 描述选择。示例：「使用 ssh-use 检查 prod 的磁盘空间」「将 ./app.tar.gz 上传到 prod 的 /srv/app.tar.gz」。安装目录、升级、凭据配置和验收方法见 [Agent Skill 接入说明](docs/agent-skill.md)。

Skill 本身不授予权限。远程客户端应使用 `agent` 身份；用户通过另一个使用 `admin` 身份的 TUI 审批。缺少 CLI、主机配置或凭据时，Agent 会报告缺失项。

## Docker Compose 部署

推荐用 Docker Compose 在单台 Linux 服务器上运行集中服务端。仓库提供多阶段 Dockerfile、非 root 服务、持久化目录及初始化脚本；需要已运行的 Docker Engine 和 Docker Compose 插件（v2 或后续版本）。

```bash
# 在仓库根目录执行；域名/IP 必须是客户端实际访问的地址
./deploy/init.sh ssh-gateway.example.com

# 安装服务端专用 SSH 私钥和已经核实的目标机公钥清单
install -m 600 /path/id_ed25519_ssh_use deploy/state/ssh/id_ed25519_ssh_use
install -m 600 /path/verified_known_hosts deploy/state/ssh/known_hosts

# 编辑脚本生成的 .env，将 SSH_USE_BIND_ADDR 设为服务端 LAN/VPN IP 或 0.0.0.0
docker compose up -d
docker compose logs --tail=100 ssh-use
```

默认只发布宿主机 `127.0.0.1:7443`。初始化脚本保留已有 `.env` 和配置，拒绝覆盖已有凭据；生成的客户端目录位于 `deploy/state/server/admin/` 和 `agent/`，可按下文连接客户端。新增主机时，SSH 私钥路径使用**容器内路径** `/home/ssh-use/.ssh/id_ed25519_ssh_use`。

非 root 用户初始化时，容器使用该用户的 UID/GID；root 初始化时使用 `10001:10001`，后续安装文件也需保持该所有权。升级、独立设备凭据、备份及目录权限详见 [Compose 部署说明](deploy/README.md)。

## 服务端模式

```text
CLI / TUI / AI Agent ── gRPC / Protobuf / TLS 1.3 ── ssh-use server ── SSH / SFTP ── 目标服务器
```

服务器清单、策略、SSH 私钥、known_hosts、连接池和审计数据库都在服务端。客户端只需要服务端地址、CA 证书和访问 token。文件以数据块流式中转，不在服务端落地完整文件；本地文件路径始终属于发起 `cp` 的客户端。

### 1. 初始化服务端

在服务端机器上执行，`--host` 必须是客户端实际访问的域名或 IP：

```bash
ssh-use server init --dir ./ssh-use-server --host ssh-gateway.example.com
ssh-use server --dir ./ssh-use-server --listen 0.0.0.0:7443
```

初始化生成：

```text
ssh-use-server/
  tls.crt, tls.key       服务端 TLS 证书和私钥
  auth.yaml             客户端身份、角色与 token 的 SHA-256
  admin/                管理员客户端配置目录
  agent/                执行客户端配置目录
```

目录权限为 0700，文件权限为 0600；已存在的目录不会被覆盖。初始化证书有效期一年，到期前需更换证书并更新客户端信任文件。也可使用自己管理的证书：

```bash
ssh-use server --listen 0.0.0.0:7443 \
  --tls-cert /path/server.crt --tls-key /path/server.key \
  --auth-file /path/auth.yaml
```

默认只监听 `127.0.0.1:7443`；远程访问需显式指定监听地址，并确保客户端可达。远程协议为 gRPC + 二进制 Protobuf + TLS 1.3（HTTP/2），默认端口仍为 7443。反向代理需支持 gRPC、HTTP/2 和长时间流式请求，也可以使用 TCP 透传；普通 HTTP/1 代理不能直接接入。客户端配置、证书和 token 文件格式保持不变。

服务端默认配置仍为 `~/.config/ssh-use/config.yaml`，审计库为 `~/.local/share/ssh-use/ssh-use.db`。可用 `SSH_USE_CONFIG_PATH` 和 `SSH_USE_DATA_DIR` 指定独立目录。同一数据目录只能启动一个 daemon 或 server。建议用专用系统账户运行，进程由 systemd 等服务管理器托管；SIGINT/SIGTERM 会关闭监听并取消当前任务。

### 2. 连接客户端

通过你信任的传输方式，将生成的 `admin/` 目录完整复制到管理设备，然后指定：

```bash
export SSH_USE_CLIENT_CONFIG="$HOME/.config/ssh-use/admin/client.yaml"
ssh-use hosts list
ssh-use tui
```

`client.yaml` 的格式：

```yaml
endpoint: ssh-gateway.example.com:7443
ca_file: ca.pem
token_file: token
```

相对路径以该 YAML 文件所在目录为基准，因此整个配置目录可以直接复制。也可以将配置放到默认位置 `~/.config/ssh-use/client.yaml`（遵循 XDG_CONFIG_HOME；如果指定了 SSH_USE_CONFIG_PATH，则使用其同级目录）。

环境变量 `SSH_USE_SERVER`、`SSH_USE_CA_FILE`、`SSH_USE_TOKEN_FILE` 可覆盖各字段；通过环境变量提供的相对文件路径以当前工作目录为基准。系统信任的 TLS 证书可省略 `ca_file`。没有禁用证书校验的开关。

为 AI Agent 指定 `agent/client.yaml`，不要提供管理员 token：

```bash
SSH_USE_CLIENT_CONFIG=/path/agent/client.yaml ssh-use exec prod -- uptime
```

| 角色 | 权限 |
| --- | --- |
| admin | 管理主机、执行、传输、查看全部任务、TUI 审批、策略和连接控制 |
| agent | 查询主机、执行、传输、查询和取消自己身份提交的任务 |

权限由服务端验证，客户端不能通过声明自己是 TUI 获得审批权。凭据的隔离仍依赖操作系统权限：同一系统账号下能读取管理员 token 的进程也能使用它。

### 3. 为每台设备签发独立凭据

在服务端本机执行；`--out` 必须是一个尚不存在的目录：

```bash
ssh-use server client add mac-admin --dir ./ssh-use-server \
  --role admin --endpoint ssh-gateway.example.com:7443 --out ./mac-admin
ssh-use server client add linux-agent --dir ./ssh-use-server \
  --role agent --endpoint ssh-gateway.example.com:7443 --out ./linux-agent
```

将各自配置目录复制到对应设备。每台设备使用不同 token 时，审计 `source` 会分别记录 `device:mac-admin`、`device:linux-agent`；客户端不能伪造它。初始化的 admin/agent 是引导用身份，多台设备共用同一 token 会被视为同一个身份。

撤销设备的新 RPC 权限：

```bash
ssh-use server client remove linux-agent --dir ./ssh-use-server
```

auth.yaml 在每次 RPC 开始时重新读取，修改无需重启；即使客户端复用已有连接，撤销后的新请求也会被拒绝。已开始的订阅、传输和任务不会因撤销自动终止；需要立即关闭所有已有会话时重启服务。最后一个管理员不能通过此命令删除。

## 服务器增删改查

这些命令作用于当前选中的执行端：远程模式操作服务端配置，本地模式操作本地 daemon 配置。

```bash
# 新增；--key 是执行端上的路径，远程模式下不是客户端路径
ssh-use hosts add prod --addr 10.0.0.10 --user deploy --port 22 \
  --key /home/ssh-use/.ssh/id_ed25519_ssh_use --tags prod,app

ssh-use hosts list
ssh-use hosts list --json
ssh-use hosts get prod --json

# 只修改给出的字段
ssh-use hosts update prod --addr 10.0.0.11 --user deploy
ssh-use hosts update prod --tags ''

ssh-use hosts delete prod
```

`host` 是 `hosts` 的别名；`ls`、`rm` / `remove` 分别对应 `list`、`delete`。名称使用字母、数字、点、下划线和连字符，最长 128 字符。

`list/get` 展示保存的原始字段：空 user/key、port=0 表示继承执行端的 defaults。`update --user ''`、`--key ''`、`--port 0` 恢复对应默认值；`--tags ''` 清空标签。

写操作先获取配置版本，再提交修改；发生并发修改会返回 `config_conflict`，不会自动覆盖。自动化调用可传 `--if-revision <list/get返回的config_revision>`。配置验证成功后原子保存并热加载；失效的空闲连接会关闭，正在执行的操作继续使用自己的配置快照。远程模式下，等待审批或排队的任务如果发现配置变化，会拒绝执行并要求重新提交。

远程模式只能执行已登记的主机，未知主机不会被当作地址直接连接。服务端 SSH 认证继续使用现有专用私钥文件；第一版不支持加密 SSH 私钥或 ssh-agent。目标机的公钥必须预先核实并写入**服务端账户**的 `~/.ssh/known_hosts`，不会自动信任未知主机。

## 执行、审批和文件传输

```bash
ssh-use exec prod -- uptime
ssh-use exec prod -- 'systemctl restart nginx'
ssh-use tui              # 管理员打开；可在另一台设备审批
ssh-use cp --atomic ./app.tar.gz prod:/srv/app.tar.gz
ssh-use cp prod:/var/log/app.log ./app.log
```

exec 保持 stdout/stderr 和远端退出码语义。普通网络断开不取消已经接受的命令；服务端继续执行，受命令超时限制。Ctrl+C 会尝试发送显式取消请求，取消仍遵循现有 SSH 执行器的语义。

```bash
ssh-use jobs list          # 管理员；JSON 输出
ssh-use jobs get <任务ID>  # 管理员或提交该任务的执行身份；JSON 输出
```

命令提交与输出订阅是两个独立 RPC。输出订阅在临时断线后自动重连，并从最后收到的游标继续读取；不会再次提交或执行命令。每个任务最多保留约 4 MiB / 1024 条实时消息，已完成任务的输出日志合计上限约 32 MiB、最多 200 个任务；这些实时日志只在当前服务进程内保留。游标过期或服务重启后，CLI 会明确报错并输出任务 ID 和请求 ID，可用 `jobs get` 查询已保存的结果与有上限的输出。

每次 exec 自动生成请求 ID。需要重试同一次提交时，必须复用原 ID：

```bash
ssh-use exec --request-id deploy-20261003-001 prod -- 'systemctl restart app'
```

同一设备身份、同一请求 ID、同一命令只接受一次，重复提交返回已有任务，不重跑；同一请求 ID 配不同命令会被拒绝。命令摘要和已接受标记持久化到 SQLite，即使审计记录按保留期清除，也不会重新执行旧 ID。请求标记目前不自动清理。CLI 不会自动重试 SSH 命令，也不保证远端操作恰好成功一次。

服务端异常退出后，未完成的历史任务标记为 `UNKNOWN`，可能已经产生远端影响，需要核对后决定是否重新提交。不会在重启时重新执行或恢复待审批任务。文件传输依赖客户端数据连接，断开后失败，不支持自动续传；下载失败会清理客户端临时文件，上传建议使用 `--atomic`。

## 远程通信与升级

管理、审批和查询使用普通 RPC；命令采用 `SubmitJob` + `WatchJob`；TUI 采用 `WatchEvents`；文件使用双向 `Transfer` 流，保留审批、Ready、校验和与本地写入确认流程。文件和 stdout/stderr 数据块使用 Protobuf `bytes`，不经过 Base64，单块上限 128 KiB。文件经服务端流式中转，服务端不落地完整文件。

同一客户端进程复用 gRPC 连接，普通 RPC 默认超时 10 秒；TUI 操作沿用 5 秒超时。不同 CLI 进程各自建立连接。服务端最多接收 128 个并发 RPC 和 128 个已接受的命令任务，单条客户端请求上限 1 MiB、服务端回复上限 8 MiB。慢输出订阅写入超过 10 秒会被断开，独立任务继续执行；文件传输仍受命令超时限制。

TUI 平时接收增量事件和每 10 秒一次的轻量心跳，临时断线后自动重连。事件窗口最多约 4 MiB / 1024 条消息，游标过期或服务重启后自动重新获取快照。TUI 中每个任务的 stdout/stderr 预览最多各 4 KiB；完整实时输出由命令的 `WatchJob` 流提供，持久化输出仍遵循审计配置。

这是远程协议 v2，不能与旧版 TLS JSON Lines 客户端/服务端混用。升级时同时更新远程服务端和所有客户端；现有 `client.yaml`、证书、token、主机配置和 SQLite 审计库可以继续使用。

## 本地模式

不配置远程 endpoint / client.yaml 时，原有本地 daemon 自动启动流程继续工作，本地仍使用 Unix socket + JSON Lines，二进制块保持原来的 Base64 兼容格式。服务端配置错误、证书错误或不可达都会显式报错，不会自动回退到本地。

## 验证

```bash
go test ./...
go test -race ./...
go vet ./...
```

测试覆盖 TLS 信任与每次 RPC 的 token 校验、连接复用与权限撤销、角色权限、跨设备审批、配置冲突与持久化、并发提交去重、断线执行与输出恢复、TUI 事件重连与游标失效，以及二进制文件分块传输与下载确认。SSH/SFTP 执行器另有进程内真实 SSH 服务集成测试。

Protobuf 定义和生成代码位于 `internal/rpcpb/`。普通构建不需要安装 protoc；修改 `.proto` 后，在安装 protoc 的开发环境运行：

```bash
go generate ./internal/rpcpb
go test ./internal/protocol -run '^$' -bench BenchmarkChunkRoundTrip -benchmem
```

生成脚本使用固定版本的 Go 插件，并将临时工具安装在临时目录。基准测试比较同一个 128 KiB 数据块的 JSON/Base64 与二进制 Protobuf 编解码，不能直接代表 SSH/SFTP 端到端吞吐。
