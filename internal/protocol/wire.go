// Domain conversions for the binary protobuf transport.
package protocol

import (
	"github.com/zhiylee/ssh-use/internal/config"
	"github.com/zhiylee/ssh-use/internal/model"
	"github.com/zhiylee/ssh-use/internal/rpcpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"time"
)

func wireTime(v time.Time) *timestamppb.Timestamp {
	if v.IsZero() {
		return nil
	}
	return timestamppb.New(v)
}
func domainTime(v *timestamppb.Timestamp) time.Time {
	if v == nil {
		return time.Time{}
	}
	return v.AsTime()
}
func wireInt(v *int) *int64 {
	if v == nil {
		return nil
	}
	x := int64(*v)
	return &x
}
func domainInt(v *int64) *int {
	if v == nil {
		return nil
	}
	x := int(*v)
	return &x
}

func ToWireHost(v *config.Host) *rpcpb.Host {
	if v == nil {
		return nil
	}
	p := &rpcpb.Host{}
	p.Addr = v.Addr
	p.User = v.User
	p.Port = int64(v.Port)
	p.Key = v.Key
	p.Tags = v.Tags
	return p
}
func FromWireHost(p *rpcpb.Host) *config.Host {
	if p == nil {
		return nil
	}
	v := &config.Host{}
	v.Addr = p.Addr
	v.User = p.User
	v.Port = int(p.Port)
	v.Key = p.Key
	v.Tags = p.Tags
	return v
}
func ToWireHostPatch(v *HostPatch) *rpcpb.HostPatch {
	if v == nil {
		return nil
	}
	p := &rpcpb.HostPatch{}
	p.Addr = v.Addr
	p.User = v.User
	p.Port = wireInt(v.Port)
	p.Key = v.Key
	if v.Tags != nil {
		p.Tags = &rpcpb.StringList{Values: *v.Tags}
	}
	return p
}
func FromWireHostPatch(p *rpcpb.HostPatch) *HostPatch {
	if p == nil {
		return nil
	}
	v := &HostPatch{}
	v.Addr = p.Addr
	v.User = p.User
	v.Port = domainInt(p.Port)
	v.Key = p.Key
	if p.Tags != nil {
		x := append([]string{}, p.Tags.Values...)
		v.Tags = &x
	}
	return v
}
func ToWirePolicyDecision(v *model.PolicyDecision) *rpcpb.PolicyDecision {
	if v == nil {
		return nil
	}
	p := &rpcpb.PolicyDecision{}
	p.Action = string(v.Action)
	p.Risk = string(v.Risk)
	p.RuleName = v.RuleName
	p.MatchedPattern = v.MatchedPattern
	p.Reason = v.Reason
	p.DefaultActionUsed = v.DefaultActionUsed
	return p
}
func FromWirePolicyDecision(p *rpcpb.PolicyDecision) *model.PolicyDecision {
	if p == nil {
		return nil
	}
	v := &model.PolicyDecision{}
	v.Action = model.Action(p.Action)
	v.Risk = model.Risk(p.Risk)
	v.RuleName = p.RuleName
	v.MatchedPattern = p.MatchedPattern
	v.Reason = p.Reason
	v.DefaultActionUsed = p.DefaultActionUsed
	return v
}
func ToWireCommandRecord(v *model.CommandRecord) *rpcpb.CommandRecord {
	if v == nil {
		return nil
	}
	p := &rpcpb.CommandRecord{}
	p.Id = v.ID
	p.CreatedAt = wireTime(v.CreatedAt)
	p.StartedAt = wireTime(v.StartedAt)
	p.FinishedAt = wireTime(v.FinishedAt)
	p.Source = v.Source
	p.ClientPid = int64(v.ClientPID)
	p.ClientCwd = v.ClientCWD
	p.Host = v.Host
	p.RemoteUser = v.RemoteUser
	p.Command = v.Command
	p.DisplayCommand = v.DisplayCommand
	p.Mode = v.Mode
	p.Risk = string(v.Risk)
	p.PolicyAction = string(v.PolicyAction)
	p.PolicyRule = v.PolicyRule
	p.MatchedPattern = v.MatchedPattern
	p.PolicyReason = v.PolicyReason
	p.DefaultAction = v.DefaultAction
	p.Status = string(v.Status)
	p.RemoteExitCode = wireInt(v.RemoteExitCode)
	p.SshUseErrorCode = v.SSHUseErrorCode
	p.DurationMs = v.DurationMS
	p.Stdout = []byte(v.Stdout)
	p.Stderr = []byte(v.Stderr)
	p.StdoutTruncated = v.StdoutTruncated
	p.StderrTruncated = v.StderrTruncated
	p.Error = v.Error
	p.ApprovalStatus = v.ApprovalStatus
	p.ApprovedAt = wireTime(v.ApprovedAt)
	p.PolicyDecision = ToWirePolicyDecision(v.PolicyDecision)
	return p
}
func FromWireCommandRecord(p *rpcpb.CommandRecord) *model.CommandRecord {
	if p == nil {
		return nil
	}
	v := &model.CommandRecord{}
	v.ID = p.Id
	v.CreatedAt = domainTime(p.CreatedAt)
	v.StartedAt = domainTime(p.StartedAt)
	v.FinishedAt = domainTime(p.FinishedAt)
	v.Source = p.Source
	v.ClientPID = int(p.ClientPid)
	v.ClientCWD = p.ClientCwd
	v.Host = p.Host
	v.RemoteUser = p.RemoteUser
	v.Command = p.Command
	v.DisplayCommand = p.DisplayCommand
	v.Mode = p.Mode
	v.Risk = model.Risk(p.Risk)
	v.PolicyAction = model.Action(p.PolicyAction)
	v.PolicyRule = p.PolicyRule
	v.MatchedPattern = p.MatchedPattern
	v.PolicyReason = p.PolicyReason
	v.DefaultAction = p.DefaultAction
	v.Status = model.Status(p.Status)
	v.RemoteExitCode = domainInt(p.RemoteExitCode)
	v.SSHUseErrorCode = p.SshUseErrorCode
	v.DurationMS = p.DurationMs
	v.Stdout = string(p.Stdout)
	v.Stderr = string(p.Stderr)
	v.StdoutTruncated = p.StdoutTruncated
	v.StderrTruncated = p.StderrTruncated
	v.Error = p.Error
	v.ApprovalStatus = p.ApprovalStatus
	v.ApprovedAt = domainTime(p.ApprovedAt)
	v.PolicyDecision = FromWirePolicyDecision(p.PolicyDecision)
	return v
}
func ToWireConnectionStatus(v *model.ConnectionStatus) *rpcpb.ConnectionStatus {
	if v == nil {
		return nil
	}
	p := &rpcpb.ConnectionStatus{}
	p.Host = v.Host
	p.Status = v.Status
	p.User = v.User
	p.Addr = v.Addr
	p.LastUsed = wireTime(v.LastUsed)
	p.ConnectedAt = wireTime(v.ConnectedAt)
	p.OpenSessions = int64(v.OpenSessions)
	p.LastCommand = v.LastCommand
	return p
}
func FromWireConnectionStatus(p *rpcpb.ConnectionStatus) *model.ConnectionStatus {
	if p == nil {
		return nil
	}
	v := &model.ConnectionStatus{}
	v.Host = p.Host
	v.Status = p.Status
	v.User = p.User
	v.Addr = p.Addr
	v.LastUsed = domainTime(p.LastUsed)
	v.ConnectedAt = domainTime(p.ConnectedAt)
	v.OpenSessions = int(p.OpenSessions)
	v.LastCommand = p.LastCommand
	return v
}
func ToWirePolicyRuleView(v *model.PolicyRuleView) *rpcpb.PolicyRuleView {
	if v == nil {
		return nil
	}
	p := &rpcpb.PolicyRuleView{}
	p.Name = v.Name
	p.Action = string(v.Action)
	p.Risk = string(v.Risk)
	p.Patterns = v.Patterns
	p.Matches = int64(v.Matches)
	return p
}
func FromWirePolicyRuleView(p *rpcpb.PolicyRuleView) *model.PolicyRuleView {
	if p == nil {
		return nil
	}
	v := &model.PolicyRuleView{}
	v.Name = p.Name
	v.Action = model.Action(p.Action)
	v.Risk = model.Risk(p.Risk)
	v.Patterns = p.Patterns
	v.Matches = int(p.Matches)
	return v
}
func ToWireRuntimeSettings(v *RuntimeSettings) *rpcpb.RuntimeSettings {
	if v == nil {
		return nil
	}
	p := &rpcpb.RuntimeSettings{}
	p.Generation = v.Generation
	p.Mode = v.Mode
	p.PolicyDefaultAction = v.PolicyDefaultAction
	p.BuiltinRules = v.BuiltinRules
	p.AuditStoreOutput = v.AuditStoreOutput
	p.AuditRetentionDays = int64(v.AuditRetentionDays)
	p.RedactSecrets = v.RedactSecrets
	p.ConfirmRisks = v.ConfirmRisks
	p.Theme = v.Theme
	p.FocusPending = v.FocusPending
	p.BellOnPending = v.BellOnPending
	return p
}
func FromWireRuntimeSettings(p *rpcpb.RuntimeSettings) *RuntimeSettings {
	if p == nil {
		return nil
	}
	v := &RuntimeSettings{}
	v.Generation = p.Generation
	v.Mode = p.Mode
	v.PolicyDefaultAction = p.PolicyDefaultAction
	v.BuiltinRules = p.BuiltinRules
	v.AuditStoreOutput = p.AuditStoreOutput
	v.AuditRetentionDays = int(p.AuditRetentionDays)
	v.RedactSecrets = p.RedactSecrets
	v.ConfirmRisks = p.ConfirmRisks
	v.Theme = p.Theme
	v.FocusPending = p.FocusPending
	v.BellOnPending = p.BellOnPending
	return v
}
func ToWireMessage(v *Message) *rpcpb.Message {
	if v == nil {
		return nil
	}
	p := &rpcpb.Message{}
	p.Cursor = v.Cursor
	p.Version = int64(v.Version)
	p.ConfigRevision = v.ConfigRevision
	p.HostConfig = ToWireHost(v.HostConfig)
	p.HostPatch = ToWireHostPatch(v.HostPatch)
	if v.Hosts != nil {
		p.Hosts = map[string]*rpcpb.Host{}
		for k, x := range v.Hosts {
			p.Hosts[k] = ToWireHost(&x)
		}
	}
	p.Type = v.Type
	p.RequestId = v.RequestID
	p.Ok = v.OK
	p.Id = v.ID
	p.Host = v.Host
	p.Command = v.Command
	p.Source = v.Source
	p.Cwd = v.CWD
	p.ClientPid = int64(v.ClientPID)
	if v.Payload != nil {
		p.Data = v.Payload
	} else {
		p.Data = []byte(v.Data)
	}
	p.Seq = v.Seq
	p.Status = v.Status
	p.RemoteExitCode = wireInt(v.RemoteExitCode)
	p.SshUseErrorCode = v.SSHUseErrorCode
	p.Error = v.Error
	p.Decision = v.Decision
	p.Paused = v.Paused
	p.Mode = v.Mode
	p.Direction = v.Direction
	p.LocalPath = v.LocalPath
	p.RemotePath = v.RemotePath
	p.Size = v.Size
	p.Bytes = v.Bytes
	p.FileMode = v.FileMode
	p.PreserveMode = v.PreserveMode
	p.Atomic = v.Atomic
	p.Checksum = v.Checksum
	p.TuiConnected = v.TUIConnected
	p.Elapsed = v.Elapsed
	p.Position = int64(v.Position)
	p.Record = ToWireCommandRecord(v.Record)
	for _, x := range v.Commands {
		p.Commands = append(p.Commands, ToWireCommandRecord(&x))
	}
	for _, x := range v.Connections {
		p.Connections = append(p.Connections, ToWireConnectionStatus(&x))
	}
	for _, x := range v.PolicyRules {
		p.PolicyRules = append(p.PolicyRules, ToWirePolicyRuleView(&x))
	}
	p.Revision = v.Revision
	p.PolicyDecision = ToWirePolicyDecision(v.PolicyDecision)
	p.RuntimeSettings = ToWireRuntimeSettings(v.RuntimeSettings)
	return p
}
func FromWireMessage(p *rpcpb.Message) *Message {
	if p == nil {
		return nil
	}
	v := &Message{}
	v.Cursor = p.Cursor
	v.Version = int(p.Version)
	v.ConfigRevision = p.ConfigRevision
	v.HostConfig = FromWireHost(p.HostConfig)
	v.HostPatch = FromWireHostPatch(p.HostPatch)
	if p.Hosts != nil {
		v.Hosts = map[string]config.Host{}
		for k, x := range p.Hosts {
			if x != nil {
				v.Hosts[k] = *FromWireHost(x)
			}
		}
	}
	v.Type = p.Type
	v.RequestID = p.RequestId
	v.OK = p.Ok
	v.ID = p.Id
	v.Host = p.Host
	v.Command = p.Command
	v.Source = p.Source
	v.CWD = p.Cwd
	v.ClientPID = int(p.ClientPid)
	if p.Type == "stdout_chunk" || p.Type == "stderr_chunk" || p.Type == "transfer.chunk" {
		v.Payload = p.Data
	} else {
		v.Data = string(p.Data)
	}
	v.Seq = p.Seq
	v.Status = p.Status
	v.RemoteExitCode = domainInt(p.RemoteExitCode)
	v.SSHUseErrorCode = p.SshUseErrorCode
	v.Error = p.Error
	v.Decision = p.Decision
	v.Paused = p.Paused
	v.Mode = p.Mode
	v.Direction = p.Direction
	v.LocalPath = p.LocalPath
	v.RemotePath = p.RemotePath
	v.Size = p.Size
	v.Bytes = p.Bytes
	v.FileMode = p.FileMode
	v.PreserveMode = p.PreserveMode
	v.Atomic = p.Atomic
	v.Checksum = p.Checksum
	v.TUIConnected = p.TuiConnected
	v.Elapsed = p.Elapsed
	v.Position = int(p.Position)
	v.Record = FromWireCommandRecord(p.Record)
	for _, x := range p.Commands {
		if x != nil {
			v.Commands = append(v.Commands, *FromWireCommandRecord(x))
		}
	}
	for _, x := range p.Connections {
		if x != nil {
			v.Connections = append(v.Connections, *FromWireConnectionStatus(x))
		}
	}
	for _, x := range p.PolicyRules {
		if x != nil {
			v.PolicyRules = append(v.PolicyRules, *FromWirePolicyRuleView(x))
		}
	}
	v.Revision = p.Revision
	v.PolicyDecision = FromWirePolicyDecision(p.PolicyDecision)
	v.RuntimeSettings = FromWireRuntimeSettings(p.RuntimeSettings)
	return v
}
