package recipe

import (
	"bufio"
	"bytes"
	"regexp"
	"sort"
	"strings"
)

// dotenvKey finds the key of an assignment line in a dotenv file, with or
// without a leading `export`.
var dotenvKey = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=`)

// RenderDotenv overlays vars onto the project's own env file. Every line of
// base survives as written except the assignments of a key in vars, which are
// replaced in place; keys base does not have are appended, sorted. A recipe that
// wrote only its own variables would throw away every other setting the
// project's file carries (spec/30, `env`).
func RenderDotenv(base []byte, vars map[string]string) []byte {
	var out bytes.Buffer
	done := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(base))
	sc.Buffer(make([]byte, 64<<10), MaxSize)
	for sc.Scan() {
		line := sc.Text()
		if m := dotenvKey.FindStringSubmatch(line); m != nil {
			if v, ok := vars[m[1]]; ok {
				if done[m[1]] {
					continue
				}
				done[m[1]] = true
				out.WriteString(m[1] + "=" + dotenvQuote(v) + "\n")
				continue
			}
		}
		out.WriteString(line + "\n")
	}
	var rest []string
	for k := range vars {
		if !done[k] {
			rest = append(rest, k)
		}
	}
	if len(rest) > 0 {
		sort.Strings(rest)
		if out.Len() > 0 {
			out.WriteString("\n# covey.project.yml\n")
		}
		for _, k := range rest {
			out.WriteString(k + "=" + dotenvQuote(vars[k]) + "\n")
		}
	}
	return out.Bytes()
}

// bareValue is what every dotenv reader takes unquoted.
var bareValue = regexp.MustCompile(`^[A-Za-z0-9_./:@+,=-]*$`)

// dotenvQuote writes a value so that the common dotenv readers (phpdotenv,
// node's dotenv, python-dotenv, Spring's) read it back unchanged. Single quotes
// are literal in all of them; double quotes are only used for what single
// quotes cannot carry, a quote or a line break.
func dotenvQuote(v string) string {
	if bareValue.MatchString(v) {
		return v
	}
	if !strings.ContainsAny(v, "'\n\r") {
		return "'" + v + "'"
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`)
	return `"` + r.Replace(v) + `"`
}

// RenderShell writes vars as a file a POSIX shell sources with `set -a`: the
// `environment` form of write_to, exported into what start brings up.
func RenderShell(vars map[string]string) []byte {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out bytes.Buffer
	out.WriteString("# written by covey from covey.project.yml\n")
	for _, k := range keys {
		out.WriteString(k + "=" + ShellQuote(vars[k]) + "\n")
	}
	return out.Bytes()
}

// ShellQuote quotes for a POSIX shell: single quotes, with a single quote
// written as '\”.
func ShellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}
