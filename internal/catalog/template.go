package catalog

// Templated catalog base URLs: providers whose own docs define the
// endpoint as a pattern the operator must complete (Cloudflare Workers
// AI's {account_id}, Cloudflare AI Gateway's
// {account_id}/{gateway_id}/{provider}, Snowflake Cortex's
// ${SNOWFLAKE_ACCOUNT}). The catalog stores the template verbatim —
// never a guessed concrete host — and this file resolves it at add
// time from operator-supplied parameters.
//
// Design rules (BETTER-than-reference behavior, recorded for A1):
//   - Parameter NAMES are extracted from the template itself, so the
//     refusal for a missing parameter can list exactly what to supply.
//   - Parameter VALUES are validated as URL path segments: no path
//     separators, no whitespace, no braces, no scheme, bounded length.
//     A template value is interpolated into a URL, so a hostile or
//     sloppy value must not be able to redirect the request elsewhere.
//   - Resolution is total: any placeholder left unresolved is an
//     error, never a silently dialable half-URL.

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// {snake_case} placeholders — the catalog's own convention.
	curlyParamRe = regexp.MustCompile(`\{([a-z][a-z0-9_]*)\}`)
	// ${UPPER_SNAKE} placeholders — the shell-style convention some
	// provider docs use (Snowflake Cortex).
	dollarParamRe = regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)\}`)
	// A resolved value must be a single URL path segment.
	templateValueRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// TemplateParams returns the placeholder names a templated base URL
// requires, in first-appearance order, without duplicates. An empty
// result means the URL is not templated.
func TemplateParams(baseURL string) []string {
	seen := map[string]bool{}
	var out []string
	for _, re := range []*regexp.Regexp{curlyParamRe, dollarParamRe} {
		for _, m := range re.FindAllStringSubmatch(baseURL, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
	}
	return out
}

// IsTemplatedBaseURL reports whether a catalog base URL carries
// placeholders that must be resolved before the provider can dial.
func IsTemplatedBaseURL(baseURL string) bool {
	return len(TemplateParams(baseURL)) > 0
}

// ResolveBaseURLTemplate substitutes params into a templated base URL.
// It returns an error naming the missing parameters when any
// placeholder has no value, and an error naming the offending
// parameter when a value is not a safe URL path segment. The resolved
// URL is returned free of placeholders; any residue is an error.
func ResolveBaseURLTemplate(baseURL string, params map[string]string) (string, error) {
	required := TemplateParams(baseURL)
	if len(required) == 0 {
		return baseURL, nil
	}
	missing := make([]string, 0, len(required))
	for _, name := range required {
		v, ok := params[name]
		if !ok || strings.TrimSpace(v) == "" {
			missing = append(missing, name)
			continue
		}
		if !templateValueRe.MatchString(v) {
			return "", fmt.Errorf("template parameter %q is not a safe URL path segment (got %d chars; allowed: letters, digits, dot, underscore, hyphen)", name, len(v))
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("missing template parameter(s) %s — supply them via the template parameters option", strings.Join(missing, ", "))
	}
	resolved := curlyParamRe.ReplaceAllStringFunc(baseURL, func(m string) string {
		return params[strings.Trim(m, "{}")]
	})
	resolved = dollarParamRe.ReplaceAllStringFunc(resolved, func(m string) string {
		return params[strings.TrimSuffix(strings.TrimPrefix(m, "${"), "}")]
	})
	if IsTemplatedBaseURL(resolved) {
		return "", fmt.Errorf("base URL still contains unresolved placeholders after substitution: %q", resolved)
	}
	return resolved, nil
}
