package localbackend

import (
	"testing"

	"github.com/fujiwara/s3rp/internal/tlatable"
)

// The transition tables are written out as TLA+ modules, which the
// specifications in tla/ read: what is model-checked there is these tables,
// not a description of them.

func TestBlobTable(t *testing.T) {
	var steps []tlatable.Step
	for s := range numBlobStates {
		for e := range numBlobEvents {
			if to := blobTransitions[s][e]; to != blobNew {
				steps = append(steps, tlatable.Step{
					tlatable.F("from", s.String()), tlatable.F("event", e.String()), tlatable.F("to", to.String()),
				})
			}
		}
	}
	tlatable.Check(t, "Blob", "./s3gw/localbackend -run TestBlobTable", steps)
}

func TestUploadTable(t *testing.T) {
	var steps []tlatable.Step
	for s := range numUploadStates {
		for e := range numUploadEvents {
			if to := uploadTransitions[s][e]; to != uploadUnknown {
				steps = append(steps, tlatable.Step{
					tlatable.F("from", s.String()), tlatable.F("event", e.String()), tlatable.F("to", to.String()),
				})
			}
		}
	}
	tlatable.Check(t, "Upload", "./s3gw/localbackend -run TestUploadTable", steps)
}

func TestVersioningTable(t *testing.T) {
	var steps []tlatable.Step
	for s := versioningOff; s < numVersioningStates; s++ {
		for e := range numVersioningEvents {
			if to := versioningTransitions[s][e]; to != 0 {
				steps = append(steps, tlatable.Step{
					tlatable.F("from", s.String()), tlatable.F("event", e.String()), tlatable.F("to", to.String()),
				})
			}
		}
	}
	tlatable.Check(t, "Versioning", "./s3gw/localbackend -run TestVersioningTable", steps)
}
