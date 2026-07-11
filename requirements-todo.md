# agent-ssh 需求代办列表

更新时间：2026-07-11

本文档将竞品调研结论转化为可执行需求。现有产品定义和架构约束以 [agent-ssh-design.md](./agent-ssh-design.md) 为准。

## 状态约定

- `[x]`：已完成并验证。
- `[ ]`：尚未开始。
- `P0`：下一阶段必须完成，直接影响 Agent 可用性或安全性。
- `P1`：重要增强，在 P0 稳定后实施。
- `P2`：可选增强，需要先验证真实使用需求。
- `DECISION`：开始实现前必须确认的产品或接口决策。

## 当前基线

- [x] 产品、CLI、Go module、配置路径和运行时标识统一为 `agent-ssh`。
- [x] 提供 `agent-ssh exec`、`agent-ssh cp`、`agent-ssh tui` 和 `agent-ssh tui --safe`。
- [x] daemon 自动启动并通过 Unix socket 提供 JSON lines 协议。
- [x] SSH client 连接池、断线重连和空闲连接回收。
- [x] stdout/stderr 流式返回并保留远端 exit code。
- [x] 单文件 SFTP 上传、下载、stdin/stdout 和可选原子替换。
- [x] `auto`、`sensitive`、`approval` 三种执行模式。
- [x] 策略识别、TUI 审批、取消、暂停和 emergency stop。
- [x] SQLite 审计、输出截断和基础秘密脱敏。
- [x] 默认启用 `known_hosts` 主机密钥校验。
- [x] 完整 Go 测试、`go vet` 和构建通过。

## P0：Agent Skill

### [ ] P0-01 提供正式 Agent Skill

需求：

- 提供 Claude Code 和 OpenCode 可安装的 Skill。
- 明确触发条件：仅在用户要求操作远程主机时使用。
- 所有远程执行和文件传输必须通过 `agent-ssh`，不得回退到裸 `ssh`、`scp` 或绕过 daemon。
- Skill 必须先解析目标别名；存在歧义时停止并列出候选目标。
- Skill 不读取、输出或持久化私钥、密码、OTP 和完整敏感配置。
- Skill 中说明审批等待、超时、取消和非零退出码的处理方式。

验收标准：

- 在 Claude Code 和 OpenCode 中各完成一次安装验证。
- 能完成只读命令、需审批命令、上传和下载四类端到端场景。
- 敏感命令仍进入现有策略和 TUI 审批流程。
- Agent 收到可区分的远端失败与 `agent-ssh` 自身错误。

参考：`Eriemon/remote-ssh`、`veithly/vibeshell`。

### [ ] P0-02 提供目标发现和配置诊断能力

需求：

- 提供机器可读的主机清单，包含别名、标签和配置有效状态。
- 默认输出不得包含私钥内容、密码、完整密钥路径或其他秘密。
- 配置诊断覆盖 YAML schema、重复别名、端口范围、密钥存在性与权限、`known_hosts` 状态。
- 网络连通性检查必须显式启用，普通清单查询不得建立 SSH 连接。
- 提供稳定 JSON schema 和稳定错误码供 Skill 消费。

验收标准：

- 无效 YAML、缺失密钥、权限过宽、未知 host key 均有明确诊断。
- JSON 输出可由自动化测试解析，不依赖面向人的提示文本。
- 诊断过程不会执行任何远端命令。

依赖：`DECISION-01`。

参考：`Eriemon/remote-ssh`、`classfang/ssh-mcp-server`、`veithly/vibeshell`。

### [ ] P0-03 完善首次主机密钥登记流程

需求：

- 未知 host key 默认阻断连接，不允许自动接受。
- TUI 展示主机别名、地址、算法和 SHA256 指纹。
- 用户确认后原子写入专用或系统 `known_hosts` 文件。
- host key 发生变化时始终阻断，不提供 Agent 自动确认入口。
- 审计记录登记、拒绝和 host key 变化事件，但不记录秘密。

验收标准：

- 新主机首次连接必须经 TUI 人工确认。
- 拒绝后远端命令不执行，CLI 返回稳定错误码。
- 模拟中间人密钥变化时无法通过普通审批继续执行。
- 并发登记不会损坏 `known_hosts` 文件。

参考：`Eriemon/remote-ssh` 的显式确认流程；避免竞品中的自动接受行为。

### [ ] P0-04 支持配置热加载和连接池精确失效

需求：

- daemon 能检测配置变化或接收显式 reload 请求。
- 新配置必须先完整解析和校验，失败时继续使用上一份有效配置。
- 根据地址、用户、端口、认证方式、密钥指纹和跳板配置生成连接指纹。
- 只关闭配置指纹发生变化的连接，不影响无关主机和运行中命令。
- 策略变更对新请求立即生效。

验收标准：

- 修改主机密钥、用户或地址后不会复用旧 SSH client。
- 无效配置不会导致 daemon 退出或清空可用连接池。
- reload 结果可在 TUI 中查看并写入审计事件。
- 并发 reload 和命令执行通过 race test。

参考：`sleepinginsummer/agent-ssh-cli` 的配置/认证指纹连接池。

### [ ] P0-05 增加远程路径安全边界

需求：

- 每个主机可配置允许访问的远程根目录 `allowed_paths`。
- 上传、下载和未来文件编辑操作均校验 canonical path。
- 必须解析目标父目录和符号链接，阻止通过 `..` 或 symlink 越界。
- 未配置 `allowed_paths` 时保持当前显式绝对路径行为，Skill 必须额外限制工作目录。
- 审计记录请求路径和最终 canonical path。

验收标准：

- 覆盖 `..`、绝对 symlink、相对 symlink、目标不存在和竞态替换测试。
- 越界请求在写入或读取任何文件内容前失败。
- 拒绝结果具有稳定错误码且可由策略和 TUI 识别。

参考：`Eriemon/remote-ssh` 的 realpath/symlink containment。

### [ ] P0-06 固化 Agent 可依赖的结构化契约

需求：

- `exec` 默认继续保持本地命令语义：stdout、stderr 和 exit code 不封装。
- 清单、诊断和状态类只读能力支持 `--json`。
- 协议版本、事件类型和错误码形成公开兼容契约。
- 明确区分远端非零退出、策略阻断、审批拒绝、审批超时、连接错误和本地协议错误。
- 未知 JSON 字段应向前兼容；破坏性协议变化必须升级版本。

验收标准：

- 为每类错误提供 CLI 和协议契约测试。
- stdout 中的任意 JSON 文本不会被误当成控制消息。
- Skill 不需要解析自然语言错误提示。

参考：`trtyr/AgentSSH`、`classfang/ssh-mcp-server`。

## P1：可靠性与工作流

### [ ] P1-01 支持可恢复文件传输

需求：

- 上传和下载使用临时文件、偏移量和 SHA256 校验恢复中断传输。
- 恢复前校验源文件大小、mtime 和摘要，源变化时重新开始。
- 成功后使用原子 rename 提交；失败时不得覆盖已有目标。
- 只重试可证明安全的传输分片，不自动重试远端命令。
- 提供过期临时文件清理策略。

验收标准：

- 在 25%、50%、90% 位置中断后均可继续传输。
- 最终文件摘要与源文件一致。
- daemon 重启后仍可识别合法的未完成传输。
- 篡改临时文件或 metadata 时拒绝恢复。

参考：`sleepinginsummer/agent-ssh-cli`。

### [ ] P1-02 提供安全精确编辑工作流

需求：

- 编辑前读取并记录远端文件摘要、权限和 owner 信息。
- 替换条件必须唯一匹配；零匹配或多匹配均停止。
- 提交时使用旧摘要作为前置条件，检测并发修改。
- 使用同目录临时文件和原子 rename，尽量保留权限和 owner。
- 审计只保存摘要和差异摘要，不默认保存完整文件内容。

验收标准：

- 并发修改时不会覆盖他人变更。
- 原子替换失败时原文件保持完整。
- Skill 可以完成“读取、精确修改、校验、提交”闭环。

依赖：`DECISION-02`、`P0-05`。

参考：`Eriemon/remote-ssh` 的受限 workspace 和审核工件。

### [ ] P1-03 支持可选认证能力

需求：

- 保持专用私钥文件为默认认证方式。
- 可选支持 `ssh-agent`，但不得成为运行必需组件。
- 支持加密私钥；口令只能通过 TUI 人工输入并保存在进程内存中。
- Agent、CLI 参数、环境变量、日志和审计中不得出现口令。
- 明确密钥解锁生命周期和 daemon 重启后的行为。

验收标准：

- 覆盖未加密密钥、加密密钥和 `ssh-agent` 三类连接测试。
- TUI 关闭或超时后可清除缓存的解锁材料。
- 日志和数据库秘密扫描测试通过。

参考：`trtyr/AgentSSH`、`EliasOenal/term-cli`；避免明文凭据配置。

### [ ] P1-04 支持 ProxyJump

需求：

- 每个主机可声明单个跳板机，后续再评估多跳。
- 跳板机和目标机分别校验 host key。
- 连接指纹包含完整跳板配置。
- TUI 展示连接链路和具体失败节点。
- 跳板连接复用遵循相同的空闲回收和配置失效规则。

验收标准：

- 直连、单跳、跳板认证失败和目标认证失败均有集成测试。
- 修改跳板配置后旧连接不会被复用。
- 审计不记录私钥内容或认证材料。

参考：`sleepinginsummer/agent-ssh-cli`、`trtyr/AgentSSH`。

### [ ] P1-05 支持断线后的有限事件恢复

需求：

- daemon 为命令事件分配单调递增 cursor。
- TUI 重连时可从最后 cursor 恢复有限窗口内的事件。
- 每条命令输出缓冲必须有字节和时间上限。
- 超出窗口时返回明确的 gap 事件，不假装数据完整。
- 保持 SQLite 审计为最终历史来源。

验收标准：

- TUI 断线重连后不重复审批、不重复执行命令。
- 慢消费者不会导致 daemon 内存无限增长。
- cursor 过期场景有清晰提示和测试。

参考：`trtyr/AgentSSH` 的 cursor/bounded output 模型。

### [ ] P1-06 增加主机标签和受控批量操作

需求：

- 主机清单支持按 tag 筛选。
- 第一阶段只允许批量执行内置只读健康检查。
- 任意写操作不得因 tag 自动扩散到多个主机。
- 每个目标独立进行策略判断、审批、限流和审计。
- TUI 显示每个目标的独立状态和失败原因。

验收标准：

- 空 tag、未知 tag 和部分主机失败行为确定且可测试。
- 批量操作设置并发上限。
- 一个目标失败不会隐藏其他目标结果。

依赖：`DECISION-04`。

参考：`veithly/vibeshell`。

## P2：可选增强

### [ ] P2-01 后台任务和状态恢复

- 策略和审批必须在任务启动前完成。
- daemon 返回稳定 task ID，并在 TUI 中展示状态、有限日志和取消结果。
- 明确 daemon 重启后任务状态是恢复、失联还是失败。
- 不通过盲重试推断任务是否成功。

依赖：`DECISION-03`。

### [ ] P2-02 MFA、sudo 和交互提示的人类接管

- 默认 exec 路径保持非交互。
- 检测到交互提示时暂停并仅允许在 TUI 中接管。
- OTP、密码和口令不得进入 Agent 上下文、日志或审计。
- 接管超时后关闭 session 并返回明确错误。

参考：`EliasOenal/term-cli`。

### [ ] P2-03 审计增强

- 导出脱敏后的 JSONL 审计记录。
- 可选 append-only hash chain，用于检测历史篡改。
- 导出操作本身写入审计事件。
- 保持 raw output 默认关闭。

### [ ] P2-04 审批效率增强

- 从选中命令生成候选规则，但保存前必须人工编辑和确认。
- `trust similar` 必须有短 TTL、范围预览和随时撤销能力。
- 批量审批必须逐项展示目标、命令和风险，不提供一键无条件批准。

## 待确认设计决策

### [ ] DECISION-01 只读 CLI 接口

在以下方案中选择一个：

1. 新增 `agent-ssh hosts` 和 `agent-ssh doctor`。
2. 保持三个主要命令，通过内部只读子命令供 Skill 使用。
3. 扩展本地协议，由 Skill helper 直接读取只读信息。

推荐方案 1。它易测试、易发现，且不会提供 CLI 审批入口。

### [ ] DECISION-02 安全编辑接口

在以下方案中选择一个：

1. 新增专用 `edit`/`patch` 协议操作。
2. 由 Skill 基于 `cp --atomic`、摘要前置条件和临时文件组合。

推荐先实现方案 2；出现重复实现或竞态问题后再增加专用协议。

### [ ] DECISION-03 后台任务语义

确认是否允许远端命令脱离 CLI 生命周期，以及 daemon 重启后如何表达无法重新附着的远端进程。语义未确定前不实现 detached job。

### [ ] DECISION-04 多主机操作范围

确认多主机能力是否永久限制为只读健康检查。默认不允许 Agent 对 tag/group 执行批量写操作。

## 明确不做

- 不提供 Web UI 或桌面文件管理器。
- 不提供常驻交互式 shell、PTY 或远程终端模拟器。
- 不提供端口转发或 SOCKS5 代理。
- 不提供绕过 daemon、策略、审批和审计的 MCP 执行通道。
- 不自动接受未知或变化的 host key。
- 不在配置、CLI 参数或环境变量中接受明文密码和 OTP。
- 不对远端命令做盲重试。
- 不支持目录递归同步和远端 shell 通配符。
- 不将 `ssh-agent` 设为必需依赖。

## 参考项目

- [sleepinginsummer/agent-ssh-cli](https://github.com/sleepinginsummer/agent-ssh-cli)：daemon、连接池指纹、传输恢复、ProxyJump。
- [trtyr/AgentSSH](https://github.com/trtyr/AgentSSH)：结构化接口、session 和有限输出读取。
- [Eriemon/remote-ssh](https://github.com/Eriemon/remote-ssh)：Skill 触发、目标发现、配置诊断、路径边界和脱敏。
- [vbs666666/remote-server-ops](https://github.com/vbs666666/remote-server-ops)：反例，避免广泛配置扫描和明文凭据。
- [veithly/vibeshell](https://github.com/veithly/vibeshell)：主机标签、session inventory 和 Skill 安装体验。
- [EliasOenal/term-cli](https://github.com/EliasOenal/term-cli)：MFA、sudo 和人类接管。
- [classfang/ssh-mcp-server](https://github.com/classfang/ssh-mcp-server)：结构化主机接口；不采用绕过审批的直接 MCP 执行模式。
