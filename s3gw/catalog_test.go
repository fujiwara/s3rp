package s3gw_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
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
		// requester's own tenant only, under the user policy alone
		s3op.OpListBuckets: {{Action: s3op.ActionListAllMyBuckets, On: s3op.OnTarget}},
		// deleteObjects authorizes per key
		s3op.OpDeleteObjects: {
			{Action: s3op.ActionDeleteObject, On: s3op.OnEachKey, VersionAction: s3op.ActionDeleteObjectVersion},
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

// Every operation the SDK's client has is either in the catalog or listed
// as unsupported, and nothing listed as unsupported is unknown to the SDK:
// an SDK update that brings an operation fails here until someone decides
// which of the two it is. PostObject is the one operation no SDK client
// has.
func TestCatalogCoversSDK(t *testing.T) {
	var sdk []string
	client := reflect.TypeFor[*s3.Client]()
	for m := range client.Methods() {
		// an operation is Name(ctx, *NameInput, ...func(*Options))
		if m.Type.NumIn() == 4 && m.Type.In(2).Kind() == reflect.Pointer && m.Type.In(2).Elem().Name() == m.Name+"Input" {
			sdk = append(sdk, m.Name)
		}
	}
	if len(sdk) < 100 {
		t.Fatalf("found %d operations on the SDK client; the method shape changed", len(sdk))
	}
	var known, unsupported []string
	for _, op := range s3op.Operations() {
		if op.Name != s3op.OpPostObject {
			known = append(known, op.Name)
		}
	}
	for _, f := range s3op.Unsupported() {
		unsupported = append(unsupported, f.Operations...)
	}
	for _, name := range append(known, unsupported...) {
		if !slices.Contains(sdk, name) {
			t.Errorf("%s is not an operation of the SDK client", name)
		}
	}
	var unaccounted []string
	for _, name := range sdk {
		if !slices.Contains(known, name) && !slices.Contains(unsupported, name) {
			unaccounted = append(unaccounted, name)
		}
	}
	if len(unaccounted) > 0 {
		t.Errorf("SDK operations neither in the s3op catalog nor listed as unsupported (s3op/operations.json): %s",
			strings.Join(unaccounted, ", "))
	}
}
