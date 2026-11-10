package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"opencode-telegram/internal/occlient"
)

func TestEmptyInlineKeyboardEncodesArray(t *testing.T) {
	data, err := json.Marshal(emptyInlineKeyboard())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"inline_keyboard":[]}` {
		t.Fatalf("empty keyboard = %s, want an explicit empty array", data)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{90 * time.Second, "1m 30s"},
		{2 * time.Minute, "2m"},
		{2*time.Hour + 5*time.Minute, "2h 5m"},
	}
	for _, c := range cases {
		if got := formatDuration(c.d); got != c.want {
			t.Errorf("formatDuration(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestSessionDurationUsesCurrentRun(t *testing.T) {
	cases := []struct {
		name string
		sess occlient.Session
		want string
	}{
		{
			name: "busy marks the latest prompt",
			sess: occlient.Session{Time: occlient.SessionTime{
				Created: 1_000,
				Busy:    50_000,
				Updated: 80_000,
			}},
			want: "30s",
		},
		{
			name: "idle is the end of the run",
			sess: occlient.Session{Time: occlient.SessionTime{
				Created: 1_000,
				Busy:    50_000,
				Updated: 80_000,
				Idle:    81_000,
			}},
			want: "31s",
		},
		{
			name: "sessions without busy fall back to created",
			sess: occlient.Session{Time: occlient.SessionTime{
				Created: 10_000,
				Updated: 70_000,
			}},
			want: "1m",
		},
	}
	for _, c := range cases {
		if got := sessionDuration(c.sess); got != c.want {
			t.Errorf("%s: sessionDuration() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestParseFieldValue(t *testing.T) {
	min, max := 1, 5
	str := occlient.FormField{Type: "string", MinLength: &min, MaxLength: &max}
	if _, err := parseFieldValue(str, ""); err == nil {
		t.Fatal("empty string must fail")
	}
	if _, err := parseFieldValue(str, "abcdef"); err == nil {
		t.Fatal("too long string must fail")
	}
	if got, err := parseFieldValue(str, "abc"); err != nil || got != "abc" {
		t.Fatalf("valid string = %v, %v", got, err)
	}

	num := occlient.FormField{Type: "number", Minimum: float64(1), Maximum: float64(10)}
	if _, err := parseFieldValue(num, "0"); err == nil {
		t.Fatal("number below minimum must fail")
	}
	if got, err := parseFieldValue(num, "2.5"); err != nil || got.(float64) != 2.5 {
		t.Fatalf("valid number = %v, %v", got, err)
	}

	boolean := occlient.FormField{Type: "boolean"}
	if got, err := parseFieldValue(boolean, "yes"); err != nil || got != true {
		t.Fatalf("yes = %v, %v", got, err)
	}
	if got, err := parseFieldValue(boolean, "no"); err != nil || got != false {
		t.Fatalf("no = %v, %v", got, err)
	}
}

func TestWhenOK(t *testing.T) {
	flow := &formFlow{Answers: map[string]any{"mode": "a"}}
	field := occlient.FormField{When: []occlient.FormWhen{{Key: "mode", Op: "eq", Value: "a"}}}
	if !whenOKForTest(flow, field) {
		t.Fatal("eq condition should pass")
	}
	field.When[0].Value = "b"
	if whenOKForTest(flow, field) {
		t.Fatal("eq condition should fail")
	}
	field.When[0].Op = "neq"
	if !whenOKForTest(flow, field) {
		t.Fatal("neq condition should pass")
	}
}

// whenOKForTest avoids constructing an App for the pure condition helper.
func whenOKForTest(flow *formFlow, field occlient.FormField) bool {
	for _, cond := range field.When {
		actual, ok := flow.Answers[cond.Key]
		if !ok {
			return false
		}
		equal := valuesEqual(actual, cond.Value)
		switch cond.Op {
		case "eq":
			if !equal {
				return false
			}
		case "neq":
			if equal {
				return false
			}
		}
	}
	return true
}

func TestTruncateText(t *testing.T) {
	if got := truncateText("short", 20); got != "short" {
		t.Fatalf("got %q", got)
	}
	long := ""
	for i := 0; i < 100; i++ {
		long += "x"
	}
	got := truncateText(long, 50)
	if len(got) > 50 {
		t.Fatalf("truncated length = %d", len(got))
	}
}

func TestTruncateTextIsUTF8Safe(t *testing.T) {
	// 200 bytes of a 2-byte rune. A naive byte cut at 50 lands inside a rune.
	long := strings.Repeat("é", 100)
	got := truncateText(long, 50)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateText produced invalid UTF-8: %q", got)
	}
	if len(got) > 50 {
		t.Fatalf("truncated length = %d, want <= 50", len(got))
	}

	oneLine := truncateOneLine(long, 21)
	if !utf8.ValidString(oneLine) || len(oneLine) > 21 {
		t.Fatalf("truncateOneLine = %q (%d bytes)", oneLine, len(oneLine))
	}
}

func TestSanitizeInvalidUTF8(t *testing.T) {
	bad := "hello \xff world"
	got := sanitizeUTF8(bad)
	if !utf8.ValidString(got) {
		t.Fatalf("sanitizeUTF8 = %q", got)
	}
}
