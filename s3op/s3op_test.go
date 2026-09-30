package s3op_test

import (
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/s3op"
	"github.com/fujiwara/s3rp/s3op/internal/s3opgen"
	"github.com/google/go-cmp/cmp"
)

func TestGenerated(t *testing.T) {
	path, want, err := s3opgen.Generate(".")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(string(want), string(got)); diff != "" {
		t.Errorf("%s is stale; run go generate ./s3op (-want +got):\n%s", path, diff)
	}
}

func TestGeneratorRejects(t *testing.T) {
	for name, src := range map[string]string{
		"unknown field":         `{"operations":[{"name":"GetObject","scope":"object","support":"supported","extra":1}]}`,
		"duplicate":             `{"operations":[{"name":"GetObject","scope":"object","support":"supported"},{"name":"GetObject","scope":"object","support":"supported"}]}`,
		"bad scope":             `{"operations":[{"name":"GetObject","scope":"key","support":"supported"}]}`,
		"bad support":           `{"operations":[{"name":"GetObject","scope":"object","support":"maybe"}]}`,
		"bad action":            `{"operations":[{"name":"GetObject","scope":"object","support":"supported","authorizations":[{"action":"GetObject","on":"target"}]}]}`,
		"bad resource":          `{"operations":[{"name":"GetObject","scope":"object","support":"supported","authorizations":[{"action":"s3:GetObject","on":"bucket"}]}]}`,
		"upper-case header":     `{"operations":[{"name":"PutObject","scope":"object","support":"supported","authorizations":[{"action":"s3:PutObjectTagging","on":"target","header":"X-Amz-Tagging"}]}]}`,
		"refused authorizes":    `{"operations":[{"name":"PutBucketPolicy","scope":"bucket","support":"refused","authorizations":[{"action":"s3:PutBucketPolicy","on":"target"}]}]}`,
		"not an operation name": `{"operations":[{"name":"get-object","scope":"object","support":"supported"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s3opgen.File([]byte(src)); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestCheckActionPattern(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		known   bool
	}{
		{"s3:*", true},
		{"s3:GetObject", true},
		{"S3:getobject", true},
		{"s3:Get*", true},
		{"s3:?etObject", true},
		{"s3:GetObjcet", false},
		{"s3:GetObjectVersion", false},
		{"s3:PutBucketPolicy", false},
		{"s3:ListAllMyBuckets", false},
		{"s3:CreateBucket*", false},
	} {
		err := s3op.CheckActionPattern(tc.pattern)
		if tc.known {
			if err != nil {
				t.Errorf("%s: %v", tc.pattern, err)
			}
			continue
		}
		if !errors.Is(err, s3op.ErrUnknownAction) {
			t.Errorf("%s: got %v, want ErrUnknownAction", tc.pattern, err)
		}
		var ue *s3op.UnknownActionError
		if !errors.As(err, &ue) || ue.Pattern != tc.pattern {
			t.Errorf("%s: got %#v, want an UnknownActionError naming it", tc.pattern, err)
		}
	}
}

func TestCatalogCopies(t *testing.T) {
	op, ok := s3op.Lookup(s3op.OpPutObject)
	if !ok {
		t.Fatal("PutObject not found")
	}
	op.Authorizations[0].Action = "s3:Changed"
	s3op.Operations()[0].Name = "Changed"
	s3op.Actions()[0] = "s3:Changed"
	again, _ := s3op.Lookup(s3op.OpPutObject)
	if again.Authorizations[0].Action != s3op.ActionPutObject {
		t.Error("Lookup shares the catalog's slice")
	}
	if s3op.Operations()[0].Name == "Changed" || s3op.Actions()[0] == "s3:Changed" {
		t.Error("the catalog was modified through a returned value")
	}
	if _, ok := s3op.Lookup("NoSuchOperation"); ok {
		t.Error("Lookup found an unknown operation")
	}
}

func TestActionsSortedAndComplete(t *testing.T) {
	var want []string
	for _, op := range s3op.Operations() {
		for _, a := range op.Authorizations {
			if !slices.Contains(want, a.Action) {
				want = append(want, a.Action)
			}
		}
	}
	slices.Sort(want)
	if diff := cmp.Diff(want, s3op.Actions()); diff != "" {
		t.Errorf("Actions differs from the operations' actions (-want +got):\n%s", diff)
	}
}

// docs/s3-api.md renders the catalog for readers: the supported-operations
// table, the refused list and the table of actions a header adds.
func TestCatalogDocumented(t *testing.T) {
	b, err := os.ReadFile("../docs/s3-api.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)

	onSuffix := map[s3op.Resource]string{s3op.OnTarget: "", s3op.OnCopySource: " (copy source)", s3op.OnEachKey: " (each key)"}
	var wantRows, wantRefused, wantHeaders []string
	for _, op := range s3op.Operations() {
		if op.Support == s3op.Refused {
			wantRefused = append(wantRefused, op.Name)
			continue
		}
		var acts []string
		for _, a := range op.Authorizations {
			if a.Header != "" {
				wantHeaders = append(wantHeaders, a.Header+" "+op.Name+" "+a.Action)
				continue
			}
			acts = append(acts, "`"+a.Action+"`"+onSuffix[a.On])
		}
		cell := strings.Join(acts, ", ")
		if cell == "" {
			cell = "—"
		}
		wantRows = append(wantRows, op.Name+" | "+cell)
	}

	var rows []string
	for _, m := range regexp.MustCompile(`(?m)^\| ([A-Z][A-Za-z0-9]+) \| (.+) \|$`).FindAllStringSubmatch(doc, -1) {
		rows = append(rows, m[1]+" | "+m[2])
	}
	if diff := cmp.Diff(wantRows, rows); diff != "" {
		t.Errorf("docs/s3-api.md operations table differs from the catalog (-catalog +docs):\n%s", diff)
	}

	m := regexp.MustCompile(`(?m)recorded under their name on ` + "`Op.Operation`" + ` so a service can count attempts: ([A-Za-z, ]+)\.`).FindStringSubmatch(doc)
	if m == nil {
		t.Fatal("docs/s3-api.md: refused operations sentence not found")
	}
	if diff := cmp.Diff(wantRefused, strings.Split(m[1], ", ")); diff != "" {
		t.Errorf("docs/s3-api.md refused operations differ from the catalog (-catalog +docs):\n%s", diff)
	}

	var headers []string
	headerRow := regexp.MustCompile("(?m)^\\| ((?:`x-amz-[a-z-]+(?:: true)?`(?:, )?)+) \\| ([A-Za-z, ]+) \\| `(s3:[A-Za-z]+)` \\|$")
	for _, m := range headerRow.FindAllStringSubmatch(doc, -1) {
		for h := range strings.SplitSeq(m[1], ", ") {
			h = strings.TrimSuffix(strings.Trim(h, "`"), ": true")
			for op := range strings.SplitSeq(m[2], ", ") {
				if op == "POST upload" {
					op = s3op.OpPostObject
				}
				headers = append(headers, h+" "+op+" "+m[3])
			}
		}
	}
	slices.Sort(wantHeaders)
	slices.Sort(headers)
	if diff := cmp.Diff(wantHeaders, headers); diff != "" {
		t.Errorf("docs/s3-api.md header actions table differs from the catalog (-catalog +docs):\n%s", diff)
	}
}
