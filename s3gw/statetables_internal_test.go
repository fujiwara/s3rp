package s3gw

import (
	"testing"

	"github.com/fujiwara/s3rp/internal/tlatable"
)

// The transition tables are written out as TLA+ modules, which the
// specifications in tla/ read: what is model-checked there is these tables,
// not a description of them.

func TestBreakerTable(t *testing.T) {
	var steps []tlatable.Step
	for s := range numBreakerStates {
		for e := range numBreakerEvents {
			st := breakerTransitions[s][e]
			steps = append(steps, tlatable.Step{
				tlatable.F("from", s.String()), tlatable.F("event", e.String()), tlatable.F("to", st.to.String()),
				tlatable.F("admit", st.admit), tlatable.F("count", st.count), tlatable.F("stamp", st.stamp),
			})
		}
	}
	tlatable.Check(t, "Breaker", "./s3gw -run TestBreakerTable", steps)
}

func TestPayloadTable(t *testing.T) {
	var steps []tlatable.Step
	for p := payloadReading; p < numPayloadPhases; p++ {
		for e := range numPayloadEvents {
			if to := payloadTransitions[p][e]; to != 0 {
				steps = append(steps, tlatable.Step{
					tlatable.F("from", p.String()), tlatable.F("event", e.String()), tlatable.F("to", to.String()),
				})
			}
		}
	}
	tlatable.Check(t, "Payload", "./s3gw -run TestPayloadTable", steps)
}

func TestReportTable(t *testing.T) {
	var steps []tlatable.Step
	for o := range numHandlerOutcomes {
		for p := range numResponsePhases {
			st := reportTransitions[o][p]
			steps = append(steps, tlatable.Step{
				tlatable.F("outcome", o.String()), tlatable.F("response", p.String()),
				tlatable.F("document", st.document), tlatable.F("observe", st.observe), tlatable.F("abort", st.abort),
			})
		}
	}
	tlatable.Check(t, "Report", "./s3gw -run TestReportTable", steps)
}
