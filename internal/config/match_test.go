package config

import "testing"

func TestMatcher(t *testing.T) {
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.NewMatcher()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		sub     string
		wantSvc string
		wantPR  int
		wantOK  bool
	}{
		{"pr-123", "web", 123, true},
		{"api-pr-7", "api", 7, true},
		{"pr-abc", "", 0, false},
		{"nope-1", "", 0, false},
		{"pr-", "", 0, false},
	}
	for _, tc := range tests {
		rule, pr, ok := m.Match(tc.sub)
		if ok != tc.wantOK || rule.Service != tc.wantSvc || pr != tc.wantPR {
			t.Errorf("Match(%q) = (%q,%d,%v), want (%q,%d,%v)",
				tc.sub, rule.Service, pr, ok, tc.wantSvc, tc.wantPR, tc.wantOK)
		}
	}
}
