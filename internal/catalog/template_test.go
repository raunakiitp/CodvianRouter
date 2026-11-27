package catalog

import "testing"

// The three real templated catalog base URLs (A1 gap 2). Everything
// here asserts resolution against the templates as they exist in the
// catalog data, so a template edit that breaks resolution fails.
func TestTemplateParamsExtraction(t *testing.T) {
	for _, tc := range []struct {
		tmpl string
		want []string
	}{
		{"https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/v1", []string{"account_id"}},
		{"https://gateway.ai.cloudflare.com/v1/{account_id}/{gateway_id}/{provider}", []string{"account_id", "gateway_id", "provider"}},
		{"https://${SNOWFLAKE_ACCOUNT}.snowflakecomputing.com/api/v2/cortex/v1", []string{"SNOWFLAKE_ACCOUNT"}},
		{"https://api.openai.com/v1", nil},
	} {
		got := TemplateParams(tc.tmpl)
		if len(got) != len(tc.want) {
			t.Fatalf("TemplateParams(%q) = %v, want %v", tc.tmpl, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("TemplateParams(%q)[%d] = %q, want %q", tc.tmpl, i, got[i], tc.want[i])
			}
		}
		if IsTemplatedBaseURL(tc.tmpl) != (len(tc.want) > 0) {
			t.Fatalf("IsTemplatedBaseURL(%q) disagrees with the extracted params", tc.tmpl)
		}
	}
}

func TestResolveBaseURLTemplateRealTemplates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tmpl   string
		params map[string]string
		want   string
	}{
		{
			name:   "cloudflare workers ai",
			tmpl:   "https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/v1",
			params: map[string]string{"account_id": "a1b2c3d4"},
			want:   "https://api.cloudflare.com/client/v4/accounts/a1b2c3d4/ai/v1",
		},
		{
			name:   "cloudflare ai gateway",
			tmpl:   "https://gateway.ai.cloudflare.com/v1/{account_id}/{gateway_id}/{provider}",
			params: map[string]string{"account_id": "a1b2c3d4", "gateway_id": "gw-1", "provider": "openai"},
			want:   "https://gateway.ai.cloudflare.com/v1/a1b2c3d4/gw-1/openai",
		},
		{
			name:   "snowflake cortex",
			tmpl:   "https://${SNOWFLAKE_ACCOUNT}.snowflakecomputing.com/api/v2/cortex/v1",
			params: map[string]string{"SNOWFLAKE_ACCOUNT": "xy12345.eu-west-1"},
			want:   "https://xy12345.eu-west-1.snowflakecomputing.com/api/v2/cortex/v1",
		},
	} {
		got, err := ResolveBaseURLTemplate(tc.tmpl, tc.params)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: resolved = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestResolveBaseURLTemplateMissingParamsAreNamed(t *testing.T) {
	_, err := ResolveBaseURLTemplate(
		"https://gateway.ai.cloudflare.com/v1/{account_id}/{gateway_id}/{provider}",
		map[string]string{"account_id": "a1"})
	if err == nil {
		t.Fatal("missing gateway_id/provider must error")
	}
	for _, name := range []string{"gateway_id", "provider"} {
		if !containsStr(err.Error(), name) {
			t.Errorf("error %q must name the missing parameter %q", err.Error(), name)
		}
	}
	if !containsStr(err.Error(), "template parameter") {
		t.Errorf("error %q must say the values are template parameters", err.Error())
	}
}

func TestResolveBaseURLTemplateUnsafeValuesRejected(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"path traversal", "../admin"},
		{"path separator", "a/b"},
		{"scheme injection", "https://evil.example"},
		{"whitespace", "acct 1"},
		{"brace re-injection", "a{b}"},
	}
	for _, tc := range cases {
		_, err := ResolveBaseURLTemplate(
			"https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/v1",
			map[string]string{"account_id": tc.value})
		if err == nil {
			t.Errorf("%s: value %q must be rejected", tc.name, tc.value)
			continue
		}
		if !containsStr(err.Error(), "safe URL path segment") {
			t.Errorf("%s: error %q must name the path-segment rule", tc.name, err.Error())
		}
	}
}

func TestResolveBaseURLTemplateNonTemplatedPassthrough(t *testing.T) {
	const u = "https://api.openai.com/v1"
	got, err := ResolveBaseURLTemplate(u, nil)
	if err != nil || got != u {
		t.Fatalf("non-templated URL changed: %q, err = %v", got, err)
	}
}

func containsStr(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
