package protocol

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"io"
	"time"

	"github.com/zhiylee/ssh-use/internal/config"
	"github.com/zhiylee/ssh-use/internal/model"
)

const ChunkSize = 128 * 1024
const MaxRequestSize = 1024 * 1024
const MaxMessageSize = 8 * 1024 * 1024

// Connection is a logical message exchange, not necessarily a byte socket.
// Local connections are Unix sockets; remote connections are gRPC calls.
type Connection interface {
	io.Closer
	SetDeadline(time.Time) error
}

type messageSender interface{ Send(Message) error }
type messageReceiver interface{ Receive() (Message, error) }

type Encoder struct{ encode func(Message) error }

func (e *Encoder) Encode(v Message) error { return e.encode(v) }

type Decoder struct{ decode func(any) error }

func (d *Decoder) Decode(v any) error { return d.decode(v) }

type Message struct {
	Cursor          uint64                   `json:"cursor,omitempty"`
	Version         int                      `json:"version,omitempty"`
	Token           string                   `json:"token,omitempty"`
	ConfigRevision  string                   `json:"config_revision,omitempty"`
	HostConfig      *config.Host             `json:"host_config,omitempty"`
	HostPatch       *HostPatch               `json:"host_patch,omitempty"`
	Hosts           map[string]config.Host   `json:"hosts,omitempty"`
	Type            string                   `json:"type"`
	RequestID       string                   `json:"request_id,omitempty"`
	OK              bool                     `json:"ok,omitempty"`
	ID              string                   `json:"id,omitempty"`
	Host            string                   `json:"host,omitempty"`
	Command         string                   `json:"command,omitempty"`
	Source          string                   `json:"source,omitempty"`
	CWD             string                   `json:"cwd,omitempty"`
	ClientPID       int                      `json:"client_pid,omitempty"`
	Data            string                   `json:"data,omitempty"`
	Payload         []byte                   `json:"-"`
	Encoding        string                   `json:"encoding,omitempty"`
	Seq             int64                    `json:"seq,omitempty"`
	Status          string                   `json:"status,omitempty"`
	RemoteExitCode  *int                     `json:"remote_exit_code,omitempty"`
	SSHUseErrorCode string                   `json:"ssh_use_error_code,omitempty"`
	Error           string                   `json:"error,omitempty"`
	Decision        string                   `json:"decision,omitempty"`
	Paused          bool                     `json:"paused,omitempty"`
	Mode            string                   `json:"mode,omitempty"`
	Direction       string                   `json:"direction,omitempty"`
	LocalPath       string                   `json:"local_path,omitempty"`
	RemotePath      string                   `json:"remote_path,omitempty"`
	Size            int64                    `json:"size,omitempty"`
	Bytes           int64                    `json:"bytes,omitempty"`
	FileMode        uint32                   `json:"file_mode,omitempty"`
	PreserveMode    bool                     `json:"preserve_mode,omitempty"`
	Atomic          bool                     `json:"atomic,omitempty"`
	Checksum        string                   `json:"checksum,omitempty"`
	TUIConnected    bool                     `json:"tui_connected,omitempty"`
	Elapsed         string                   `json:"elapsed,omitempty"`
	Position        int                      `json:"position,omitempty"`
	Record          *model.CommandRecord     `json:"record,omitempty"`
	Commands        []model.CommandRecord    `json:"commands,omitempty"`
	Connections     []model.ConnectionStatus `json:"connections,omitempty"`
	PolicyRules     []model.PolicyRuleView   `json:"policy_rules,omitempty"`
	Revision        uint64                   `json:"revision,omitempty"`
	PolicyDecision  *model.PolicyDecision    `json:"policy_decision,omitempty"`
	RuntimeSettings *RuntimeSettings         `json:"runtime_settings,omitempty"`
}

// Pointer fields preserve the difference between omitted and explicitly cleared values.
type HostPatch struct {
	Addr *string   `json:"addr,omitempty"`
	User *string   `json:"user,omitempty"`
	Port *int      `json:"port,omitempty"`
	Key  *string   `json:"key,omitempty"`
	Tags *[]string `json:"tags,omitempty"`
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

func NewEncoder(w any) *Encoder {
	if sender, ok := w.(messageSender); ok {
		return &Encoder{encode: sender.Send}
	}
	enc := json.NewEncoder(w.(io.Writer))
	return NewMessageEncoder(func(v Message) error {
		if v.Payload != nil {
			v.Data = base64.StdEncoding.EncodeToString(v.Payload)
			v.Encoding = "base64"
			v.Payload = nil
		}
		return enc.Encode(v)
	})
}

func NewDecoder(r any) *Decoder {
	if receiver, ok := r.(messageReceiver); ok {
		return NewMessageDecoder(receiver.Receive)
	}
	return &Decoder{decode: json.NewDecoder(r.(io.Reader)).Decode}
}

func NewMessageEncoder(send func(Message) error) *Encoder { return &Encoder{encode: send} }
func NewMessageDecoder(receive func() (Message, error)) *Decoder {
	return &Decoder{decode: func(v any) error {
		msg, err := receive()
		if err == nil {
			*v.(*Message) = msg
		}
		return err
	}}
}

// Bound each incoming network frame, not the total stream (uploads may be large).
func NewBoundedDecoder(r io.Reader, maxFrame int) *Decoder {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), maxFrame)
	return &Decoder{decode: func(v any) error {
		if scanner.Scan() {
			return json.Unmarshal(scanner.Bytes(), v)
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		return io.EOF
	}}
}

func Chunk(kind, id string, seq int64, data []byte) Message {
	return Message{
		Type:    kind,
		ID:      id,
		Seq:     seq,
		Payload: append([]byte{}, data...),
	}
}

func DecodeData(msg Message) ([]byte, error) {
	if msg.Payload != nil {
		return msg.Payload, nil
	}
	if msg.Encoding == "base64" {
		return base64.StdEncoding.DecodeString(msg.Data)
	}
	return []byte(msg.Data), nil
}
