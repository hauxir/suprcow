package engine

import (
	"context"
	"strings"
	"testing"
)

type call struct {
	dir  string
	name string
	args []string
}

// fakeRunner records calls and returns programmed stdout keyed by a substring
// of the joined args.
type fakeRunner struct {
	calls   []call
	outputs map[string]string
}

func (f *fakeRunner) Run(_ context.Context, dir string, _ []string, name string, args ...string) (string, error) {
	f.calls = append(f.calls, call{dir: dir, name: name, args: args})
	joined := strings.Join(args, " ")
	for k, v := range f.outputs {
		if strings.Contains(joined, k) {
			return v, nil
		}
	}
	return "", nil
}

func (f *fakeRunner) last() call { return f.calls[len(f.calls)-1] }

func TestUpArgs(t *testing.T) {
	f := &fakeRunner{}
	c := NewCompose(f)
	spec := Spec{
		Project:      "demo-pr-123",
		WorkingDir:   "/wt",
		ComposeFiles: []string{"docker-compose.yml", "docker-compose.suprcow.yml"},
		EnvFiles:     []string{".preview.env"},
	}
	if err := c.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.last().args, " ")
	want := "compose -p demo-pr-123 -f docker-compose.yml -f docker-compose.suprcow.yml --env-file .preview.env up -d --build --remove-orphans"
	if got != want {
		t.Errorf("up args:\n got=%q\nwant=%q", got, want)
	}
	if f.last().dir != "/wt" {
		t.Errorf("working dir = %q", f.last().dir)
	}
}

func TestDownArgs(t *testing.T) {
	f := &fakeRunner{}
	c := NewCompose(f)
	if err := c.Down(context.Background(), "demo-pr-1"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.last().args, " ")
	if got != "compose -p demo-pr-1 down -v --remove-orphans" {
		t.Errorf("down args = %q", got)
	}
}

func TestState(t *testing.T) {
	tests := []struct {
		name    string
		outputs map[string]string
		want    RunState
	}{
		{"running", map[string]string{"ps -q": "abc123\n"}, StateRunning},
		{"stopped", map[string]string{"ps -aq": "abc123\n"}, StateStopped},
		{"absent", map[string]string{}, StateAbsent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCompose(&fakeRunner{outputs: tc.outputs})
			got, err := c.State(context.Background(), "p")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("State = %q, want %q", got, tc.want)
			}
		})
	}
}
