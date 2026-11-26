package tlatable_test

import (
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/internal/tlatable"
)

func TestModule(t *testing.T) {
	got, err := tlatable.Module("Door", "./door -run TestDoorTable", []tlatable.Step{
		{tlatable.F("from", "open"), tlatable.F("event", "push"), tlatable.F("to", "shut"), tlatable.F("locks", false), tlatable.F("n", 1)},
		{tlatable.F("from", "shut"), tlatable.F("event", "turn"), tlatable.F("to", "shut"), tlatable.F("locks", true), tlatable.F("n", 2)},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		" MODULE DoorTable ",
		"S3RP_TLA_UPDATE=1 go test ./door -run TestDoorTable",
		"DoorSteps == {\n",
		`    [from |-> "open", event |-> "push", to |-> "shut", locks |-> FALSE, n |-> 1],` + "\n",
		`    [from |-> "shut", event |-> "turn", to |-> "shut", locks |-> TRUE, n |-> 2]` + "\n}\n",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("expect the module to contain %q, got:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(string(got), strings.Repeat("=", 77)+"\n") {
		t.Errorf("expect the module to end with its closing line, got:\n%s", got)
	}
}

func TestModuleRejects(t *testing.T) {
	for name, steps := range map[string][]tlatable.Step{
		"no steps":              nil,
		"a missing field":       {{tlatable.F("from", "a"), tlatable.F("to", "b")}, {tlatable.F("from", "a")}},
		"another field":         {{tlatable.F("from", "a")}, {tlatable.F("to", "a")}},
		"an unknown type":       {{tlatable.F("from", 1.5)}},
		"a quoted string":       {{tlatable.F("from", `a"b`)}},
		"a string of two lines": {{tlatable.F("from", "a\nb")}},
	} {
		t.Run(name, func(t *testing.T) {
			if out, err := tlatable.Module("X", "./x", steps); err == nil {
				t.Errorf("expect an error, got:\n%s", out)
			}
		})
	}
}
