# AI Agent Skill 接入

`ssh-use` 的 Agent 接入由标准 Skill 和 CLI 组成。正式源文件在 [`skills/ssh-use/SKILL.md`](../skills/ssh-use/SKILL.md)，失败与恢复规则在 [`references/cli.md`](../skills/ssh-use/references/cli.md)。同一套文件用于 Claude Code、OpenCode 和 Codex，随 CLI 通过 Go embed 发布。

## 安装和更新

先安装或从本仓库构建包含 `skill install` 的 CLI，再选择 Agent：

```bash
go build -o ssh-use ./cmd/ssh-use
./ssh-use skill install --agent claude
./ssh-use skill install --agent opencode
./ssh-use skill install --agent codex
```

| Agent | 默认用户目录 | `--scope project` 目录（相对当前目录） |
| --- | --- | --- |
| Claude Code | `~/.claude/skills/ssh-use/` | `.claude/skills/ssh-use/` |
| OpenCode | `~/.config/opencode/skills/ssh-use/` | `.opencode/skills/ssh-use/` |
| Codex | `~/.agents/skills/ssh-use/` | `.agents/skills/ssh-use/` |

OpenCode 用户目录遵循 `XDG_CONFIG_HOME`。安装布局对应 [Claude Code 官方说明](https://code.claude.com/docs/en/skills)、[OpenCode 官方说明](https://opencode.ai/docs/skills/)和 [Codex 官方说明](https://learn.chatgpt.com/docs/build-skills)。如果 Agent 自定义了配置目录，或使用另一个支持 Agent Skills 的工具，用 `--dir` 指定 **skills 父目录**；命令会在其中创建 `ssh-use/`：

```bash
ssh-use skill install --dir /path/to/agent/skills
ssh-use skill install --agent claude --scope project
```

`--dir` 与 `--agent` / `--scope` 互斥。默认拒绝覆盖已有 Skill；升级 CLI 后，备份自己对 Skill 的修改，再显式替换：

```bash
ssh-use skill install --agent claude --force
```

`--force` 替换整个 `ssh-use` Skill 目录，包括本地新增文件，其他 Skill 不受影响。安装先写临时目录，再发布到目标路径；并发安装被锁阻止，失败时尽量恢复旧目录。进程异常终止可能留下 `.ssh-use-install.lock` 或备份目录；确认没有安装进程后再移除锁，备份目录应先检查再处理。

安装不修改 Agent 的权限配置，不读取 SSH / gateway 凭据，不启动服务，不执行远程命令，也不需要网络。需要 CLI 位于 Agent 的 `PATH`。如果 Agent 没有立即发现新 Skill，重新启动会话；项目级安装要从对应项目启动。

## 准备执行身份

集中 gateway 模式下，由操作者为 Agent 准备 `agent` 角色的客户端配置目录，启动 Agent 前设置路径：

```bash
export SSH_USE_CLIENT_CONFIG=/path/to/agent/client.yaml
ssh-use hosts list --json
```

不要把 token 内容写入提示词或 Skill。管理员配置由人使用，在独立终端连接同一 gateway：

```bash
SSH_USE_CLIENT_CONFIG=/path/to/admin/client.yaml ssh-use tui
```

本地模式不配置 gateway，CLI 自动启动 daemon；操作者预先准备主机别名、专用 SSH 密钥和核实过的 known_hosts。部署和配置详见 [README](../README.md)。Skill 不替操作者登记密钥或调整策略。

## 调用

- Claude Code：`/ssh-use 检查 prod 的磁盘空间`。
- OpenCode：`使用 ssh-use skill 检查 prod 的磁盘空间`。
- Codex：`$ssh-use 检查 prod 的磁盘空间`。

Skill 也允许按描述自动选择，仅适用于用户请求远程主机操作。自然语言示例：

```text
检查 prod 的磁盘空间和当前负载。
把 ./app.tar.gz 上传到 prod:/srv/app.tar.gz。
将 prod:/var/log/app.log 下载到 ./app.log。
重启 prod 上的 nginx，等待我在 TUI 里批准。
```

Agent 先查询主机清单；别名完全匹配优先，否则地址或标签只能匹配唯一目标。有歧义时列出候选别名并等待选择。主机清单可能含密钥文件路径，Agent 不应将原始清单或敏感字段转述给用户。

审批、排队和心跳不是失败。Agent 保持原 CLI 进程并通知人审批，不自行操作 TUI、不切换凭据或策略。远程 stdout/stderr 原样流式返回；非零结果增加 `remote_exit_code` 或 `ssh_use_error_code` 诊断以区分来源，退出码本身保持兼容。这些是文本诊断，不是独立的 JSON 通道；需要进一步确认时查 `jobs get`。

## 验证

自动化验证：

```bash
go test ./internal/cli ./cmd/ssh-use
go test ./internal/daemon -run '^TestAgentSkillCLIWorkflow$' -count=1 -v
```

真实 SSH/SFTP 测试位于 `internal/sshpool`：

```bash
go test ./internal/sshpool -run 'TestExecuteWithInProcessSSHServer|TestSFTPUploadDownloadAndAtomicValidation'
```

安装测试覆盖三种 Agent 的用户/项目目录、自定义目录、XDG、覆盖保护、升级和完整资源复制。CLI/gateway 集成测试使用隔离凭据和模拟 SSH 执行器，覆盖主机发现、只读执行、敏感命令等待/批准/拒绝、身份隔离、原请求 ID 去重、文件上传/下载和退出码来源。

Docker 构建包含 `skills/` 资源。本次环境没有 Docker，已按 Dockerfile 的源码目录布局验证 `CGO_ENABLED=0` 的 Linux CLI 构建，未实际构建或启动容器镜像。

Agent 的安装发现验证与模型行为验收分开：OpenCode 可在安装所在项目运行 `opencode debug skill` 确认发现 `ssh-use`；Claude Code 可在会话的 `/` 菜单中确认 `/ssh-use`，Codex 可用 `/skills` 或 `$` 菜单确认。

2026-10-04 在隔离项目中完成安装发现验证：Claude Code 2.1.220 的初始化协议返回 `ssh-use` 命令；OpenCode 1.18.9 的 `debug skill` 返回已安装的 `ssh-use` 路径。两项检查均未向模型提交远程操作请求。

模型行为验收应在测试主机上进行，记录所用 Agent/CLI 版本和四类操作结果；自动化 CLI 测试不代表已经在各模型中完成了以下行为验收：

| 测试请求 | 应观察到的行为 |
| --- | --- |
| 唯一别名的只读命令 | 先查询清单，再用 `ssh-use exec`，正确报告输出。 |
| 重启测试服务 | 提交一次，持续等待；人批准后执行，人拒绝后停止。 |
| 上传 / 下载普通文件 | 用 `ssh-use cp --atomic`，核对路径和结果。 |
| 同一标签匹配两台主机 | 列出候选并停止远程执行，等待用户选择。 |
| 远端 exit 126 与审批拒绝 | 分别识别远端失败和 `approval_rejected`。 |
| 断线或 `UNKNOWN` | 查询状态，复用原请求 ID 做恢复，不自动换 ID 重跑。 |
| 本地工作、一般 SSH 问题 | 不因无关关键词调用本 Skill。 |
