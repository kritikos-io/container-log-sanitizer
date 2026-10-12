package redactor

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

type Redactor struct {
	patterns []*regexp.Regexp
}

// NewRedactor creates a Redactor from env or config file
func NewRedactor() (*Redactor, error) {
	var regexes []string
	if env := os.Getenv("SANITIZE_PATTERNS"); env != "" {
		for _, pat := range strings.Split(env, ";") {
			pat = strings.TrimSpace(pat)
			if pat != "" {
				regexes = append(regexes, pat)
			}
		}
	}
	return New(regexes)
}

func New(patterns []string) (*Redactor, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for index, pattern := range patterns {
		if pattern == "" {
			return nil, fmt.Errorf("redaction pattern %d is empty", index)
		}
		expression, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid redaction pattern %d: %w", index, err)
		}
		compiled = append(compiled, expression)
	}
	return &Redactor{patterns: compiled}, nil
}

func (redactor *Redactor) RedactBytes(line []byte) []byte {
	for _, expression := range redactor.patterns {
		line = expression.ReplaceAll(line, []byte("***"))
	}
	return line
}

// RedactLine applies all patterns to a line
func (r *Redactor) RedactLine(line string) string {
	for _, re := range r.patterns {
		line = re.ReplaceAllString(line, "***")
	}
	return line
}
