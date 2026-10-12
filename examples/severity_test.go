package examples

import (
	"bytes"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"text/template"

	"github.com/grafana/alloy/syntax/ast"
	"github.com/grafana/alloy/syntax/parser"
)

type visitor func(ast.Node)

func (visit visitor) Visit(node ast.Node) ast.Visitor {
	if node != nil {
		visit(node)
	}
	return visit
}

func TestSeverityProfile(t *testing.T) {
	data, err := os.ReadFile("severity.alloy")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile("severity.alloy", data)
	if err != nil {
		t.Fatal(err)
	}
	var patterns []*regexp.Regexp
	var ansi *regexp.Regexp
	var normalize *template.Template
	ast.Walk(visitor(func(node ast.Node) {
		block, ok := node.(*ast.BlockStmt)
		if !ok {
			return
		}
		values := map[string]string{}
		for _, statement := range block.Body {
			attribute, ok := statement.(*ast.AttributeStmt)
			if !ok {
				continue
			}
			literal, ok := attribute.Value.(*ast.LiteralExpr)
			if !ok {
				continue
			}
			value, err := strconv.Unquote(literal.Value)
			if err == nil {
				values[attribute.Name.Name] = value
			}
		}
		switch strings.Join(block.Name, ".") {
		case "stage.regex":
			patterns = append(patterns, regexp.MustCompile(values["expression"]))
		case "stage.replace":
			ansi = regexp.MustCompile(values["expression"])
		case "stage.output", "stage.decolorize":
			t.Fatal("severity parsing must not rewrite the original line")
		case "stage.template":
			if values["source"] == "severity" {
				normalize = template.Must(template.New("severity").Funcs(template.FuncMap{"ToLower": strings.ToLower, "TrimSpace": strings.TrimSpace}).Parse(values["template"]))
			}
		}
	}), file)
	if len(patterns) != 2 || ansi == nil || normalize == nil {
		t.Fatal("severity stages are incomplete")
	}
	tests := []struct {
		line, explicit, want string
		profile              int
	}{
		{line: "\x1b[31mERR\x1b[0m 14:44:37 [app.redis] connection failed", want: "error", profile: 0},
		{line: "WRN 14:44:37 [app.redis] closed", want: "warn", profile: 0},
		{line: "INFO 14:44:37 error is a word in this message", want: "info", profile: 0},
		{line: "time=2026-10-11 14:44:37 EEST pid=17 LOG: restartpoint complete", want: "info", profile: 1},
		{line: "PANIC: database failure", want: "fatal", profile: 1},
		{line: "WARNING: connection closed", want: "warn", profile: 1},
		{line: "arbitrary text mentioning ERROR", want: "unknown", profile: 0},
		{line: "  at stack.frame", want: "unknown", profile: 0},
		{line: "ERR anything", explicit: "INFO", want: "info", profile: 0},
		{line: "plain", explicit: "warning", want: "warn", profile: 0},
		{line: "plain", explicit: "30", want: "unknown", profile: 0},
	}
	for _, test := range tests {
		clean := ansi.ReplaceAllString(test.line, "")
		values := map[string]any{"severity_raw": test.explicit}
		if match := patterns[test.profile].FindStringSubmatch(clean); match != nil {
			values["severity_plain"] = match[1]
		}
		var output bytes.Buffer
		if err := normalize.Execute(&output, values); err != nil {
			t.Fatal(err)
		}
		if output.String() != test.want {
			t.Errorf("severity(%q, explicit=%q) = %q, want %q", test.line, test.explicit, output.String(), test.want)
		}
	}
}
