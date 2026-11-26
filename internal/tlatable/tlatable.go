// Package tlatable writes a Go transition table as a TLA+ module, so that the
// specification in tla/ that is model-checked reads the very table the Go
// code runs on. A test in the table's package renders it and compares the
// result with the module in the tree (Check), as the other generated files
// are held.
package tlatable

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Field is one field of a step: a string, a bool or an int.
type Field struct {
	Name  string
	Value any
}

// Step is one row of a table: what an event does in a state. Every step of
// a table has the same fields, in the same order.
type Step []Field

// F is a Field.
func F(name string, value any) Field { return Field{Name: name, Value: value} }

// Module renders the steps as the TLA+ module <name>Table, which defines
// the set of records <name>Steps. test is the test that holds the module to
// the Go table, named in the header for whoever finds it stale.
func Module(name, test string, steps []Step) ([]byte, error) {
	if len(steps) == 0 {
		return nil, fmt.Errorf("%s: no steps", name)
	}
	module := name + "Table"
	var b bytes.Buffer
	dashes := strings.Repeat("-", max(4, (77-len(" MODULE ")-len(module))/2))
	fmt.Fprintf(&b, "%s MODULE %s %s\n", dashes, module, dashes)
	b.WriteString("\\* Code generated from the Go transition table. DO NOT EDIT.\n")
	fmt.Fprintf(&b, "\\* Regenerate: S3RP_TLA_UPDATE=1 go test %s\n\n", test)
	fmt.Fprintf(&b, "%sSteps == {\n", name)
	for i, step := range steps {
		if len(step) != len(steps[0]) {
			return nil, fmt.Errorf("%s: step %d has %d fields, the first has %d", name, i, len(step), len(steps[0]))
		}
		b.WriteString("    [")
		for j, f := range step {
			if f.Name != steps[0][j].Name {
				return nil, fmt.Errorf("%s: step %d has the field %q where the first has %q", name, i, f.Name, steps[0][j].Name)
			}
			if j > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s |-> ", f.Name)
			switch v := f.Value.(type) {
			case string:
				if strings.ContainsAny(v, "\"\\\n") {
					return nil, fmt.Errorf("%s: step %d: %q cannot be written as a TLA+ string", name, i, v)
				}
				fmt.Fprintf(&b, "%q", v)
			case bool:
				b.WriteString(strings.ToUpper(fmt.Sprint(v)))
			case int:
				fmt.Fprint(&b, v)
			default:
				return nil, fmt.Errorf("%s: step %d: field %s is a %T", name, i, f.Name, f.Value)
			}
		}
		b.WriteString("]")
		if i < len(steps)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n\n")
	b.WriteString(strings.Repeat("=", 77))
	b.WriteString("\n")
	return b.Bytes(), nil
}

// Check holds tla/<name>Table.tla to the steps: it fails when the module in
// the tree is not what the steps render to, and writes it instead when
// S3RP_TLA_UPDATE is set. test is the go test invocation that runs it, as
// one would type it after "go test".
func Check(t *testing.T, name, test string, steps []Step) {
	t.Helper()
	want, err := Module(name, test, steps)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := moduleDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+"Table.tla")
	if os.Getenv("S3RP_TLA_UPDATE") != "" {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is stale: the Go table changed, so the specification that reads it must be checked again.\n"+
			"Run S3RP_TLA_UPDATE=1 go test %s, then tla/check.sh.\nwant:\n%s\ngot:\n%s", path, test, want, got)
	}
}

// moduleDir is the tla directory of the repository the test runs in.
func moduleDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "tla"), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above the working directory")
		}
		dir = parent
	}
}
