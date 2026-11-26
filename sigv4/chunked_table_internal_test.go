package sigv4

import (
	"testing"

	"github.com/fujiwara/s3rp/internal/tlatable"
)

// TestChunkTable writes the decoder's transition table out as the TLA+
// module tla/ChunkedReader.tla reads: what is model-checked there is this
// table, not a description of it.
func TestChunkTable(t *testing.T) {
	var steps []tlatable.Step
	for p := chunkStart; p < numChunkPhases; p++ {
		for e := range numChunkEvents {
			if to := chunkTransitions[p][e]; to != 0 {
				steps = append(steps, tlatable.Step{
					tlatable.F("from", p.String()), tlatable.F("event", e.String()), tlatable.F("to", to.String()),
				})
			}
		}
	}
	tlatable.Check(t, "Chunk", "./sigv4 -run TestChunkTable", steps)
}
