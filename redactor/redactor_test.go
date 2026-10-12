package redactor

import "testing"

func TestRedactLine(t *testing.T) {
	t.Setenv("SANITIZE_PATTERNS", `[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,};token=[^\s]+`)
	r, err := NewRedactor()
	if err != nil {
		t.Fatalf("failed to create redactor: %v", err)
	}
	cases := []struct {
		in, want string
	}{
		{"user@example.com", "***"},
		{"token=abc123", "***"},
		{"no match here", "no match here"},
	}
	for _, c := range cases {
		got := r.RedactLine(c.in)
		if got != c.want {
			t.Errorf("RedactLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNewRejectsInvalidPatterns(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
	}{
		{"empty", []string{""}},
		{"invalid", []string{"["}},
		{"invalid after valid", []string{`token=[^\s]+`, "["}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := New(testCase.patterns); err == nil {
				t.Fatal("expected invalid patterns to be rejected")
			}
		})
	}
}

func TestNewRedactorRejectsInvalidPatterns(t *testing.T) {
	t.Setenv("SANITIZE_PATTERNS", `token=[^\s]+;[`)
	if _, err := NewRedactor(); err == nil {
		t.Fatal("expected invalid environment pattern to be rejected")
	}
}

func TestRedactBytes(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		input    string
		want     string
	}{
		{"multiple patterns", []string{`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`, `token=[^\s]+`}, "user@example.com token=abc123", "*** ***"},
		{"no match", []string{`token=[^\s]+`}, "no match here", "no match here"},
		{"no patterns", nil, "unchanged", "unchanged"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			redactor, err := New(testCase.patterns)
			if err != nil {
				t.Fatalf("failed to create redactor: %v", err)
			}
			if got := string(redactor.RedactBytes([]byte(testCase.input))); got != testCase.want {
				t.Errorf("RedactBytes(%q) = %q, want %q", testCase.input, got, testCase.want)
			}
		})
	}
}
