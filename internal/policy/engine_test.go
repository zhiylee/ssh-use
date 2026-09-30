package policy

import (
	"testing"

	"github.com/zhiylee/ssh-use/internal/config"
	"github.com/zhiylee/ssh-use/internal/model"
)

func TestEvaluateSensitiveBuiltinRules(t *testing.T) {
	engine, err := New(config.Default().Policy)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		cmd       string
		action    model.Action
		risk      model.Risk
		rule      string
		defaulted bool
	}{
		{"readonly", "docker ps", model.ActionAllow, model.RiskLow, "readonly_inspection", false},
		{"service", "systemctl restart nginx", model.ActionApprove, model.RiskHigh, "service_mutation", false},
		{"file", "chmod 600 /root/.ssh/id", model.ActionApprove, model.RiskHigh, "file_mutation", false},
		{"destructive", "rm -rf /", model.ActionBlock, model.RiskCritical, "destructive_block", false},
		{"unmatched", "date", model.ActionAllow, model.RiskLow, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := engine.Evaluate(tt.cmd)
			if got.Action != tt.action || got.Risk != tt.risk || got.RuleName != tt.rule || got.DefaultActionUsed != tt.defaulted {
				t.Fatalf("Evaluate(%q) = action=%s risk=%s rule=%q default=%v", tt.cmd, got.Action, got.Risk, got.RuleName, got.DefaultActionUsed)
			}
		})
	}
}

func TestEvaluateModes(t *testing.T) {
	cfg := config.Default().Policy
	engine, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	engine.SetMode(config.ModeAuto)
	if got := engine.Evaluate("systemctl restart nginx"); got.Action != model.ActionAllow {
		t.Fatalf("auto mode should allow approve rules, got %s", got.Action)
	}
	if got := engine.Evaluate("reboot"); got.Action != model.ActionBlock {
		t.Fatalf("auto mode should still block destructive rules, got %s", got.Action)
	}

	engine.SetMode(config.ModeApproval)
	if got := engine.Evaluate("uptime"); got.Action != model.ActionApprove {
		t.Fatalf("approval mode should approve readonly commands, got %s", got.Action)
	}
	if got := engine.Evaluate("rm -rf /"); got.Action != model.ActionBlock {
		t.Fatalf("approval mode should still block destructive rules, got %s", got.Action)
	}
}

func TestCustomRulesAndPriority(t *testing.T) {
	falseValue := false
	engine, err := New(config.PolicyConfig{
		Mode:          config.ModeSensitive,
		DefaultAction: "approve",
		BuiltinRules:  &falseValue,
		Rules: []config.RuleConfig{
			{Name: "allow_all", Action: "allow", Risk: "low", Patterns: []string{`^deploy`}},
			{Name: "block_prod", Action: "block", Risk: "critical", Patterns: []string{`^deploy prod`}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := engine.Evaluate("deploy prod"); got.Action != model.ActionBlock || got.RuleName != "block_prod" {
		t.Fatalf("block should outrank allow, got %#v", got)
	}
	if got := engine.Evaluate("unknown"); got.Action != model.ActionApprove || !got.DefaultActionUsed {
		t.Fatalf("default approve not used, got %#v", got)
	}
}

func TestInvalidRegex(t *testing.T) {
	falseValue := false
	_, err := New(config.PolicyConfig{
		Mode:         config.ModeSensitive,
		BuiltinRules: &falseValue,
		Rules:        []config.RuleConfig{{Name: "bad", Action: "allow", Risk: "low", Patterns: []string{"("}}},
	})
	if err == nil {
		t.Fatal("expected invalid regex error")
	}
}

func TestAccessorsAndFallbackParsing(t *testing.T) {
	falseValue := false
	engine, err := New(config.PolicyConfig{
		Mode:          "",
		DefaultAction: "wat",
		BuiltinRules:  &falseValue,
		Rules: []config.RuleConfig{{
			Name:     "fallbacks",
			Action:   "wat",
			Risk:     "wat",
			Patterns: []string{`^x$`},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if engine.Mode() != config.ModeSensitive {
		t.Fatalf("mode=%s", engine.Mode())
	}
	rules := engine.Rules()
	if len(rules) != 1 || rules[0].Action != model.ActionApprove || rules[0].Risk != model.RiskMedium {
		t.Fatalf("rules=%#v", rules)
	}
	if got := engine.Evaluate("x"); got.Action != model.ActionApprove || got.Risk != model.RiskMedium {
		t.Fatalf("decision=%#v", got)
	}
}
