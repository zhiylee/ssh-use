package protocol

import (
	"encoding/base64"
	"encoding/json"
	"io"

	"agent-ssh/internal/model"
)

type Encoder = json.Encoder
type Decoder = json.Decoder

type Message struct {
	Type              string                   `json:"type"`
	RequestID         string                   `json:"request_id,omitempty"`
	OK                bool                     `json:"ok,omitempty"`
	ID                string                   `json:"id,omitempty"`
	Host              string                   `json:"host,omitempty"`
	Command           string                   `json:"command,omitempty"`
	Source            string                   `json:"source,omitempty"`
	CWD               string                   `json:"cwd,omitempty"`
	ClientPID         int                      `json:"client_pid,omitempty"`
	Data              string                   `json:"data,omitempty"`
	Encoding          string                   `json:"encoding,omitempty"`
	Seq               int64                    `json:"seq,omitempty"`
	Status            string                   `json:"status,omitempty"`
	RemoteExitCode    *int                     `json:"remote_exit_code,omitempty"`
	AgentSSHErrorCode string                   `json:"agent_ssh_error_code,omitempty"`
	Error             string                   `json:"error,omitempty"`
	Decision          string                   `json:"decision,omitempty"`
	Paused            bool                     `json:"paused,omitempty"`
	Mode              string                   `json:"mode,omitempty"`
	Direction         string                   `json:"direction,omitempty"`
	LocalPath         string                   `json:"local_path,omitempty"`
	RemotePath        string                   `json:"remote_path,omitempty"`
	Size              int64                    `json:"size,omitempty"`
	Bytes             int64                    `json:"bytes,omitempty"`
	FileMode          uint32                   `json:"file_mode,omitempty"`
	PreserveMode      bool                     `json:"preserve_mode,omitempty"`
	Atomic            bool                     `json:"atomic,omitempty"`
	Checksum          string                   `json:"checksum,omitempty"`
	TUIConnected      bool                     `json:"tui_connected,omitempty"`
	Elapsed           string                   `json:"elapsed,omitempty"`
	Position          int                      `json:"position,omitempty"`
	Record            *model.CommandRecord     `json:"record,omitempty"`
	Commands          []model.CommandRecord    `json:"commands,omitempty"`
	Connections       []model.ConnectionStatus `json:"connections,omitempty"`
	PolicyRules       []model.PolicyRuleView   `json:"policy_rules,omitempty"`
	Revision          uint64                   `json:"revision,omitempty"`
	PolicyDecision    *model.PolicyDecision    `json:"policy_decision,omitempty"`
	RuntimeSettings   *RuntimeSettings         `json:"runtime_settings,omitempty"`
}

type RuntimeSettings struct {
	Generation          uint64   `json:"generation"`
	Mode                string   `json:"mode"`
	PolicyDefaultAction string   `json:"policy_default_action"`
	BuiltinRules        bool     `json:"builtin_rules"`
	AuditStoreOutput    string   `json:"audit_store_output"`
	AuditRetentionDays  int      `json:"audit_retention_days"`
	RedactSecrets       bool     `json:"redact_secrets"`
	ConfirmRisks        []string `json:"confirm_risks"`
	Theme               string   `json:"theme"`
	FocusPending        bool     `json:"focus_pending"`
	BellOnPending       bool     `json:"bell_on_pending"`
}

func NewEncoder(w io.Writer) *json.Encoder {
	return json.NewEncoder(w)
}

func NewDecoder(r io.Reader) *json.Decoder {
	return json.NewDecoder(r)
}

func Chunk(kind, id string, seq int64, data []byte) Message {
	return Message{
		Type:     kind,
		ID:       id,
		Seq:      seq,
		Encoding: "base64",
		Data:     base64.StdEncoding.EncodeToString(data),
	}
}

func DecodeData(msg Message) ([]byte, error) {
	if msg.Encoding == "base64" {
		return base64.StdEncoding.DecodeString(msg.Data)
	}
	return []byte(msg.Data), nil
}
