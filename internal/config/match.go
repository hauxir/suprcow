package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Matcher resolves an incoming subdomain back to the service and PR number it
// belongs to — the inverse of RenderContext.Host. It is built once from a
// Config and is safe for concurrent use.
type Matcher struct {
	entries []matchEntry
}

type matchEntry struct {
	rule ExposeRule
	re   *regexp.Regexp
}

// NewMatcher compiles the expose subdomain patterns into matchers.
func (c *Config) NewMatcher() (*Matcher, error) {
	m := &Matcher{}
	for _, e := range c.Expose {
		// Turn a pattern like "api-pr-{n}" into ^api-pr-(\d+)$.
		quoted := regexp.QuoteMeta(e.Subdomain)
		pattern := "^" + strings.Replace(quoted, regexp.QuoteMeta(prToken), `(\d+)`, 1) + "$"
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("compile subdomain %q: %w", e.Subdomain, err)
		}
		m.entries = append(m.entries, matchEntry{rule: e, re: re})
	}
	return m, nil
}

// Match resolves a subdomain label (the host minus the base domain) to its
// expose rule and PR number.
func (m *Matcher) Match(subdomain string) (ExposeRule, int, bool) {
	for _, e := range m.entries {
		if groups := e.re.FindStringSubmatch(subdomain); groups != nil {
			pr, err := strconv.Atoi(groups[1])
			if err != nil {
				continue
			}
			return e.rule, pr, true
		}
	}
	return ExposeRule{}, 0, false
}
