package audit

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"

	"github.com/zhiylee/ssh-use/internal/model"
	"github.com/zhiylee/ssh-use/internal/paths"
)

type Store struct {
	db *sql.DB
}

func Open(enabled bool) (*Store, error) {
	if !enabled {
		return &Store{}, nil
	}
	if err := os.MkdirAll(paths.DataDir(), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", paths.DBPath())
	if err != nil {
		return nil, err
	}
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(paths.DBPath(), 0o600)
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS commands (
  id TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  started_at TEXT,
  finished_at TEXT,
  source TEXT,
  client_pid INTEGER,
  client_cwd TEXT,
  host TEXT NOT NULL,
  remote_user TEXT,
  command TEXT NOT NULL,
  display_command TEXT NOT NULL,
  mode TEXT,
  risk TEXT,
  policy_action TEXT,
  policy_rule TEXT,
  matched_pattern TEXT,
  policy_reason TEXT,
  default_action INTEGER,
  status TEXT NOT NULL,
  remote_exit_code INTEGER,
  ssh_use_error_code TEXT,
  duration_ms INTEGER,
  stdout TEXT,
  stderr TEXT,
  stdout_truncated INTEGER,
  stderr_truncated INTEGER,
  error TEXT,
  approval_status TEXT,
  approved_at TEXT
);
CREATE INDEX IF NOT EXISTS commands_created_at_idx ON commands(created_at DESC);
CREATE INDEX IF NOT EXISTS commands_status_idx ON commands(status);
`)
	if err != nil {
		return err
	}

	// Preserve audit history when opening a database from before the rename.
	var legacyColumns int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('commands') WHERE name = 'agent_ssh_error_code'`).Scan(&legacyColumns); err != nil {
		return err
	}
	if legacyColumns > 0 {
		_, err = s.db.ExecContext(ctx, `ALTER TABLE commands RENAME COLUMN agent_ssh_error_code TO ssh_use_error_code`)
	}
	return err
}

func (s *Store) Save(ctx context.Context, rec model.CommandRecord) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO commands (
  id, created_at, started_at, finished_at, source, client_pid, client_cwd,
  host, remote_user, command, display_command, mode, risk, policy_action,
  policy_rule, matched_pattern, policy_reason, default_action, status,
  remote_exit_code, ssh_use_error_code, duration_ms, stdout, stderr,
  stdout_truncated, stderr_truncated, error, approval_status, approved_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  started_at=excluded.started_at,
  finished_at=excluded.finished_at,
  source=excluded.source,
  client_pid=excluded.client_pid,
  client_cwd=excluded.client_cwd,
  host=excluded.host,
  remote_user=excluded.remote_user,
  command=excluded.command,
  display_command=excluded.display_command,
  mode=excluded.mode,
  risk=excluded.risk,
  policy_action=excluded.policy_action,
  policy_rule=excluded.policy_rule,
  matched_pattern=excluded.matched_pattern,
  policy_reason=excluded.policy_reason,
  default_action=excluded.default_action,
  status=excluded.status,
  remote_exit_code=excluded.remote_exit_code,
  ssh_use_error_code=excluded.ssh_use_error_code,
  duration_ms=excluded.duration_ms,
  stdout=excluded.stdout,
  stderr=excluded.stderr,
  stdout_truncated=excluded.stdout_truncated,
  stderr_truncated=excluded.stderr_truncated,
  error=excluded.error,
  approval_status=excluded.approval_status,
  approved_at=excluded.approved_at
`,
		rec.ID,
		formatTime(rec.CreatedAt),
		formatTime(rec.StartedAt),
		formatTime(rec.FinishedAt),
		rec.Source,
		rec.ClientPID,
		rec.ClientCWD,
		rec.Host,
		rec.RemoteUser,
		rec.Command,
		rec.DisplayCommand,
		rec.Mode,
		string(rec.Risk),
		string(rec.PolicyAction),
		rec.PolicyRule,
		rec.MatchedPattern,
		rec.PolicyReason,
		boolInt(rec.DefaultAction),
		string(rec.Status),
		nullInt(rec.RemoteExitCode),
		rec.SSHUseErrorCode,
		rec.DurationMS,
		rec.Stdout,
		rec.Stderr,
		boolInt(rec.StdoutTruncated),
		boolInt(rec.StderrTruncated),
		rec.Error,
		rec.ApprovalStatus,
		formatTime(rec.ApprovedAt),
	)
	return err
}

func (s *Store) Recent(ctx context.Context, limit int) ([]model.CommandRecord, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, created_at, started_at, finished_at, source, client_pid, client_cwd,
  host, remote_user, command, display_command, mode, risk, policy_action,
  policy_rule, matched_pattern, policy_reason, default_action, status,
  remote_exit_code, ssh_use_error_code, duration_ms, stdout, stderr,
  stdout_truncated, stderr_truncated, error, approval_status, approved_at
FROM commands
ORDER BY created_at DESC
LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []model.CommandRecord
	for rows.Next() {
		var rec model.CommandRecord
		var created, started, finished, approved string
		var risk, action, status string
		var defaultAction, stdoutTruncated, stderrTruncated int
		var remoteExit sql.NullInt64
		if err := rows.Scan(
			&rec.ID, &created, &started, &finished, &rec.Source, &rec.ClientPID, &rec.ClientCWD,
			&rec.Host, &rec.RemoteUser, &rec.Command, &rec.DisplayCommand, &rec.Mode, &risk, &action,
			&rec.PolicyRule, &rec.MatchedPattern, &rec.PolicyReason, &defaultAction, &status,
			&remoteExit, &rec.SSHUseErrorCode, &rec.DurationMS, &rec.Stdout, &rec.Stderr,
			&stdoutTruncated, &stderrTruncated, &rec.Error, &rec.ApprovalStatus, &approved,
		); err != nil {
			return nil, err
		}
		rec.CreatedAt = parseTime(created)
		rec.StartedAt = parseTime(started)
		rec.FinishedAt = parseTime(finished)
		rec.ApprovedAt = parseTime(approved)
		if remoteExit.Valid {
			value := int(remoteExit.Int64)
			rec.RemoteExitCode = &value
		}
		rec.Risk = model.Risk(risk)
		rec.PolicyAction = model.Action(action)
		rec.Status = model.Status(status)
		rec.DefaultAction = defaultAction != 0
		rec.StdoutTruncated = stdoutTruncated != 0
		rec.StderrTruncated = stderrTruncated != 0
		records = append(records, rec)
	}
	return records, rows.Err()
}

func (s *Store) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	if s == nil || s.db == nil || cutoff.IsZero() {
		return 0, nil
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM commands WHERE created_at < ?`, formatTime(cutoff))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return t
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func LimitOutput(output string, max int) (string, bool) {
	if max <= 0 || len(output) <= max {
		return output, false
	}
	if max < 128 {
		return output[:max], true
	}
	marker := fmt.Sprintf("\n...[ssh-use truncated %d bytes]...\n", len(output)-max)
	if len(marker) >= max {
		return output[:max], true
	}
	remaining := max - len(marker)
	head := remaining / 2
	tail := remaining - head
	return output[:head] + marker + output[len(output)-tail:], true
}
