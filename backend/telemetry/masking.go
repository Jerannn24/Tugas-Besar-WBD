package telemetry

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	secretAssignment = regexp.MustCompile(`(?i)(password|passwd|token|secret|authorization)\s*[:=]\s*([^\s,;]+)`)
	uriCredentials   = regexp.MustCompile(`(?i)(://)[^:/@]+:[^@]+@`)
)

func MaskSecrets(value string) string {
	value = secretAssignment.ReplaceAllString(value, "$1=[REDACTED]")
	return uriCredentials.ReplaceAllString(value, "$1[REDACTED]@")
}

func MaskSQL(input string) string {
	var out strings.Builder
	out.Grow(len(input))
	for i := 0; i < len(input); {
		if i+1 < len(input) && input[i:i+2] == "--" {
			for i < len(input) && input[i] != '\n' {
				i++
			}
			out.WriteString("--[REDACTED]")
			continue
		}
		if i+1 < len(input) && input[i:i+2] == "/*" {
			i += 2
			depth := 1
			for i < len(input) && depth > 0 {
				if i+1 < len(input) && input[i:i+2] == "/*" {
					depth++
					i += 2
					continue
				}
				if i+1 < len(input) && input[i:i+2] == "*/" {
					depth--
					i += 2
					continue
				}
				i++
			}
			out.WriteString("/*[REDACTED]*/")
			continue
		}
		if input[i] == '\'' || input[i] == '"' {
			quote := input[i]
			i++
			for i < len(input) {
				if input[i] == quote {
					i++
					if i < len(input) && input[i] == quote {
						i++
						continue
					}
					break
				}
				if input[i] == '\\' && i+1 < len(input) {
					i += 2
					continue
				}
				i++
			}
			if quote == '\'' {
				out.WriteString("'[REDACTED]'")
			} else {
				out.WriteString("\"[REDACTED]\"")
			}
			continue
		}
		if input[i] == '$' {
			if end := dollarQuoteEnd(input, i); end > i {
				delim := input[i : strings.Index(input[i:], "$")+i+1]
				closeAt := strings.Index(input[end:], delim)
				if closeAt >= 0 {
					i = end + closeAt + len(delim)
				} else {
					i = len(input)
				}
				out.WriteString("'[REDACTED]'")
				continue
			}
		}
		if unicode.IsDigit(rune(input[i])) && (i == 0 || (!isIdent(input[i-1]) && input[i-1] != '$')) {
			j := i + 1
			for j < len(input) && (unicode.IsDigit(rune(input[j])) || strings.ContainsRune(".eE+-", rune(input[j]))) {
				j++
			}
			out.WriteString("?")
			i = j
			continue
		}
		out.WriteByte(input[i])
		i++
	}
	return out.String()
}

func dollarQuoteEnd(s string, start int) int {
	if start >= len(s) || s[start] != '$' {
		return -1
	}
	for i := start + 1; i < len(s); i++ {
		if s[i] == '$' {
			return i + 1
		}
		if !(s[i] == '_' || s[i] >= 'a' && s[i] <= 'z' || s[i] >= 'A' && s[i] <= 'Z' || s[i] >= '0' && s[i] <= '9') {
			return -1
		}
	}
	return -1
}

func isIdent(b byte) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}
