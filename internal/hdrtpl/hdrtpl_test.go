package hdrtpl

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestParseErrors(t *testing.T) {
	for _, v := range []string{
		"{{header.X",               // unclosed
		"a }} b",                   // unmatched
		"{{$sesion}}",              // typo in a variable
		"{{header.Authorization}}", // would leak the gateway key
		"{{header.x-api-key ?? $uuid}}",
		"{{ }}",
		"{{foo}}",
		"a\r\nX-Injected: 1",
	} {
		if _, err := Parse(v); err == nil {
			t.Errorf("accepted %q", v)
		}
	}
}

func TestEval(t *testing.T) {
	h := http.Header{}
	h.Set("X-Session-Id", "conv_1")
	c := &Context{Header: h, Conversation: "ses_abc", RequestID: "req_1", KeyName: "张三 dev", KeyID: 7, Model: "kimi-k3",
		Now: time.Unix(1700000000, 0)}
	cases := []struct {
		tpl, want string
		ok        bool
	}{
		{"fixed", "fixed", true},
		{"{{header.X-Session-Id}}", "conv_1", true},
		{"{{header.x-session-id}}", "conv_1", true}, // header names are case-insensitive
		{"{{header.X-Missing}}", "", false},
		{"{{header.X-Missing?}}", "", false},
		{"{{header.X-Missing ?? $conversation}}", "ses_abc", true},
		{"{{ header.X-Session-Id ?? $conversation }}", "conv_1", true},
		{`{{header.X-Missing ?? "ai-route"}}`, "ai-route", true},
		{"trace-{{$requestId}}-{{$timestamp}}", "trace-req_1-1700000000", true},
		{"{{$keyName}}/{{$keyId}}/{{$model}}", "%E5%BC%A0%E4%B8%89+dev/7/kimi-k3", true},
	}
	for _, tc := range cases {
		tp, err := Parse(tc.tpl)
		if err != nil {
			t.Fatalf("%s: %v", tc.tpl, err)
		}
		got, ok := tp.Eval(c)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s = %q,%v want %q,%v", tc.tpl, got, ok, tc.want, tc.ok)
		}
	}
	// no conversation (e.g. embeddings): the generated fallback is missing too
	tp, _ := Parse("{{header.X-Missing ?? $conversation}}")
	if _, ok := tp.Eval(&Context{}); ok {
		t.Error("empty conversation should omit the header")
	}
	u, _ := Parse("{{$uuid}}")
	a, _ := u.Eval(nil)
	b, _ := u.Eval(nil)
	if a == b || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(a) {
		t.Errorf("uuid: %s %s", a, b)
	}
}

func TestRequiredAndDynamic(t *testing.T) {
	cases := map[string]string{
		"fixed":                                     "",
		"{{header.X-A}}":                            "X-A",
		"{{header.x-opencode-session}}":             "X-Opencode-Session",
		"{{header.X-A?}}":                           "",
		"{{header.X-A ?? $conversation}}":           "",
		"{{$conversation ?? header.X-A}}":           "X-A", // the caller's header is the last resort
		"{{header.X-A}}-{{header.X-B ?? \"none\"}}": "X-A",
	}
	for v, want := range cases {
		tp, err := Parse(v)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(tp.Required(), ","); got != want {
			t.Errorf("Required(%s) = %q, want %q", v, got, want)
		}
		if tp.Dynamic() != (v != "fixed") {
			t.Errorf("Dynamic(%s)", v)
		}
	}
}
