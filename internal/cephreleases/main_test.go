package main

import (
	"os"
	"slices"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/google/go-cmp/cmp"
)

func loadIndex(t *testing.T) releaseIndex {
	t.Helper()
	b, err := os.ReadFile("testdata/releases.yml")
	if err != nil {
		t.Fatal(err)
	}
	var idx releaseIndex
	if err := yaml.Unmarshal(b, &idx); err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestCheck(t *testing.T) {
	const (
		gaUmbrella  = "Ceph umbrella (v21) is GA: make it the default test backend"
		gaTentacle  = "Ceph tentacle (v20) is GA: make it the default test backend"
		eolSquid    = "Ceph squid (v19) is EOL: drop it from the test targets"
		eolQuincy   = "Ceph quincy (v17) is EOL: drop it from the test targets"
		eolTentacle = "Ceph tentacle (v20) is EOL: drop it from the test targets"
	)
	idx := loadIndex(t)
	tests := []struct {
		name         string
		defaultMajor int
		tested       []int
		today        string
		want         []string
	}{
		{"up to date", 21, []int{19, 20, 21}, "2026-11-18", nil},
		{"newer release is GA", 20, []int{19, 20}, "2026-11-18", []string{gaUmbrella}},
		{"two releases behind", 19, []int{19}, "2026-11-18", []string{gaTentacle, gaUmbrella}},
		{"a release candidate is not GA", 21, []int{21}, "2027-08-01", nil},
		{"the day before the estimated EOL", 21, []int{19, 20, 21}, "2026-12-01", nil},
		{"the estimated EOL date", 21, []int{19, 20, 21}, "2026-12-02", []string{eolSquid}},
		{"an untested EOL release is no finding", 21, []int{20, 21}, "2026-12-03", nil},
		{"a confirmed EOL outranks the estimate", 21, []int{17, 21}, "2024-07-01", nil},
		{"confirmed EOL", 21, []int{17, 21}, "2025-01-13", []string{eolQuincy}},
		{"ordered by major", 20, []int{19, 20}, "2027-06-01", []string{eolSquid, eolTentacle, gaUmbrella}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, f := range check(idx, tt.defaultMajor, tt.tested, tt.today) {
				if f.Body == "" {
					t.Errorf("%q has no body", f.Title)
				}
				got = append(got, f.Title)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("findings (-want +got):\n%s", diff)
			}
		})
	}
}

// The weekly job reads the repository's own files; a restructuring that
// stops them parsing must fail here, not go quiet on a schedule.
func TestRepositoryFiles(t *testing.T) {
	const compose, s3tests = "../../compose.yml", "../../.github/workflows/s3tests.yml"
	def, err := defaultMajor(compose)
	if err != nil {
		t.Fatal(err)
	}
	tested, err := testedMajors(compose, s3tests)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(tested, def) {
		t.Errorf("tested majors %v do not include the default %d", tested, def)
	}
	if got := slices.Max(tested); got != def {
		t.Errorf("the default ceph service runs v%d, but v%d is also under test: the default must be the newest", def, got)
	}
}
