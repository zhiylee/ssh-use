# ssh-use 设计方案

`ssh-use` 是一个面向 AI agent 的远程命令执行工具。

当前已支持可选的集中服务端模式、gRPC / Protobuf / TLS 远程通信、每次 RPC 的 token 认证、跨设备 TUI 审批，以及 `hosts` 增删改查命令。部署方式和实际行为以 [README](README.md) 为准；下文保留最初的本地 daemon 设计背景。

GitHub 仓库：<https://github.com/zhiylee/ssh-use>。

安装 CLI：

```bash
go install github.com/zhiylee/ssh-use/cmd/ssh-use@latest
```

目标是让 Claude Code、OpenCode 等 agent 不再频繁执行 `ssh root@host "command"`，而是通过本地常驻 daemon 复用 SSH 连接，并提供终端交互式 TUI 让用户实时审查、审批、拒绝和查看历史命令。

## 从 agent-ssh 迁移

项目、CLI 和正式 Agent Skill 统一使用 `ssh-use`。升级前停止旧 daemon，再迁移已有配置和审计库：

- 配置文件：`~/.config/agent-ssh/config.yaml` -> `~/.config/ssh-use/config.yaml`。
- 审计数据库：`~/.local/share/agent-ssh/agent-ssh.db` -> `~/.local/share/ssh-use/ssh-use.db`。首次打开会自动迁移旧错误码列，保留历史记录。
- 环境变量前缀：`AGENT_SSH_` -> `SSH_USE_`，适用于 `SOURCE`、`CONFIG_PATH`、`DATA_DIR` 和 `RUNTIME_DIR`。
- 本地协议与审计记录中的错误码字段：`agent_ssh_error_code` -> `ssh_use_error_code`。

设置了 XDG 路径时，使用相应目录。迁移审计库时应保留整个数据目录；若存在同名的 `-wal`、`-shm` 或 `-journal` 配套文件，需一起迁移并使用新的数据库文件名前缀。原 SSH 私钥无需重建，在迁移后的配置中继续使用原路径即可；新配置的默认路径为 `~/.ssh/id_ed25519_ssh_use`。

## 核心目标

- AI agent 使用简单命令执行远程操作。
- 本地 daemon 长期维护 SSH 连接池，避免频繁建立 SSH 连接。
- 用户通过 TUI 统一查看 AI 执行过的命令。
- 支持全自动、全审批、敏感命令审批三种模式，默认使用 `sensitive`。
- 支持策略规则识别高风险命令。
- 支持流式返回 stdout/stderr，避免长命令看起来像卡死。
- 支持用户在 TUI 中取消命令、暂停新命令、紧急停止异常执行流。
- 支持 SQLite 本地审计。
- 支持 TLS 服务端统一执行和审计，客户端可从多台机器接入。
- 支持 `hosts list/get/add/update/delete` 管理当前执行端的服务器清单。
- 支持空闲连接自动断开，下次命令自动重连。
- 不做 Web UI，所有用户交互都在终端 TUI 内完成。

## 非目标

- 不做 Web 控制台。
- 不做远程 HTTP API。
- 不做交互式 SSH shell。
- 不复用同一个远端 shell。
- 不提供 `ssh-use approve`、`ssh-use reject`、`ssh-use pending` 这类命令行审批入口。
- 不把 `ssh-agent` 作为必需组件。
- 不做复杂 RBAC、多用户系统、云端审计。
- 不做文件管理器、PTY、远程终端模拟器。

## 产品形态

AI agent 使用：

```bash
ssh-use exec <host> -- <command>
ssh-use cp [--atomic] <source> <destination>
```

用户审批使用：

```bash
ssh-use tui
```

当普通 TUI 布局在小终端或异常终端中不可用时，用户可以使用安全交互模式：

```bash
ssh-use tui --safe
```

`--safe` 仍然是 TUI，不提供命令行审批入口，只使用单栏、少颜色、少布局的保守界面。

示例：

```bash
ssh-use exec prod1 -- "uptime"
ssh-use exec prod1 -- "docker ps"
ssh-use exec prod1 -- docker ps
ssh-use exec prod1 -- "systemctl restart nginx"
ssh-use tui
```

用户审批、拒绝、查看历史、切换模式、查看连接状态都在 `ssh-use tui` 中完成。

## 整体架构

```text
Claude Code / OpenCode
        |
        | ssh-use exec prod1 -- "systemctl restart nginx"
        v
ssh-use CLI
        |
        | Unix socket + JSON lines
        v
ssh-use daemon
        |
        | policy engine
        | approval manager
        | audit logger
        | SSH connection pool
        v
remote servers


User
        |
        | ssh-use tui
        v
interactive terminal dashboard
        |
        | Unix socket + event stream
        v
ssh-use daemon
```

## 关键设计决策

### 必须使用 daemon

普通 CLI 进程执行完就退出，进程内 SSH 连接也会消失。

因此需要本地 daemon 维护 SSH 连接池：

```text
ssh-use exec:
短生命周期客户端，负责把命令发给 daemon，并等待结果。

ssh-use daemon:
常驻进程，负责 SSH 连接池、策略判断、审批等待、审计存储。

ssh-use tui:
终端交互控制台，负责用户审查和审批。
```

`ssh-use exec` 和 `ssh-use tui` 都会自动启动 daemon。

### 不依赖 ssh-agent

默认直接读取配置中的私钥文件：

```yaml
hosts:
  prod1:
    addr: 10.0.0.11
    user: root
    key: ~/.ssh/id_ed25519_ssh_use
```

`ssh-agent` 只作为未来可选增强，不是核心路径。

推荐用户为 `ssh-use` 创建专用 SSH key，便于权限隔离和审计。

### 复用 ssh.Client，每条命令新建 ssh.Session

SSH 层次：

```text
TCP connection
  -> SSH transport
    -> authentication
      -> SSH channel / session
        -> exec command
```

复用 `ssh.Client` 可以避免重复 TCP 连接、SSH 握手、密钥交换和认证。

每条命令新建 `ssh.Session` 只是在已有 SSH 连接上打开一个新的 channel，开销较小。

推荐模型：

```text
第一次执行:
建立 TCP/SSH/auth -> NewSession -> Run command

后续执行:
复用 SSH client -> NewSession -> Run command
```

不复用远端 shell，原因是：

- stdout/stderr 边界难判断。
- exit code 难准确获取。
- 命令超时和清理复杂。
- 多个 agent 并发时容易串输出。
- `cd`、`export`、`alias` 会污染后续命令。
- 需要额外处理 prompt、PTY、信号、转义。

## 执行流程

```text
1. AI agent 执行 ssh-use exec prod1 -- "cmd"
2. CLI 连接本地 daemon
3. daemon 创建 command record
4. policy engine 判断 allow / approve / block
5. allow: 直接执行
6. approve: 进入 TUI 审批队列，CLI 阻塞等待
7. block: 直接拒绝
8. 执行时复用 ssh.Client
9. 每条命令创建新的 ssh.Session
10. stdout/stderr/remote_exit_code/ssh_use_error_code 写入审计数据库
11. CLI 把结果返回给 AI agent
12. TUI 实时更新命令状态
```

命令状态：

```text
CREATED
PAUSED
PENDING_APPROVAL
APPROVED
REJECTED
QUEUED
RUNNING
DONE
FAILED
TIMEOUT
BLOCKED
CANCELLED
CANCEL_FAILED
```

`FAILED` 表示远端命令已执行但返回非零或执行出错。

`ssh_use_error_code` 表示 ssh-use 自身错误，例如审批拒绝、审批超时、连接失败、策略阻断、取消。

`remote_exit_code` 表示远端命令 exit code。未执行远端命令时为空。

## CLI 命令设计

只暴露三个主要命令：

```bash
ssh-use exec <host> -- <command>
ssh-use cp [--atomic] <source> <destination>
ssh-use tui
```

`exec` 支持两种命令传递方式：

```bash
ssh-use exec prod1 -- "docker ps"
ssh-use exec prod1 -- docker ps
```

解析规则：

```text
1. `--` 后只有一个参数时，直接作为远端命令字符串。
2. `--` 后有多个参数时，使用 POSIX shell quoting 组合成远端命令。
3. 涉及管道、重定向、`&&`、`;`、变量展开时，推荐 AI agent 使用单字符串形式。
4. CLI 不在本地执行命令拼接后的内容，只发送给 daemon。
```

示例：

```bash
ssh-use exec prod1 -- "cd /app && git status"
ssh-use exec prod1 -- "journalctl -u nginx -n 100 --no-pager"
ssh-use exec prod1 -- docker logs --tail=100 api
```

内部可保留 daemon 子命令用于调试，但不作为用户主流程。

### 文件传输

`cp` 使用类似 scp 的端点形式，但不承诺兼容 scp 的全部参数和行为：

```bash
# 本地上传到远端
ssh-use cp ./app.yaml prod1:/etc/app/app.yaml

# 远端下载到本地
ssh-use cp prod1:/var/log/app.log ./app.log

# stdin/stdout
generate-config | ssh-use cp - prod1:/etc/app/config.yaml
ssh-use cp prod1:/var/log/app.log - > ./app.log
```

第一版约束：

```text
只支持单个普通文件。
远端端点格式为 <host>:/absolute/path。
host 使用 ssh-use 配置中的用户和密钥，不支持 user@host 临时覆盖。
不支持本地到本地、远端到远端、目录递归和远端 shell 通配符。
默认直接写入目标，与 scp 类似；失败时已有目标可能不完整。
--atomic 使用同目录临时文件和 SFTP POSIX rename，不支持时直接报错。
文件内容流式传输，不写入命令审计；审计只记录端点和执行结果。
```

文件传输复用 daemon 中的 `ssh.Client`，每次传输新开 SFTP channel。上传和下载继续经过策略判断、TUI 审批、单 host 队列、取消、超时和审计流程。

不提供：

```bash
ssh-use approve <id>
ssh-use reject <id>
ssh-use pending
ssh-use logs
ssh-use status
```

这些能力全部放进 TUI。

## AI 执行体验

`ssh-use exec` 的行为必须尽量接近本地执行命令。

输出规则：

```text
远端 stdout -> 本地 stdout
远端 stderr -> 本地 stderr
远端 exit code -> 本地 exit code
ssh-use 自身提示 -> 本地 stderr
```

daemon 执行远端命令时必须流式转发输出，不等命令结束后一次性返回。

原因：

- AI agent 需要实时看到构建、部署、测试、日志输出。
- 长命令如果长期无输出，容易被 agent 判断为卡死。
- 用户在 TUI 中也需要看到 running 命令的最近输出。

流式输出只影响 CLI 和 TUI 展示。审计数据库只保存脱敏、截断后的输出副本。

队列规则：

```text
同一 host 默认串行执行。
命令被串行队列阻塞时，状态为 QUEUED。
CLI 在 stderr 中提示 queue position。
TUI Activity 显示 queued command 和前序 command id。
```

示例：

```text
ssh-use: queued on prod1 behind cmd_01JZAAA, position=2
```

CLI 被中断时的行为：

```text
pending / queued 命令：标记为 CANCELLED，不再执行。
running 命令：关闭对应 ssh.Session，尽量终止远端命令。
已完成命令：不受影响。
```

这样可以避免 AI agent 超时或用户 Ctrl-C 后留下孤儿待审批项或孤儿远端命令。

## AI 等待审批体验

当命令需要审批且 TUI 已打开时，TUI 自动出现待审批项。

当命令需要审批但 TUI 未打开时，`ssh-use exec` 输出：

```text
ssh-use: waiting for user approval
open console: ssh-use tui

id: cmd_01JZABC
host: prod1
risk: high
command: systemctl restart nginx
```

用户打开 TUI 后可以审批。

等待审批期间，`ssh-use exec` 必须定期向 stderr 输出心跳，默认每 30 秒一次：

```text
ssh-use: still waiting for approval id=cmd_01JZABC elapsed=30s open_console="ssh-use tui"
```

如果 TUI 已连接，心跳中显示：

```text
ssh-use: waiting for approval in TUI id=cmd_01JZABC elapsed=30s
```

心跳不能写入 stdout，避免污染远端命令输出。

审批通过后，原 `ssh-use exec` 继续执行远程命令并返回结果。

审批拒绝后，`ssh-use exec` 返回：

```text
ssh-use: command rejected by user
```

建议 exit code：

```text
126
```

审批超时返回：

```text
ssh-use: approval timeout
```

建议 exit code：

```text
124
```

## 执行模式

支持三种模式：

```text
auto:
自动执行所有非 block 命令，但 block 规则仍然阻断。

sensitive:
allow 自动执行，approve 进入 TUI 审批，未命中规则的命令按 default_action 处理，block 阻断。

approval:
所有非 block 命令都进入 TUI 审批。
```

默认模式：

```text
sensitive
```

默认模式约定：

```text
首次启动没有配置文件时，daemon 使用 sensitive。
配置文件缺少 policy.mode 时，daemon 使用 sensitive。
TUI 切换模式后，应持久化当前 mode。
重启 daemon 后，优先使用配置中持久化的 mode。
```

`auto` 模式下仍保留 `block`，避免明显危险命令自动执行。

默认配置使用只审批敏感命令的策略：

```text
mode: sensitive
default_action: allow
```

也就是说，默认只审批敏感命令：命中 approve 规则的命令进入 TUI，命中 block 规则的命令阻断，其他命令自动执行并记录审计。

## 策略引擎

策略使用规则匹配，第一版用正则即可。

默认策略是只审批敏感命令：明确危险命令阻断，明确敏感命令审批，其他命令自动执行并记录审计。

规则动作：

```text
allow:
直接执行。

approve:
进入 TUI 审批。

block:
直接阻断。
```

风险等级：

```text
low
medium
high
critical
```

动作优先级：

```text
block > approve > allow
```

默认行为：

```text
auto 模式:
命中 block 则阻断，其他命令自动执行。

sensitive 模式:
命中 allow 则自动执行。
命中 approve 则进入 TUI 审批。
命中 block 则阻断。
未命中规则则执行 default_action，默认 allow。

approval 模式:
命中 block 则阻断，其他命令全部进入 TUI 审批。
```

Review 详情中必须展示策略判断原因：

```text
matched rule
matched pattern
final action
risk
priority source
default_action 是否参与判断
```

配置示例：

```yaml
policy:
  mode: sensitive
  default_action: allow
  builtin_rules: true

  rules:
    - name: readonly_inspection
      action: allow
      risk: low
      patterns:
        - "^ls(\\s|$)"
        - "^pwd$"
        - "^whoami$"
        - "^hostname$"
        - "^uptime$"
        - "^df\\s"
        - "^free\\s"
        - "^docker ps(\\s|$)"
        - "^docker logs\\b"
        - "^systemctl status\\s"
        - "^journalctl\\s"

    - name: service_mutation
      action: approve
      risk: high
      patterns:
        - "\\bsystemctl\\s+(restart|stop|start|reload)\\b"
        - "\\bdocker\\s+(restart|stop|rm|rmi)\\b"
        - "\\bdocker compose\\s+up\\b"

    - name: destructive_block
      action: block
      risk: critical
      patterns:
        - "\\brm\\s+-rf\\s+/($|\\s)"
        - "\\bmkfs\\b"
        - "\\bdd\\s+.*\\bof=/dev/"
        - "\\bshutdown\\b"
        - "\\breboot\\b"
```

需要审批的典型命令：

```bash
systemctl restart nginx
docker restart api
docker compose up -d
chmod
chown
mv
cp
tee
git pull
```

应阻断或强提示的典型命令：

```bash
rm -rf /
mkfs
dd if=... of=/dev/...
shutdown
reboot
iptables -F
ufw disable
curl ... | sh
wget ... | bash
docker system prune
kubectl delete
```

## TUI 总体设计

TUI 是用户主界面。

启动：

```bash
ssh-use tui
```

TUI 启动后：

```text
1. 自动启动 daemon
2. 获取当前快照
3. 订阅 daemon 事件流
4. 实时展示命令、审批、连接、策略
5. 用户操作通过 Unix socket 发回 daemon
```

TUI 必须支持两种布局：

```text
normal:
默认布局，宽屏时显示 Review、Detail、Recent Activity。

safe:
保守布局，单栏列表 + 详情页，适合小终端、颜色异常终端、渲染异常场景。
```

响应式规则：

```text
终端宽度 >= 120 且高度 >= 32: 使用三块布局。
终端宽度 < 120 或高度 < 32: 自动切换单栏布局。
用户传入 --safe: 强制单栏布局。
```

页面：

```text
1 Review
2 Activity
3 Connections
4 Policy
5 Settings
```

快捷键：

```text
1-5       切换页面
j/k       上下移动
enter     查看详情
a         approve selected; configured risks require Enter confirmation
r         reject immediately
x         cancel selected queued/running command
p         pause/resume new commands
!         emergency stop
m         切换模式
/         过滤
esc       关闭弹窗或取消输入
?         帮助
q         退出 TUI
```

第一版不做批量审批、临时信任、规则编辑，降低误操作风险。

## 用户控制和急停

TUI 必须提供取消和暂停能力，避免 AI agent 异常执行时用户只能等待超时。

取消命令：

```text
x cancel selected
```

取消行为：

```text
PENDING_APPROVAL: 标记 CANCELLED，唤醒等待中的 exec，返回 ssh-use cancellation。
QUEUED: 标记 CANCELLED，不进入执行。
RUNNING: 关闭对应 ssh.Session，尽量终止远端命令。
DONE / FAILED / BLOCKED / REJECTED / TIMEOUT: 不允许取消。
```

暂停新命令：

```text
p pause/resume new commands
```

暂停后：

```text
新 exec 请求仍会被记录和展示，但状态为 PAUSED。
PAUSED 命令不会进入策略审批或远端执行。
用户 resume 后，PAUSED 命令按创建顺序继续进入正常流程。
```

紧急停止：

```text
! emergency stop
```

紧急停止需要二次确认。

确认后：

```text
1. daemon 进入 paused 状态。
2. 所有 PENDING_APPROVAL / QUEUED / PAUSED 命令标记 CANCELLED。
3. TUI 弹窗列出 RUNNING 命令，用户选择是否逐个 cancel。
4. 新 exec 请求只记录，不执行，直到用户 resume。
```

状态栏必须显著展示暂停状态：

```text
PAUSED: new commands are not executing
```

## Review 页面

默认页面，用于处理待审批命令。

布局示例：

```text
┌─ ssh-use ───────────────────────────────────────────────────────────────┐
│ Mode: Sensitive   Pending: 2   Running: 1   Connected: 3   Source: all  │
├───────────────────────────────┬───────────────────────────────────────┤
│ Pending Review                │ Command Detail                        │
│                               │                                       │
│ > HIGH   prod1  root  12s     │ ID: cmd_01JZABC                       │
│   systemctl restart nginx     │ Source: claude-code                   │
│                               │ Workspace: /root/codes/app            │
│   HIGH   prod2  root  04s     │ Host: prod1                           │
│   docker compose up -d        │ User: root                            │
│                               │ Risk: high                            │
│                               │ Rule: service_mutation                │
│                               │                                       │
│                               │ Command:                              │
│                               │ systemctl restart nginx               │
│                               │                                       │
│                               │ Reason:                               │
│                               │ Service restart changes runtime state │
├───────────────────────────────┴───────────────────────────────────────┤
│ Recent Activity                                                        │
│ 14:21:09 DONE      LOW       prod1  uptime                  exit=0 81ms │
│ 14:21:14 PENDING   HIGH      prod1  systemctl restart nginx             │
│ 14:21:18 RUNNING   LOW       prod2  docker logs --tail=100 api          │
│ 14:21:22 BLOCKED   CRITICAL  prod1  rm -rf /                            │
├───────────────────────────────────────────────────────────────────────┤
│ a approve r reject x cancel p pause ! stop m mode 1-5 pages / filter ? │
└───────────────────────────────────────────────────────────────────────┘
```

排序：

```text
critical > high > medium > low
同风险按创建时间升序
```

需要二次确认的审批风险级别由配置决定，默认是 `high` 和 `critical`：

```yaml
approval:
  confirm_risks: [high, critical]
```

配置为 `[]` 时，所有风险级别都按 `a` 直接审批。可用级别为 `low`、`medium`、`high`、`critical`。

二次确认交互：

```text
APPROVE COMMAND?

Host: prod1

Command:
systemctl restart nginx

[Enter] Approve    [Esc] Go back
```

审批动作与确认使用不同按键，避免终端按键自动重复导致误批。

当命令同时满足以下条件时，确认要求更严格：

```text
target host 属于 prod 命名或配置标记 prod
remote user 是 root
risk 是 high 或 critical
```

此时需要输入 host 名称或命令 id 后 4 位确认。

`critical + block` 命令不能审批，只能查看阻断原因。

## Activity 页面

用于审计 AI 执行历史。

示例：

```text
TIME      STATUS     RISK      HOST    SOURCE       COMMAND
14:21:01  DONE       LOW       prod1   claude-code  uptime
14:21:03  DONE       LOW       prod1   claude-code  docker ps
14:21:14  PENDING    HIGH      prod1   claude-code  systemctl restart nginx
14:21:20  BLOCKED    CRITICAL  prod1   opencode     rm -rf /
14:21:30  FAILED     MEDIUM    prod2   claude-code  cat /root/.env
```

详情展示：

```text
stdout
stderr
exit code
duration
source
workspace
approval decision
policy rule
connection reused
created_at
started_at
finished_at
```

stdout/stderr 默认折叠，避免刷屏。

RUNNING 命令详情中显示最近输出尾部：

```text
last stdout lines
last stderr lines
```

完整输出仍由 `ssh-use exec` 流式返回给 AI agent。TUI 只显示最近窗口，避免大输出拖慢界面。

## Connections 页面

用于查看和管理 SSH 连接池。

示例：

```text
HOST    STATUS      USER    ADDR          IDLE     LAST COMMAND
prod1   CONNECTED   root    10.0.0.11     1m12s    systemctl status nginx
prod2   CONNECTED   root    10.0.0.12     8s       docker ps
prod3   CLOSED      root    10.0.0.13     -        uptime
```

操作：

```text
d       disconnect selected
D       disconnect all idle
enter   connection detail
```

断开保护：

```text
存在 RUNNING session 的连接不能直接断开。
用户按 d 时，TUI 必须展示 active command id 和 command 摘要。
只有先 cancel running command，或命令结束后，才能普通 disconnect。
```

强制关闭可以作为未来增强，不放入 MVP，避免误中断远端任务。

连接详情：

```text
Host: prod1
Addr: 10.0.0.11:22
User: root
Connected since: 14:10:03
Last used: 14:21:30
Open sessions: 0
Idle timeout: 10m
```

## Policy 页面

展示当前模式、规则和命中统计。

示例：

```text
Mode: Sensitive

RULE                         ACTION    RISK      MATCHES
destructive_block            block     critical  1
service_mutation             approve   high      4
docker_mutation              approve   high      2
readonly_inspection          allow     low       31
```

选中规则后显示：

```text
service_mutation

Action: approve
Risk: high

Patterns:
  \bsystemctl\s+(restart|stop|start|reload)\b
  \bdocker\s+(restart|stop|rm|rmi)\b
```

第一版只展示规则，不在 TUI 中编辑规则。

## Settings 页面

用于运行时设置。

示例：

```text
Execution Mode:
  [ ] Auto
  [x] Sensitive
  [ ] Approval

Audit:
  [x] Store commands
  Output retention: summary
  [x] Redact secrets

Approval:
  Timeout: 30m

TUI:
  [x] Focus pending command
  [ ] Bell on pending
```

切换到 Auto 需要确认：

```text
Switch to Auto mode?

Sensitive commands will run without approval.
Blocked commands will still be blocked.

Enter confirm    Esc cancel
```

## 颜色设计

颜色用于快速识别重点，但不能只依赖颜色，必须同时显示文本状态和风险等级。

风险颜色：

```text
LOW       green / dim green
MEDIUM    yellow
HIGH      orange / bright yellow
CRITICAL  red text + dark red background
```

状态颜色：

```text
PENDING   yellow
RUNNING   cyan / blue
DONE      green
FAILED    red
BLOCKED   red background
REJECTED  gray + red accent
TIMEOUT   magenta
QUEUED    dim gray
```

模式颜色：

```text
Auto       green
Sensitive  yellow
Approval   orange/red
```

连接颜色：

```text
CONNECTED  green
IDLE       dim yellow
CLOSED     gray
ERROR      red
```

视觉优先级：

```text
1. BLOCKED / CRITICAL
2. PENDING / HIGH
3. FAILED / TIMEOUT
4. RUNNING
5. DONE
```

设计规则：

```text
Pending 数字用黄色，>0 时加粗。
Critical 行使用红色背景，不只用红字。
当前选中行使用反色或边框，不只靠颜色。
低风险命令降低亮度，让高风险更突出。
命令文本保持默认色，风险和状态用颜色。
stderr 用红色前缀或红色边框，stdout 用默认色。
成功不要大面积绿色，避免界面噪音。
```

主题：

```text
dark
light
mono
```

`mono` 主题用于颜色不可用或色盲友好场景：

```text
[!] CRITICAL
[?] PENDING
[>] RUNNING
[OK] DONE
[x] FAILED
[-] REJECTED
```

## 事件和通信协议

CLI、TUI 和 daemon 使用 Unix socket 通信。

Socket 路径：

```text
$XDG_RUNTIME_DIR/ssh-use/ssh-use.sock
```

协议使用 JSON lines。

`exec` 请求：

```json
{
  "type": "exec",
  "host": "prod1",
  "command": "systemctl restart nginx",
  "source": "claude-code",
  "cwd": "/root/codes/app"
}
```

执行期间，daemon 通过同一个连接向 `ssh-use exec` 发送流式输出事件。

stdout chunk：

```json
{
  "type": "stdout_chunk",
  "id": "cmd_01JZABC",
  "data": "..."
}
```

stderr chunk：

```json
{
  "type": "stderr_chunk",
  "id": "cmd_01JZABC",
  "data": "..."
}
```

`ssh-use exec` 收到 chunk 后立即写入本地 stdout/stderr。

最终响应：

```json
{
  "ok": true,
  "id": "cmd_01JZABC",
  "status": "done",
  "remote_exit_code": 0,
  "ssh_use_error_code": ""
}
```

TUI 订阅事件：

```json
{
  "type": "subscribe_events"
}
```

daemon 推送事件：

```json
{"type":"command.created","id":"cmd_01JZABC"}
{"type":"command.pending","id":"cmd_01JZABC"}
{"type":"command.queued","id":"cmd_01JZABC"}
{"type":"command.running","id":"cmd_01JZABC"}
{"type":"command.stdout","id":"cmd_01JZABC"}
{"type":"command.stderr","id":"cmd_01JZABC"}
{"type":"command.done","id":"cmd_01JZABC"}
{"type":"command.blocked","id":"cmd_01JZABC"}
{"type":"command.cancelled","id":"cmd_01JZABC"}
{"type":"connection.updated","host":"prod1"}
{"type":"policy.mode_changed","mode":"sensitive"}
{"type":"daemon.paused","paused":true}
```

TUI 审批请求：

```json
{
  "type": "approval.decide",
  "id": "cmd_01JZABC",
  "decision": "approve"
}
```

TUI 拒绝请求：

```json
{
  "type": "approval.decide",
  "id": "cmd_01JZABC",
  "decision": "reject"
}
```

TUI 取消命令请求：

```json
{
  "type": "command.cancel",
  "id": "cmd_01JZABC"
}
```

TUI 暂停或恢复请求：

```json
{
  "type": "daemon.pause",
  "paused": true
}
```

所有会改变状态的请求都必须返回 ack：

```json
{
  "ok": true,
  "type": "ack",
  "request_id": "req_01JZ..."
}
```

## 审计存储

使用 SQLite。

路径：

```text
~/.local/share/ssh-use/ssh-use.db
```

命令表字段：

```text
id
created_at
started_at
finished_at
source
client_pid
client_cwd
host
remote_user
command
display_command
mode
risk
policy_action
policy_rule
status
remote_exit_code
ssh_use_error_code
duration_ms
stdout
stderr
stdout_truncated
stderr_truncated
error
approval_status
approved_at
```

连接状态可以只保存在 daemon 内存中，不必落库。

配置：

```yaml
audit:
  enabled: true
  store_output: summary
  max_output_bytes: 262144
  retention_days: 14
  redact_secrets: true
```

输出保存策略：

```text
none:
只保存命令 metadata，不保存 stdout/stderr。

summary:
默认值。保存脱敏后的前后若干 KB 和输出大小、行数、截断标记。

redacted_full:
保存脱敏后的完整输出，但仍受 max_output_bytes 限制。
```

默认使用 `summary`，避免 AI 读取 `.env`、token、数据库连接串或日志中的敏感数据后被完整持久化。

输出过长时必须截断，并记录：

```text
stdout_truncated
stderr_truncated
stdout_original_bytes
stderr_original_bytes
```

命令和输出需要脱敏。

基础脱敏规则：

```text
password=...
token=...
secret=...
api_key=...
Authorization: Bearer ...
AWS_SECRET_ACCESS_KEY=...
```

脱敏后展示：

```text
password=[REDACTED]
token=[REDACTED]
Authorization: Bearer [REDACTED]
```

数据库中默认不保存 raw output。若未来支持 raw output，必须显式配置开启，并在 TUI 中显示风险提示。

## SSH 连接池

连接池 key：

```go
type ConnKey struct {
    Host string
    User string
    Addr string
    Port int
    Key  string
}
```

连接结构：

```go
type SSHConn struct {
    Client      *ssh.Client
    LastUsed    time.Time
    IdleTimeout time.Duration
    Mu          sync.Mutex
}
```

执行模型：

```text
1. 根据 host 配置获取连接 key
2. 连接存在且可用则复用
3. 连接不存在或已断开则重连
4. 每条命令创建新 ssh.Session
5. 同一 host 第一版默认串行执行
6. 执行结束更新 LastUsed
7. 后台定期清理空闲连接
```

运行中命令取消：

```text
daemon 为每个 RUNNING 命令保存对应 ssh.Session。
收到 cancel 请求时关闭 session。
session 关闭失败或远端进程未退出时，标记为 CANCEL_FAILED，并在 TUI 中提示用户。
```

注意：SSH exec 没有可靠的跨 shell 进程树 kill 语义。第一版以关闭 session 为主，不承诺能杀死远端命令创建的所有后台子进程。

超时：

```yaml
defaults:
  connect_timeout: 10s
  command_timeout: 5m
  idle_timeout: 10m
```

后台清理：

```text
每 30s 扫描连接池。
超过 idle_timeout 的连接自动关闭。
下次命令自动重连。
```

## 配置文件

路径：

```text
~/.config/ssh-use/config.yaml
```

示例：

```yaml
defaults:
  user: root
  port: 22
  key: ~/.ssh/id_ed25519_ssh_use
  idle_timeout: 10m
  connect_timeout: 10s
  command_timeout: 5m

hosts:
  prod1:
    addr: 10.0.0.11
    user: root
    key: ~/.ssh/id_ed25519_ssh_use

  prod2:
    addr: 10.0.0.12
    user: ubuntu
    key: ~/.ssh/id_ed25519_ssh_use

policy:
  mode: sensitive
  default_action: allow
  builtin_rules: true

audit:
  enabled: true
  store_output: summary
  max_output_bytes: 262144
  retention_days: 14
  redact_secrets: true

approval:
  timeout: 30m
  confirm_risks: [high, critical]

tui:
  theme: dark
  focus_pending: true
  bell_on_pending: false
```

## 命令来源识别

支持通过环境变量识别来源：

```bash
SSH_USE_SOURCE=claude-code ssh-use exec prod1 -- "uptime"
SSH_USE_SOURCE=opencode ssh-use exec prod1 -- "uptime"
```

也可以由 CLI 自动采集：

```text
client_pid
client_cwd
local_user
parent_process
```

TUI 中展示：

```text
Source: claude-code
Workspace: /root/codes/app
```

## 安全设计

默认安全策略：

```text
不监听 TCP。
只使用 Unix socket。
socket 权限 0600。
SQLite 文件权限 0600。
默认开启 host key 校验。
默认开启审计。
默认开启脱敏。
默认 sensitive 模式。
block 规则在 auto 模式下仍生效。
default_action 默认为 allow，只审批命中敏感规则的命令。
审计输出默认只保存 summary，不保存 raw output。
```

SSH key 建议：

```text
为 ssh-use 创建专用 key。
不要复用个人主 key。
按服务器和用户限制权限。
生产环境不要关闭 host key 校验。
```

## Go 技术选型

核心依赖：

```text
golang.org/x/crypto/ssh
github.com/charmbracelet/bubbletea
github.com/charmbracelet/lipgloss
github.com/charmbracelet/bubbles
gopkg.in/yaml.v3
modernc.org/sqlite 或 github.com/mattn/go-sqlite3
```

可选依赖：

```text
github.com/oklog/ulid/v2
golang.org/x/crypto/ssh/knownhosts
```

TUI 推荐使用 Bubble Tea，因为事件驱动模型适合 daemon 事件流。

## 项目结构

推荐结构：

```text
 ssh-use/
  go.mod
  cmd/
    ssh-use/
      main.go
  internal/
     cli/
       exec.go
       cp.go
       tui.go
    daemon/
      server.go
      lifecycle.go
      protocol.go
      events.go
    sshpool/
      pool.go
      exec.go
      client.go
    policy/
      engine.go
      rules.go
      builtin.go
    approval/
      manager.go
    audit/
      store.go
      models.go
      migrations.go
    config/
      config.go
    redact/
      redact.go
    tui/
      app.go
      model.go
      update.go
      view.go
      keys.go
      page_review.go
      page_activity.go
      page_connections.go
      page_policy.go
      page_settings.go
```

## MVP 范围

第一版必须实现：

```text
ssh-use exec
ssh-use cp
ssh-use tui
ssh-use tui --safe
daemon 自动启动
Unix socket JSON lines 协议
SSH client 连接池
每条命令新建 ssh.Session
stdout/stderr 流式返回
单文件 SFTP 上传下载
stdin/stdout 文件传输
可选原子替换
审批等待心跳
命令队列状态展示
TUI 内 cancel queued/running command
TUI 内 pause/resume new commands
TUI 内 emergency stop
空闲连接自动关闭
YAML 配置
SQLite 审计
策略引擎
三种模式 auto / sensitive / approval
default_action 默认为 allow，只审批敏感命令
TUI Review 页面
TUI Activity 页面
TUI Connections 页面
TUI Policy 页面
TUI Settings 页面
TUI 内 approve once / reject
TUI 颜色语义
stdout/stderr 折叠查看
基础脱敏
审计输出默认 summary 保存
```

第一版不做：

```text
Web UI
命令行审批
批量审批
临时信任
规则编辑
ssh-agent 必需依赖
交互式 shell
PTY
复杂跳板机
远程 daemon
多用户权限系统
```

## 后续增强

可以逐步增加：

```text
trust similar for 10 min
bulk approve
create rule from selected command
advanced filters
export audit log
desktop notification
ssh-agent optional auth
proxy_jump
host tags
multi-host exec
append-only audit hash chain
```

## 推荐实现顺序

```text
1. 配置加载
2. Unix socket daemon
3. ssh-use exec 请求和响应
4. SSH 连接池
5. 单 host 命令执行
6. stdout/stderr 流式返回
7. SQLite 审计
8. 脱敏和输出截断
9. 策略引擎
10. approval manager
11. 审批等待心跳和队列状态
12. TUI 基础框架
13. Review 页面和审批动作
14. Activity 页面
15. cancel / pause / emergency stop
16. Connections 页面和断开保护
17. Policy 和 Settings 页面
18. 颜色主题和 mono 主题
19. 超时、断线重连、空闲回收
```

## 最终体验

用户打开 TUI：

```bash
ssh-use tui
```

AI agent 执行：

```bash
ssh-use exec prod1 -- "docker ps"
ssh-use exec prod1 -- "systemctl restart nginx"
```

TUI 实时展示：

```text
docker ps                  LOW   allowed        DONE
systemctl restart nginx    HIGH  waiting        PENDING
```

用户在 TUI 中按 `a`，确认后命令继续执行，AI agent 收到远程执行结果。

这套设计让 AI agent 只负责发命令，用户在一个持续运行的终端界面中集中观察、审批和控制远程命令执行。
