package policy_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/policy"
)

// docs/policy.schema.json is the grammar of a bucket policy document. These
// tests hold it to Parse.

const schemaPath = "../docs/policy.schema.json"

// schemaAt walks the schema document down the given object keys.
func schemaAt(t *testing.T, schema any, path ...string) any {
	t.Helper()
	for _, key := range path {
		m, ok := schema.(map[string]any)
		if !ok {
			t.Fatalf("schema: %q is not in an object (%v)", key, path)
		}
		if schema, ok = m[key]; !ok {
			t.Fatalf("schema: no %q (%v)", key, path)
		}
	}
	return schema
}

// TestSchemaLimits: the limits written in the schema are the package's.
func TestSchemaLimits(t *testing.T) {
	text, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	var schema any
	if err := json.Unmarshal(text, &schema); err != nil {
		t.Fatal(err)
	}
	// the array form of a string-or-array element is its second alternative
	arrayOf := func(path ...string) []string { return append(path, "anyOf") }
	maxItems := func(path []string) float64 {
		alternatives := schemaAt(t, schema, path...).([]any)
		return schemaAt(t, alternatives[1], "maxItems").(float64)
	}
	for _, tc := range []struct {
		name string
		got  float64
		want int
	}{
		{"statements", schemaAt(t, schema, "properties", "Statement", "maxItems").(float64), policy.MaxStatements},
		{"actions", maxItems(arrayOf("$defs", "statement", "properties", "Action")), policy.MaxActionsPerStatement},
		{"resources", maxItems(arrayOf("$defs", "statement", "properties", "Resource")), policy.MaxResourcesPerStatement},
		{"principal names", maxItems(arrayOf("$defs", "principalNames", "properties", "S3RP")), policy.MaxPrincipalUsers},
		{"action length", schemaAt(t, schema, "$defs", "action", "maxLength").(float64), policy.MaxPatternLen},
		{"resource length", schemaAt(t, schema, "$defs", "resource", "maxLength").(float64), policy.MaxPatternLen},
	} {
		if int(tc.got) != tc.want {
			t.Errorf("%s: the schema says %v, the package %d", tc.name, tc.got, tc.want)
		}
	}
	keys := schemaAt(t, schema, "$defs", "sourceIP", "patternProperties").(map[string]any)
	for _, key := range keys {
		alternatives := schemaAt(t, key, "anyOf").([]any)
		if got := schemaAt(t, alternatives[1], "maxItems").(float64); int(got) != policy.MaxConditionValues {
			t.Errorf("condition values: the schema says %v, the package %d", got, policy.MaxConditionValues)
		}
	}
	if got := schemaAt(t, schema, "$defs", "principalNames", "required").([]any); len(got) != 1 || got[0] != policy.DefaultPrincipalKey {
		t.Errorf("principal key: the schema requires %v, the package reads %q", got, policy.DefaultPrincipalKey)
	}
}

// schemaStrings are values a mutation puts in a document: each is right for
// some element and wrong for the others, or just past a limit.
var schemaStrings = []string{
	"", "*", "Allow", "Deny", "allow", "2012-10-17", "2008-10-17", "2020-01-01",
	"ta/alice", "ta/*", "Ta/alice", "ta", "t/a", "*/alice", "0a/9b", "t-a/a_b", "_a/alice", "ta/-b", "ta/alice/x",
	"s3:GetObject", "S3:*", "s3:", "iam:PassRole", "s3:K", "s3:" + strings.Repeat("a", 125), "s3:" + strings.Repeat("a", 126),
	fuzzBucket, fuzzBucket + "/*", "other/*", fuzzBucket + "x", fuzzBucket + "/" + strings.Repeat("é", 63), fuzzBucket + "/" + strings.Repeat("é", 64), fuzzBucket + "/" + strings.Repeat("a", 127),
	"10.0.0.0/8", "10.1.2.3", "::1", "2001:db8::/32", "::ffff:10.0.0.0/104", "::ffff:0.0.0.0/95",
	"10.0.0.0/33", "10.0.0", "fe80::1%eth0", "010.0.0.1", "example.com",
	"aws:SourceIp", "AWS:SOURCEIP", "aws:sourceip", "aws:SourceIP ", "awſ:SourceIp", "aws:Referer",
	"S3RP", "s3rp", "AWS",
	"Sid", "Effect", "Principal", "NotPrincipal", "Action", "Resource", "Condition", "NotAction", "NotResource",
	"Version", "Id", "Statement", "statement", "IpAddress", "NotIpAddress", "StringEquals",
}

type schemaMutator struct {
	rng *rand.Rand
}

func (m *schemaMutator) str() string { return schemaStrings[m.rng.IntN(len(schemaStrings))] }

// value is something to put where the grammar expects something else.
func (m *schemaMutator) value() any {
	switch m.rng.IntN(9) {
	case 0:
		return nil
	case 1:
		return float64(m.rng.IntN(3))
	case 2:
		return m.rng.IntN(2) == 0
	case 3:
		return []any{}
	case 4:
		return map[string]any{}
	case 5:
		return []any{m.str(), m.str()}
	case 6:
		return map[string]any{m.str(): m.str()}
	}
	return m.str()
}

// nodes lists every object and array of the document.
func schemaNodes(v any, out []any) []any {
	switch v := v.(type) {
	case map[string]any:
		out = append(out, v)
		for _, key := range slices.Sorted(func(yield func(string) bool) {
			for k := range v {
				if !yield(k) {
					return
				}
			}
		}) {
			out = schemaNodes(v[key], out)
		}
	case []any:
		out = append(out, v)
		for _, e := range v {
			out = schemaNodes(e, out)
		}
	}
	return out
}

// mutate changes one thing somewhere in the document. An array is changed
// through its parent, so the document is passed by its root object.
func (m *schemaMutator) mutate(root map[string]any) {
	nodes := schemaNodes(root, nil)
	var objects []map[string]any
	for _, n := range nodes {
		if o, ok := n.(map[string]any); ok {
			objects = append(objects, o)
		}
	}
	o := objects[m.rng.IntN(len(objects))]
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if len(keys) == 0 {
		o[m.str()] = m.value()
		return
	}
	key := keys[m.rng.IntN(len(keys))]
	switch m.rng.IntN(10) {
	case 8: // the other effect
		for _, o := range objects {
			if e, ok := o["Effect"].(string); ok && m.rng.IntN(2) == 0 {
				o["Effect"] = map[string]string{"Allow": "Deny", "Deny": "Allow"}[e]
			}
		}
	case 9: // Principal for NotPrincipal, or the reverse
		for _, o := range objects {
			for _, pair := range [][2]string{{"Principal", "NotPrincipal"}, {"NotPrincipal", "Principal"}} {
				if v, ok := o[pair[0]]; ok && m.rng.IntN(2) == 0 {
					delete(o, pair[0])
					o[pair[1]] = v
					break
				}
			}
		}
	case 0: // drop a member
		delete(o, key)
	case 1: // add one
		o[m.str()] = m.value()
	case 2: // rename one
		v := o[key]
		delete(o, key)
		switch m.rng.IntN(3) {
		case 0:
			o[strings.ToLower(key)] = v
		case 1:
			o[key+"s"] = v
		default:
			o[m.str()] = v
		}
	case 3: // a value of another kind
		o[key] = m.value()
	case 4: // a string for a string
		o[key] = m.str()
	case 5: // one string of a list changed, or the single string
		switch v := o[key].(type) {
		case []any:
			if len(v) > 0 {
				v[m.rng.IntN(len(v))] = m.str()
			}
		default:
			o[key] = m.str()
		}
	case 6: // a single value where a list was, or the reverse
		switch v := o[key].(type) {
		case []any:
			if len(v) > 0 {
				o[key] = v[0]
			}
		default:
			o[key] = []any{v}
		}
	case 7: // a list grown to a limit, or one past it
		if v, ok := o[key].([]any); ok && len(v) > 0 {
			n := []int{10, 11, 20, 21, 30, 31, 50, 51, 100, 101}[m.rng.IntN(10)]
			grown := make([]any, n)
			for i := range grown {
				grown[i] = v[i%len(v)]
			}
			o[key] = grown
		}
	}
}

// schemaResidue is what Parse refuses that a schema cannot say: the bucket a
// resource names, a Sid given twice, whether a condition value is an
// address, and sizes counted in bytes: of a pattern, and of the document.
var schemaResidue = regexp.MustCompile(strings.Join([]string{
	`does not refer to bucket`,
	`duplicate Sid`,
	`invalid source IP value`,
	`resource pattern too long`,
	`policy is \d+ bytes`,
}, "|"))

// TestSchema: a document Parse accepts is valid against the schema, and one
// that is valid and still refused is refused for a reason of schemaResidue.
// S3RP_JSONSCHEMA names the validator (the jsonschema of mise.toml); without
// it the test is skipped.
func TestSchema(t *testing.T) {
	const validatorEnv = "S3RP_JSONSCHEMA"
	validator := os.Getenv(validatorEnv)
	if validator == "" {
		t.Skipf("%s is not set", validatorEnv)
	}
	// read here, not only by the validator, so that the test cache sees a
	// changed schema
	if _, err := os.ReadFile(schemaPath); err != nil {
		t.Fatal(err)
	}
	seed := rand.Uint64()
	t.Logf("seed %d", seed)
	rng := rand.New(rand.NewPCG(seed, 0))
	m := &schemaMutator{rng: rng}

	type document struct {
		text string
		err  error
	}
	var documents []document
	add := func(v any) {
		text, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		// one document a line
		if bytes.ContainsAny(text, "\n\r") {
			t.Fatalf("a document with a line break: %s", text)
		}
		_, err = policy.Parse(fuzzBucket, string(text), nil)
		documents = append(documents, document{string(text), err})
	}
	for range 20000 {
		data := make([]byte, 512)
		for i := range data {
			data[i] = byte(rng.UintN(256))
		}
		text, err := json.Marshal((&gen{data: data}).policy())
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(text, &doc); err != nil {
			t.Fatal(err)
		}
		if rng.IntN(4) == 0 {
			doc["Version"] = "2012-10-17"
		}
		add(doc)
		for range 1 + rng.IntN(3) {
			m.mutate(doc)
			add(doc)
		}
	}

	// one file a document: for a .jsonl file of the same documents the
	// validator (17.3.3) gave verdicts that differ from these
	dir := t.TempDir()
	for i, d := range documents {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%06d.json", i)), []byte(d.text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), validator, "validate", schemaPath, dir, "--json", "--continue", "--fast")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, _ := cmd.Output() // it exits non-zero when an instance is invalid
	// stdout has the verdicts and stderr the files they are of, in one order
	var files []int
	for line := range strings.Lines(stderr.String()) {
		name := filepath.Base(strings.TrimSpace(line))
		if n, ok := strings.CutSuffix(name, ".json"); ok {
			i, err := strconv.Atoi(n)
			if err != nil {
				t.Fatalf("validator: %s", line)
			}
			files = append(files, i)
		}
	}
	if len(files) != len(documents) {
		t.Fatalf("the validator named %d of %d documents\n%.2000s", len(files), len(documents), stderr.String())
	}
	valid := make([]*bool, len(documents))
	results := json.NewDecoder(bytes.NewReader(out))
	for _, i := range files {
		var result struct {
			Valid *bool `json:"valid"`
		}
		if err := results.Decode(&result); err != nil || result.Valid == nil {
			t.Fatalf("no verdict for document %d: %v", i, err)
		}
		valid[i] = result.Valid
	}

	var acceptedValid, refusedInvalid, residue, failures int
	fail := func(format string, args ...any) {
		if failures++; failures <= 5 {
			t.Errorf(format, args...)
		}
	}
	for i, d := range documents {
		if valid[i] == nil {
			t.Fatalf("no verdict for document %d", i)
		}
		switch valid := *valid[i]; {
		case d.err == nil && valid:
			acceptedValid++
		case d.err == nil:
			fail("Parse accepts a document the schema does not:\n%s", d.text)
		case !valid:
			refusedInvalid++
		case schemaResidue.MatchString(d.err.Error()):
			residue++
		default:
			fail("the schema accepts a document Parse refuses for a reason it could state: %v\n%s", d.err, d.text)
		}
	}
	if failures > 0 {
		t.Errorf("%d of %d documents", failures, len(documents))
	}
	t.Logf("%d documents: %d accepted and valid, %d refused and invalid, %d valid and refused for what a schema cannot say",
		len(documents), acceptedValid, refusedInvalid, residue)
	// each kind must have occurred, or the comparison showed nothing of it
	for name, n := range map[string]int{"accepted and valid": acceptedValid, "refused and invalid": refusedInvalid, "refused as residue": residue} {
		if n == 0 {
			t.Errorf("no document was %s", name)
		}
	}
}
