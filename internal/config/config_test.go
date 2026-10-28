package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, text string) string {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMissingFileGivesDefaults(t *testing.T) {
	c, problems := Load(filepath.Join(t.TempDir(), "none.toml"))
	if problems != nil || !reflect.DeepEqual(c, Defaults()) {
		t.Errorf("Load = %+v, %v", c, problems)
	}
}

func TestLoadLaysConfigOverDefaults(t *testing.T) {
	c, problems := Load(write(t, `
[keys]
mic = { hold = "fn" }
send = { text = "continue", submit = true }
branch = "/model sonnet"

[stick]
up = { keys = "ctrl+r" }
down = { run = ["notify-send", "hi"] }

[terminal]
command = "foot -e"
`))
	if problems != nil {
		t.Fatalf("problems: %v", problems)
	}
	want := map[[2]string]Action{
		{"keys", "mic"}:     {Hold: "fn"},
		{"keys", "send"}:    {Text: "continue", Submit: true},
		{"keys", "branch"}:  {Name: "/model sonnet"},
		{"keys", "approve"}: {Name: "approve"},
		{"stick", "up"}:     {Keys: "ctrl+r"},
		{"stick", "down"}:   {Run: []string{"notify-send", "hi"}},
		{"dial", "right"}:   {Name: "effort-up"},
	}
	for at, action := range want {
		if got := c.Action(at[0], at[1]); !reflect.DeepEqual(got, action) {
			t.Errorf("%s.%s = %+v, want %+v", at[0], at[1], got, action)
		}
	}
	if !reflect.DeepEqual(c.Terminal, []string{"foot", "-e"}) {
		t.Errorf("terminal = %q", c.Terminal)
	}
}

func TestLoadNamesEachProblem(t *testing.T) {
	c, problems := Load(write(t, `
[keys]
mic = "dictate"
send = { text = "x", keys = "enter" }
lightning = "fast"

[dial]
press = { hold = "fn" }

[pedals]
left = "fast"
`))
	got := strings.Join(problems, "\n")
	for _, want := range []string{
		`keys.mic: unknown action "dictate"`,
		"keys.send: expected one of",
		"keys.lightning: no such control",
		"dial.press: hold works on keys only",
		"[pedals]: no such section",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("problems lack %q:\n%s", want, got)
		}
	}
	if !reflect.DeepEqual(c.Action("keys", "mic"), Action{Name: "voice"}) {
		t.Errorf("a wrong entry should keep its default, got %+v", c.Action("keys", "mic"))
	}
}

func TestLoadReportsSyntaxErrors(t *testing.T) {
	_, problems := Load(write(t, "[keys\n"))
	if len(problems) != 1 {
		t.Errorf("problems = %v", problems)
	}
}

func TestLoadReadsThePadChannel(t *testing.T) {
	c, problems := Load(write(t, "[pad]\nchannel = 2\n"))
	if problems != nil || c.Channel != 2 {
		t.Errorf("Load = %d, %v", c.Channel, problems)
	}
	for _, text := range []string{"[pad]\nchannel = 0\n", "[pad]\nchannel = 10\n", "[pad]\nchannel = \"2\"\n", "[pad]\nport = 2\n"} {
		c, problems := Load(write(t, text))
		if len(problems) != 1 || c.Channel != 0 {
			t.Errorf("Load(%q) = %d, %v", text, c.Channel, problems)
		}
	}
}
