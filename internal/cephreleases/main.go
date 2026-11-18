// Command cephreleases reports where the Ceph releases under test have
// fallen behind upstream, as the "Following Ceph releases" rule in
// AGENTS.md asks: a release that reached GA but is not the default backend
// yet, and a tested release that reached its end of life. The weekly
// ceph-releases workflow turns each finding into an issue.
//
// It reads Ceph's own release index (doc/releases/releases.yml in
// ceph/ceph) from a file, so the caller decides how to fetch it.
package main

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

type finding struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type releaseIndex struct {
	Releases map[string]releaseLine `yaml:"releases"`
}

type releaseLine struct {
	TargetEOL string `yaml:"target_eol"`
	ActualEOL string `yaml:"actual_eol"`
	Releases  []struct {
		Version string `yaml:"version"`
	} `yaml:"releases"`
}

// gaVersion matches a stable release: x.2.z (x.0.z is development, x.1.z
// a release candidate).
var gaVersion = regexp.MustCompile(`^(\d+)\.2\.\d+$`)

// major is the release line's major version, and ga whether it has a
// stable release. A line without one (still in development) has major 0.
func (l releaseLine) major() (major int, ga bool) {
	for _, r := range l.Releases {
		if m := gaVersion.FindStringSubmatch(r.Version); m != nil {
			major, _ = strconv.Atoi(m[1])
			return major, true
		}
	}
	return 0, false
}

// eol is the line's end-of-life date (YYYY-MM-DD) and whether upstream
// has confirmed it; until then the date is upstream's estimate.
func (l releaseLine) eol() (date string, confirmed bool) {
	if l.ActualEOL != "" {
		return l.ActualEOL, true
	}
	return l.TargetEOL, false
}

// check compares the tested majors with the release index. today is
// YYYY-MM-DD, which orders like the index's dates as a plain string.
func check(idx releaseIndex, defaultMajor int, tested []int, today string) []finding {
	type hit struct {
		major int
		finding
	}
	var hits []hit
	for name, line := range idx.Releases {
		major, ga := line.major()
		if !ga {
			continue
		}
		if major > defaultMajor {
			hits = append(hits, hit{major, finding{
				Title: fmt.Sprintf("Ceph %s (v%d) is GA: make it the default test backend", name, major),
				Body: fmt.Sprintf("Ceph %s has a stable release (v%d.2.x), and the compose `ceph` service still runs v%d.\n\n"+
					"Per \"Following Ceph releases\" in AGENTS.md: point `ceph` at the new release once its image is on quay.io, "+
					"move the previous default to a `ceph-<release>` service (compose.yml, the matrix in test.yml, the image choices in s3tests.yml), "+
					"fix `docker/ceph-demo.sh` if the bootstrap changed, and run the s3-tests workflow on the new default.",
					name, major, defaultMajor),
			}})
		}
		date, confirmed := line.eol()
		if slices.Contains(tested, major) && date != "" && date <= today {
			state := "passed its estimated end of life"
			if confirmed {
				state = "reached its end of life"
			}
			hits = append(hits, hit{major, finding{
				Title: fmt.Sprintf("Ceph %s (v%d) is EOL: drop it from the test targets", name, major),
				Body: fmt.Sprintf("Ceph %s (v%d) %s on %s, and it is still a test target.\n\n"+
					"Per \"Following Ceph releases\" in AGENTS.md: remove its compose service, matrix entry and s3-tests image choice, "+
					"together with the workarounds that existed only for it. "+
					"An estimated date can slip: check https://docs.ceph.com/en/latest/releases/ first.",
					name, major, state, date),
			}})
		}
	}
	slices.SortFunc(hits, func(a, b hit) int {
		return cmp.Or(cmp.Compare(a.major, b.major), cmp.Compare(a.Title, b.Title))
	})
	findings := make([]finding, 0, len(hits))
	for _, h := range hits {
		findings = append(findings, h.finding)
	}
	return findings
}

var cephImage = regexp.MustCompile(`quay\.io/ceph/ceph:v(\d+)`)

// testedMajors lists the major of every Ceph image the files name.
func testedMajors(files ...string) ([]int, error) {
	var majors []int
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		for _, m := range cephImage.FindAllStringSubmatch(string(b), -1) {
			n, _ := strconv.Atoi(m[1])
			if !slices.Contains(majors, n) {
				majors = append(majors, n)
			}
		}
	}
	slices.Sort(majors)
	return majors, nil
}

// defaultMajor is the major the compose file's `ceph` service runs.
func defaultMajor(composeFile string) (int, error) {
	b, err := os.ReadFile(composeFile)
	if err != nil {
		return 0, err
	}
	var compose struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(b, &compose); err != nil {
		return 0, fmt.Errorf("%s: %w", composeFile, err)
	}
	m := cephImage.FindStringSubmatch(compose.Services["ceph"].Image)
	if m == nil {
		return 0, fmt.Errorf("%s: the ceph service has no quay.io/ceph/ceph image", composeFile)
	}
	return strconv.Atoi(m[1])
}

func run() error {
	releases := flag.String("releases", "", "Ceph's doc/releases/releases.yml (required)")
	compose := flag.String("compose", "compose.yml", "compose file holding the ceph services")
	also := flag.String("also", ".github/workflows/s3tests.yml", "comma-separated files naming further Ceph images under test")
	today := flag.String("today", time.Now().UTC().Format(time.DateOnly), "date to judge end of life against")
	flag.Parse()
	if *releases == "" {
		return fmt.Errorf("-releases is required")
	}
	b, err := os.ReadFile(*releases)
	if err != nil {
		return err
	}
	var idx releaseIndex
	if err := yaml.Unmarshal(b, &idx); err != nil {
		return fmt.Errorf("%s: %w", *releases, err)
	}
	if len(idx.Releases) == 0 {
		return fmt.Errorf("%s: no releases found", *releases)
	}
	def, err := defaultMajor(*compose)
	if err != nil {
		return err
	}
	files := []string{*compose}
	if *also != "" {
		files = append(files, strings.Split(*also, ",")...)
	}
	tested, err := testedMajors(files...)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(check(idx, def, tested, *today))
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cephreleases:", err)
		os.Exit(1)
	}
}
