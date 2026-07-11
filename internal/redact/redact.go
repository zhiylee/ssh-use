package redact

import "regexp"

var patterns = []struct {
	re          *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`(?i)(password\s*=\s*)[^\s]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)(token\s*=\s*)[^\s]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)(secret\s*=\s*)[^\s]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)(api_key\s*=\s*)[^\s]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)(AWS_SECRET_ACCESS_KEY\s*=\s*)[^\s]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)(Authorization:\s*Bearer\s+)[^\s]+`), `${1}[REDACTED]`},
}

func Redact(s string) string {
	for _, pattern := range patterns {
		s = pattern.re.ReplaceAllString(s, pattern.replacement)
	}
	return s
}
