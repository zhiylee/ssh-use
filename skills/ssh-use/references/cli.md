# CLI results and recovery

## Discovery and prerequisites

`ssh-use hosts list --json` returns a response with `type: "hosts"`, `ok: true`, `config_revision`, and `hosts` (an object keyed by alias; omitted when empty). A host has `addr`, `user`, `port`, `key`, and `tags`; empty user/key or port=0 inherits execution-service defaults. These are configured values, not resolved defaults or a connectivity/credential check. `hosts get <alias> --json` returns `host_config` for one alias. Both queries may start/connect to the local daemon or contact the gateway, but do not initiate target SSH connections. The raw output can include a private-key **path**; never expose or save that field.

No `doctor`, config-diagnostics, recursive-copy, interactive-shell/PTY, CLI-approval, or `jobs cancel` command is available. `jobs list` is administrator-only in gateway mode; an agent uses `jobs get <job-id>` for its own identity's jobs. Querying `jobs get` is read-only and does not attach to the live output stream.

CLI installation and gateway/SSH credential setup are operator prerequisites. In gateway mode the client needs an agent-role client configuration; SSH keys and known_hosts reside on the execution service. `SSH_USE_SOURCE` labels local audit only; gateway identity comes from the authenticated token.

## Results

`exec` writes remote stdout to stdout and remote stderr to stderr, alongside CLI status messages. A completed remote process preserves its exit code. On a nonzero remote exit the CLI adds `ssh-use: remote_exit_code=N job_id=...`. A service-reported failure adds `ssh-use: ssh_use_error_code=CODE job_id=...`. Treat these as diagnostics, not a structured JSON event stream; remote stderr can contain similar text. Confirm from `jobs get` when the origin is uncertain.

| Service error | CLI exit | Agent action |
| --- | --- | --- |
| `approval_rejected`, `policy_blocked` | 126 | Stop the operation and report the refusal. |
| `approval_timeout` | 124 | Approval expired; report it. Resubmission requires a new authorized attempt. |
| `command_timeout` | 124 | Remote effects may already exist; inspect before retrying. |
| `cancelled` | 130 | Check recorded state before claiming all remote work stopped. |
| `policy_error` | 125 | Operator must fix policy/configuration. |
| `connection_failed`, `ssh_failed` | 255 | Report the connection/execution error; do not switch transports. |
| Other service/transport errors | 1 | Read the diagnostic and any available job state. |
| Invalid CLI arguments | 2 | Correct syntax only; do not infer this is a remote exit. |

A remote program can also exit with 1, 2, 124, 125, 126, 130, or 255. Never classify solely by the number. Early CLI argument/config/transport failures may have no service error marker or job ID.

`ssh-use jobs get <job-id>` returns the command record directly as JSON. Relevant fields are `id`, `host`, `status`, `remote_exit_code`, `ssh_use_error_code`, `error`, `stdout`, `stderr`, `stdout_truncated`, and `stderr_truncated`. Saved output is redacted and bounded; use truncation flags and do not treat it as a full recovered transcript.

| Recorded status | Meaning |
| --- | --- |
| `CREATED`, `PAUSED`, `PENDING_APPROVAL`, `APPROVED`, `QUEUED`, `RUNNING` | Nonterminal; do not submit a duplicate. |
| `DONE` | Completed successfully. |
| `FAILED` | Inspect `remote_exit_code` versus `ssh_use_error_code`. |
| `REJECTED`, `BLOCKED` | Refused; do not evade or automatically resubmit. |
| `TIMEOUT`, `CANCELLED` | Recorded timeout/cancellation; remote effects may exist. |
| `UNKNOWN`, `CANCEL_FAILED` | Completion or cancellation is uncertain; verify before another mutation. |

## Recovery in gateway mode

Keep the original alias, command string, request ID, and gateway identity. A request ID is 1–128 characters; choose a unique value for each logical operation and do not reuse a tutorial's literal example ID.

1. A temporary output-stream disconnection reconnects automatically with a cursor. Continue waiting on the original CLI process while it does so.
2. If the CLI exits, retain the reported job ID and request ID. Query `jobs get <job-id>` when known. An accepted command continues during an ordinary network disconnect.
3. If acceptance is uncertain, repeat only the identical `exec --request-id <original-id> <original-alias> -- '<original-command>'` against the same gateway identity. The gateway returns the existing job rather than running it again. A different command under that ID is rejected. This is a recovery lookup, not authorization for a new mutation.
4. A duplicate request does not reattach the original live output; it can return the existing job with a nonzero CLI status while the job is pending or its saved result is unknown. Inspect `jobs get`, and do not assign a fresh ID just to make the CLI return success.
5. After service restart, unfinished tasks can become `UNKNOWN`; they are not resumed/re-executed. Verify the remote outcome and obtain a clear decision before a new attempt.

Deduplication is gateway-only and does not promise exactly-once remote effects. Local daemon mode does not deduplicate `--request-id`; after uncertain delivery, inspect the job and remote state instead of replaying the command. File transfers have no request-ID recovery or automatic resume. After a failed transfer, check source/destination state before a deliberate new transfer; `--atomic` reduces partial destination replacement but does not make network failures a resumable transfer.
