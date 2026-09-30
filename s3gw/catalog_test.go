package s3gw_test

import (
	"slices"
	"testing"

	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/s3op"
	"github.com/google/go-cmp/cmp"
)

// Every operation in the s3op catalog is one the gateway dispatches to, and
// every route is in the catalog (the route tables panic at init otherwise).
// ListBuckets and PostObject have their own entry paths in handleRequest;
// TestListBucketsHooked and the PostObject tests check what they record.
func TestCatalogOperationsRouted(t *testing.T) {
	routed := append(s3gw.RouteOperations(), s3op.OpListBuckets, s3op.OpPostObject)
	slices.Sort(routed)
	routed = slices.Compact(routed)
	var catalog []string
	for _, op := range s3op.Operations() {
		catalog = append(catalog, op.Name)
	}
	slices.Sort(catalog)
	if diff := cmp.Diff(catalog, routed); diff != "" {
		t.Errorf("routed operations differ from the s3op catalog (-catalog +routed):\n%s", diff)
	}
}

// The operations below authorize outside the route binding, by hand; their
// catalog entries must say what that code does.
func TestCatalogHandAuthorized(t *testing.T) {
	for name, want := range map[string][]s3op.Authorization{
		// handleRequest answers ListBuckets from the store, for the
		// requester's own tenant only
		s3op.OpListBuckets: nil,
		// deleteObjects authorizes per key
		s3op.OpDeleteObjects: {
			{Action: s3op.ActionDeleteObject, On: s3op.OnEachKey},
			{Action: s3op.ActionBypassGovernanceRetention, On: s3op.OnEachKey, Header: "x-amz-bypass-governance-retention"},
		},
	} {
		op, ok := s3op.Lookup(name)
		if !ok {
			t.Fatalf("%s is not in the catalog", name)
		}
		if diff := cmp.Diff(want, op.Authorizations); diff != "" {
			t.Errorf("%s authorizations (-code +catalog):\n%s", name, diff)
		}
	}
}
