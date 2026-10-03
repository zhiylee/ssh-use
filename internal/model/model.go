package model

import "time"

type Status string

const (
	StatusCreated         Status = "CREATED"
	StatusUnknown         Status = "UNKNOWN"
	StatusPaused          Status = "PAUSED"
	StatusPendingApproval Status = "PENDING_APPROVAL"
	StatusApproved        Status = "APPROVED"
	StatusRejected        Status = "REJECTED"
	StatusQueued          Status = "QUEUED"
	StatusRunning         Status = "RUNNING"
	StatusDone            Status = "DONE"
	StatusFailed          Status = "FAILED"
	StatusTimeout         Status = "TIMEOUT"
	StatusBlocked         Status = "BLOCKED"
	StatusCancelled       Status = "CANCELLED"
	StatusCancelFailed    Status = "CANCEL_FAILED"
)

func (s Status) Cancellable() bool {
	switch s {
	case StatusCreated, StatusPaused, StatusPendingApproval, StatusApproved, StatusQueued, StatusRunning:
		return true
	default:
		return false
	}
}

type Action string

const (
	ActionAllow   Action = "allow"
	ActionApprove Action = "approve"
	ActionBlock   Action = "block"
)

type Risk string

const (
	RiskLow      Risk = "low"
	RiskMedium   Risk = "medium"
	RiskHigh     Risk = "high"
	RiskCritical Risk = "critical"
)

type PolicyDecision struct {
	Action            Action `json:"action"`
	Risk              Risk   `json:"risk"`
	RuleName          string `json:"rule_name,omitempty"`
	MatchedPattern    string `json:"matched_pattern,omitempty"`
	Reason            string `json:"reason,omitempty"`
	DefaultActionUsed bool   `json:"default_action_used,omitempty"`
}

type CommandRecord struct {
	ID              string          `json:"id"`
	CreatedAt       time.Time       `json:"created_at"`
	StartedAt       time.Time       `json:"started_at,omitempty"`
	FinishedAt      time.Time       `json:"finished_at,omitempty"`
	Source          string          `json:"source,omitempty"`
	ClientPID       int             `json:"client_pid,omitempty"`
	ClientCWD       string          `json:"client_cwd,omitempty"`
	Host            string          `json:"host"`
	RemoteUser      string          `json:"remote_user,omitempty"`
	Command         string          `json:"command"`
	DisplayCommand  string          `json:"display_command"`
	Mode            string          `json:"mode"`
	Risk            Risk            `json:"risk"`
	PolicyAction    Action          `json:"policy_action"`
	PolicyRule      string          `json:"policy_rule,omitempty"`
	MatchedPattern  string          `json:"matched_pattern,omitempty"`
	PolicyReason    string          `json:"policy_reason,omitempty"`
	DefaultAction   bool            `json:"default_action,omitempty"`
	Status          Status          `json:"status"`
	RemoteExitCode  *int            `json:"remote_exit_code,omitempty"`
	SSHUseErrorCode string          `json:"ssh_use_error_code,omitempty"`
	DurationMS      int64           `json:"duration_ms,omitempty"`
	Stdout          string          `json:"stdout,omitempty"`
	Stderr          string          `json:"stderr,omitempty"`
	StdoutTruncated bool            `json:"stdout_truncated,omitempty"`
	StderrTruncated bool            `json:"stderr_truncated,omitempty"`
	Error           string          `json:"error,omitempty"`
	ApprovalStatus  string          `json:"approval_status,omitempty"`
	ApprovedAt      time.Time       `json:"approved_at,omitempty"`
	PolicyDecision  *PolicyDecision `json:"policy_decision,omitempty"`
}

type ConnectionStatus struct {
	Host         string    `json:"host"`
	Status       string    `json:"status"`
	User         string    `json:"user"`
	Addr         string    `json:"addr"`
	LastUsed     time.Time `json:"last_used,omitempty"`
	ConnectedAt  time.Time `json:"connected_at,omitempty"`
	OpenSessions int       `json:"open_sessions"`
	LastCommand  string    `json:"last_command,omitempty"`
}

type PolicyRuleView struct {
	Name     string   `json:"name"`
	Action   Action   `json:"action"`
	Risk     Risk     `json:"risk"`
	Patterns []string `json:"patterns"`
	Matches  int      `json:"matches"`
}
