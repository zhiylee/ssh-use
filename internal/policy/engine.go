package policy

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/zhiylee/ssh-use/internal/config"
	"github.com/zhiylee/ssh-use/internal/model"
)

type Engine struct {
	mode          string
	defaultAction model.Action
	rules         []Rule
}

type Rule struct {
	Name     string
	Action   model.Action
	Risk     model.Risk
	Patterns []string
	Regexps  []*regexp.Regexp
}

func New(cfg config.PolicyConfig) (*Engine, error) {
	mode := cfg.Mode
	if mode == "" {
		mode = config.ModeSensitive
	}
	defaultAction := parseAction(cfg.DefaultAction, model.ActionAllow)
	rules := make([]Rule, 0)
	if cfg.BuiltinRules == nil || *cfg.BuiltinRules {
		rules = append(rules, builtinRules()...)
	}
	for _, rc := range cfg.Rules {
		rules = append(rules, Rule{
			Name:     rc.Name,
			Action:   parseAction(rc.Action, model.ActionApprove),
			Risk:     parseRisk(rc.Risk, model.RiskMedium),
			Patterns: rc.Patterns,
		})
	}
	for i := range rules {
		for _, pattern := range rules[i].Patterns {
			re, err := regexp.Compile(pattern)
			if err != nil {
				return nil, fmt.Errorf("policy rule %s pattern %q: %w", rules[i].Name, pattern, err)
			}
			rules[i].Regexps = append(rules[i].Regexps, re)
		}
	}
	return &Engine{mode: mode, defaultAction: defaultAction, rules: rules}, nil
}

func (e *Engine) Mode() string {
	return e.mode
}

func (e *Engine) SetMode(mode string) {
	switch mode {
	case config.ModeAuto, config.ModeSensitive, config.ModeApproval:
		e.mode = mode
	}
}

func (e *Engine) Rules() []Rule {
	result := make([]Rule, len(e.rules))
	copy(result, e.rules)
	return result
}

func (e *Engine) Evaluate(command string) model.PolicyDecision {
	matched := e.bestMatch(command)

	switch e.mode {
	case config.ModeAuto:
		if matched != nil && matched.rule.Action == model.ActionBlock {
			return matched.decision("blocked by policy in auto mode", false)
		}
		if matched != nil {
			decision := matched.decision("allowed by auto mode", false)
			decision.Action = model.ActionAllow
			return decision
		}
		return model.PolicyDecision{Action: model.ActionAllow, Risk: model.RiskLow, Reason: "auto mode default allow", DefaultActionUsed: true}
	case config.ModeApproval:
		if matched != nil && matched.rule.Action == model.ActionBlock {
			return matched.decision("blocked by policy in approval mode", false)
		}
		if matched != nil {
			decision := matched.decision("approval mode requires user approval", false)
			decision.Action = model.ActionApprove
			return decision
		}
		return model.PolicyDecision{Action: model.ActionApprove, Risk: model.RiskMedium, Reason: "approval mode default approve", DefaultActionUsed: true}
	default:
		if matched != nil {
			return matched.decision("matched sensitive policy rule", false)
		}
		return model.PolicyDecision{Action: e.defaultAction, Risk: riskForDefault(e.defaultAction), Reason: "sensitive mode default action", DefaultActionUsed: true}
	}
}

type match struct {
	rule    Rule
	pattern string
}

func (m match) decision(reason string, defaultAction bool) model.PolicyDecision {
	return model.PolicyDecision{
		Action:            m.rule.Action,
		Risk:              m.rule.Risk,
		RuleName:          m.rule.Name,
		MatchedPattern:    m.pattern,
		Reason:            reason,
		DefaultActionUsed: defaultAction,
	}
}

func (e *Engine) bestMatch(command string) *match {
	var best *match
	bestRank := -1
	for _, rule := range e.rules {
		for i, re := range rule.Regexps {
			if !re.MatchString(command) {
				continue
			}
			rank := actionRank(rule.Action)
			if rank > bestRank {
				bestRank = rank
				pattern := ""
				if i < len(rule.Patterns) {
					pattern = rule.Patterns[i]
				}
				candidate := match{rule: rule, pattern: pattern}
				best = &candidate
			}
		}
	}
	return best
}

func actionRank(action model.Action) int {
	switch action {
	case model.ActionBlock:
		return 3
	case model.ActionApprove:
		return 2
	case model.ActionAllow:
		return 1
	default:
		return 0
	}
}

func parseAction(value string, fallback model.Action) model.Action {
	switch model.Action(strings.ToLower(strings.TrimSpace(value))) {
	case model.ActionAllow:
		return model.ActionAllow
	case model.ActionApprove:
		return model.ActionApprove
	case model.ActionBlock:
		return model.ActionBlock
	default:
		return fallback
	}
}

func parseRisk(value string, fallback model.Risk) model.Risk {
	switch model.Risk(strings.ToLower(strings.TrimSpace(value))) {
	case model.RiskLow:
		return model.RiskLow
	case model.RiskMedium:
		return model.RiskMedium
	case model.RiskHigh:
		return model.RiskHigh
	case model.RiskCritical:
		return model.RiskCritical
	default:
		return fallback
	}
}

func riskForDefault(action model.Action) model.Risk {
	if action == model.ActionApprove {
		return model.RiskMedium
	}
	if action == model.ActionBlock {
		return model.RiskCritical
	}
	return model.RiskLow
}

func builtinRules() []Rule {
	return []Rule{
		{
			Name:   "destructive_block",
			Action: model.ActionBlock,
			Risk:   model.RiskCritical,
			Patterns: []string{
				`\brm\s+-rf\s+/($|\s)`,
				`\bmkfs\b`,
				`\bdd\s+.*\bof=/dev/`,
				`\bshutdown\b`,
				`\breboot\b`,
				`\biptables\s+-F\b`,
				`\bufw\s+disable\b`,
				`\bcurl\b.*\|\s*(sh|bash)\b`,
				`\bwget\b.*\|\s*(sh|bash)\b`,
				`\bdocker\s+system\s+prune\b`,
				`\bkubectl\s+delete\b`,
			},
		},
		{
			Name:   "service_mutation",
			Action: model.ActionApprove,
			Risk:   model.RiskHigh,
			Patterns: []string{
				`\bsystemctl\s+(restart|stop|start|reload)\b`,
				`\bdocker\s+(restart|stop|rm|rmi)\b`,
				`\bdocker\s+compose\s+up\b`,
			},
		},
		{
			Name:   "file_mutation",
			Action: model.ActionApprove,
			Risk:   model.RiskHigh,
			Patterns: []string{
				`\bchmod\b`,
				`\bchown\b`,
				`\bmv\s+`,
				`\bcp\s+`,
				`\btee\s+`,
				`\bgit\s+pull\b`,
			},
		},
		{
			Name:   "readonly_inspection",
			Action: model.ActionAllow,
			Risk:   model.RiskLow,
			Patterns: []string{
				`^ls(\s|$)`,
				`^pwd$`,
				`^whoami$`,
				`^hostname$`,
				`^uptime$`,
				`^df\s`,
				`^free\s`,
				`^docker\s+ps(\s|$)`,
				`^docker\s+logs\b`,
				`^systemctl\s+status\s`,
				`^journalctl\s`,
			},
		},
	}
}
