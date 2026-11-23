package s3gw_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fujiwara/s3rp/cors"
	"github.com/fujiwara/s3rp/internal/policyspec"
	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/s3op"
	"github.com/fujiwara/s3rp/store"
)

// Tenant isolation, checked by enumeration against the specification in
// isolationspec_test.go. Two tenants: ta owns the bucket under test, tb is
// the other side. The scope is small on purpose — a rule that breaks,
// breaks in a small world — and every world in it is checked, not a sample.

var isoUsers = []isoUser{
	{tenant: "ta", name: "alice"},
	{tenant: "ta", name: "ann", policy: []policy.ActionStatement{
		{Effect: "Allow", Action: []string{"s3:Get*", "s3:List*"}},
	}},
	{tenant: "tb", name: "bob"},
	{tenant: "tb", name: "carol"},
	{tenant: "tb", name: "dan", policy: []policy.ActionStatement{
		{Effect: "Allow", Action: []string{"s3:*"}},
		{Effect: "Deny", Action: []string{"s3:PutObject"}},
	}},
}

func (u isoUser) keyID() string  { return strings.ToUpper(u.tenant + u.name + "key") }
func (u isoUser) secret() string { return u.name + "secret" }

func isoUserSpecs(tenant string) []userSpec {
	var out []userSpec
	for _, u := range isoUsers {
		if u.tenant == tenant {
			// a copy: the gateway compiles the statements it is given in
			// place, and the worlds run in parallel
			out = append(out, userSpec{name: u.name, keyID: u.keyID(), secret: u.secret(), policy: slices.Clone(u.policy)})
		}
	}
	return out
}

// isoStore serves the world: its buckets with their policies, and the users
// of both tenants.
func isoStore(t *testing.T, w isoWorld) memStore {
	t.Helper()
	m := memStore{keys: map[string]*store.Key{}, buckets: map[string]*store.Bucket{}}
	for _, tenant := range []string{"ta", "tb"} {
		var buckets []bucketSpec
		for name, b := range w {
			if b.owner == tenant {
				buckets = append(buckets, bucketSpec{
					name:       name,
					policyText: isoPolicyText(b.policy),
					// a rule that answers any origin, so a response to a
					// requester the bucket is hidden from would show it
					cors: []*cors.Rule{{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET", "PUT", "POST", "DELETE", "HEAD"}}},
				})
			}
		}
		m.addTenant(t, tenant, isoUserSpecs(tenant), buckets)
	}
	return m
}

// isoPolicies is every policy of up to two statements, in either order, over
// a small vocabulary.
func isoPolicies() [][]isoStatement {
	var allows, denies []isoStatement
	for _, action := range []string{"s3:*", "s3:GetObject", "s3:GetObject*", "s3:PutObject", "s3:ListBucket"} {
		for _, resource := range []string{"b", "b/*", "b/pub/*"} {
			for _, principal := range []string{"*", "ta/*", "ta/alice", "tb/*", "tb/bob"} {
				allows = append(allows, isoStatement{effect: "Allow", principal: principal, action: action, resource: resource})
				denies = append(denies, isoStatement{effect: "Deny", principal: principal, action: action, resource: resource})
			}
			for _, principal := range []string{"ta/alice", "tb/*", "tb/bob"} {
				denies = append(denies, isoStatement{effect: "Deny", principal: principal, notPrincipal: true, action: action, resource: resource})
			}
		}
	}
	// no policy, each statement alone, and every ordered pair: two Denies
	// combine as an Allow and a Deny do, and the answer must not depend on
	// which comes first
	statements := append(slices.Clone(allows), denies...)
	policies := [][]isoStatement{nil}
	for _, first := range statements {
		policies = append(policies, []isoStatement{first})
		for _, second := range statements {
			policies = append(policies, []isoStatement{first, second})
		}
	}
	return policies
}

var (
	isoActions   = []string{"s3:GetObject", "s3:GetObjectVersion", "s3:PutObject", "s3:ListBucket", "s3:DeleteObject"}
	isoResources = []string{"b", "b/pub/k", "b/priv/k"}
)

// isoParts is how many independent parts an enumeration is cut into, only
// to run them in parallel.
const isoParts = 8

// setIsoPolicy gives bucket b of the store the policy, parsed as a store
// would parse it.
func setIsoPolicy(t *testing.T, m memStore, statements []isoStatement) string {
	t.Helper()
	text := isoPolicyText(statements)
	b := m.buckets["b"]
	b.PolicyText, b.Policy = text, nil
	if text != "" {
		p, err := policy.Parse("b", text, nil)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		b.Policy = p
	}
	return text
}

// TestAuthorizationMatchesSpec compares each way the gateway decides —
// bucket resolution, authorize, the per-object authorizer and its skip —
// with the specification, for every policy of isoPolicies and every user,
// action and resource.
func TestAuthorizationMatchesSpec(t *testing.T) {
	policies := isoPolicies()
	for i := range isoParts {
		chunk := policies[i*len(policies)/isoParts : (i+1)*len(policies)/isoParts]
		t.Run(fmt.Sprintf("part%d", i), func(t *testing.T) {
			t.Parallel()
			world := isoWorld{"b": {owner: "ta"}}
			m := isoStore(t, world)
			gw := s3gw.New(m)
			ctx := t.Context()
			for _, statements := range chunk {
				world["b"] = isoBucket{owner: "ta", policy: statements}
				text := setIsoPolicy(t, m, statements)
				for _, u := range isoUsers {
					key := m.keys[u.keyID()]
					for _, action := range isoActions {
						for _, resource := range isoResources {
							got := gw.DecideAs(ctx, key, "b", action, resource)
							visible := world.visible(u, "b")
							permitted := world.permits(u, "b", action, resource)
							want := s3gw.AuthorizationDecision{Visible: visible, Allowed: permitted, PerObject: permitted}
							// the skip is an optimization the specification does
							// not have; it only must never skip a refusal
							want.SkipsCheck = got.SkipsCheck
							if got != want || (got.SkipsCheck && !permitted) {
								t.Fatalf("%s %s on %s under %s:\n got %+v\nwant %+v", u.principal(), action, resource, text, got, want)
							}
						}
					}
				}
			}
		})
	}
	t.Logf("%d policies", len(policies))
}

// TestAuthorizationMatchesLeanSpec is the same comparison with the Lean
// specification (policy/spec) in the Go one's place: what the gateway
// decides is what `permits` and `visible` say there, for the same policies,
// users, actions and resources.
func TestAuthorizationMatchesLeanSpec(t *testing.T) {
	path := os.Getenv(policyspec.Env)
	if path == "" {
		t.Skipf("%s is not set", policyspec.Env)
	}
	policies := isoPolicies()
	for i := range isoParts {
		chunk := policies[i*len(policies)/isoParts : (i+1)*len(policies)/isoParts]
		t.Run(fmt.Sprintf("part%d", i), func(t *testing.T) {
			t.Parallel()
			m := isoStore(t, isoWorld{"b": {owner: "ta"}})
			gw := s3gw.New(m)
			ctx := t.Context()
			spec, err := policyspec.Start(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			for _, statements := range chunk {
				text := setIsoPolicy(t, m, statements)
				specStatements, err := policyspec.Statements(m.buckets["b"].Policy)
				if err != nil {
					t.Fatal(err)
				}
				for _, u := range isoUsers {
					key := m.keys[u.keyID()]
					c := &policyspec.Case{
						Owner:      "ta",
						Statements: specStatements,
						UserPolicy: policyspec.UserStatements(key.Policy),
					}
					for _, action := range isoActions {
						for _, resource := range isoResources {
							c.Requests = append(c.Requests, policyspec.NewRequest(u.principal(), action, resource, netip.Addr{}))
						}
					}
					answers, err := spec.Ask(c)
					if err != nil {
						t.Fatal(err)
					}
					for j, r := range c.Requests {
						got := gw.DecideAs(ctx, key, "b", r.Action, r.Resource)
						want := s3gw.AuthorizationDecision{
							Visible:    answers[j].Visible,
							Allowed:    answers[j].Permits,
							PerObject:  answers[j].Permits,
							SkipsCheck: got.SkipsCheck,
						}
						if got != want || (got.SkipsCheck && !answers[j].Permits) {
							t.Fatalf("%s %s on %s under %s:\n got %+v\nwant %+v", u.principal(), r.Action, r.Resource, text, got, want)
						}
					}
				}
			}
			if err := spec.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestIsolationSpecMatchesLeanSpec: the specification of isolationspec_test.go
// and the Lean one (policy/spec) say the same of whole requests — every
// operation of the catalog in every form isoRequests gives it, a copy from
// each bucket, and DeleteObjects key by key — in every world of isoWorlds,
// as every user. The gateway is held to the first by
// TestOperationsEnforceIsolation and TestDeleteObjectsEnforcesIsolationPerKey;
// which actions an operation needs is that specification's reading of the
// catalog, and is given to the Lean one.
func TestIsolationSpecMatchesLeanSpec(t *testing.T) {
	path := os.Getenv(policyspec.Env)
	if path == "" {
		t.Skipf("%s is not set", policyspec.Env)
	}
	spec, err := policyspec.Start(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}

	var requests []isoRequest
	for name := range isoRoutes {
		op, ok := s3op.Lookup(name)
		if !ok {
			t.Fatalf("isoRoutes names %s, which is not in the catalog", name)
		}
		if op.Scope != s3op.ScopeService {
			requests = append(requests, isoRequests(op)...)
		}
	}
	deleteKeys := []struct {
		key     string
		version bool
	}{{"pub/k", false}, {"priv/k", false}, {"pub/v", true}, {"priv/v", true}}

	others := isoWorld{
		"b2": {owner: "ta", policy: []isoStatement{{effect: "Allow", principal: "*", action: "s3:GetObject*", resource: "b2/*"}}},
		"c":  {owner: "tb", policy: []isoStatement{{effect: "Allow", principal: "*", action: "s3:GetObject*", resource: "c/*"}}},
	}
	compared := 0
	for _, statements := range isoWorlds() {
		world := isoWorld{"b": {owner: "ta", policy: statements}}
		maps.Copy(world, others)
		var buckets []policyspec.Bucket
		for _, name := range slices.Sorted(maps.Keys(world)) {
			b := policyspec.Bucket{Name: name, Owner: world[name].owner, Statements: []policyspec.Statement{}}
			if text := isoPolicyText(world[name].policy); text != "" {
				p, err := policy.Parse(name, text, nil)
				if err != nil {
					t.Fatalf("%s: %v", text, err)
				}
				if b.Statements, err = policyspec.Statements(p); err != nil {
					t.Fatal(err)
				}
			}
			buckets = append(buckets, b)
		}
		text := isoPolicyText(statements)
		for _, u := range isoUsers {
			w := &policyspec.World{
				Buckets:    buckets,
				UserPolicy: policyspec.UserStatements(&policy.UserPolicy{Statements: u.policy}),
			}
			var want []policyspec.Verdict
			var names []string
			for _, r := range requests {
				resource := "b"
				if r.key != "" {
					resource += "/" + r.key
				}
				o := policyspec.Operation{Tenant: u.tenant, User: u.name, Bucket: "b", Needs: []policyspec.Need{}}
				for _, action := range r.targetActions() {
					o.Needs = append(o.Needs, policyspec.Need{Action: action, Resource: resource})
				}
				if action := r.copyAction(); action != "" {
					o.Copy = &policyspec.Copy{Bucket: r.copyBucket, Action: action, Resource: r.copyBucket + "/" + isoCopyKey}
				}
				w.Operations = append(w.Operations, o)
				want = append(want, policyspec.Verdict{Admits: world.admitted(u, "b", r)})
				names = append(names, r.String())
			}
			for _, bypass := range []bool{false, true} {
				o := policyspec.Operation{Tenant: u.tenant, User: u.name, Bucket: "b", Needs: []policyspec.Need{}}
				v := policyspec.Verdict{Admits: world.visible(u, "b")}
				for _, k := range deleteKeys {
					action := s3op.ActionDeleteObject
					if k.version {
						action = s3op.ActionDeleteObjectVersion
					}
					needs := []policyspec.Need{{Action: action, Resource: "b/" + k.key}}
					permitted := world.permits(u, "b", action, "b/"+k.key)
					// the header asks for one more action on every key
					if bypass {
						needs = append(needs, policyspec.Need{Action: s3op.ActionBypassGovernanceRetention, Resource: "b/" + k.key})
						permitted = permitted && world.permits(u, "b", s3op.ActionBypassGovernanceRetention, "b/"+k.key)
					}
					o.Keys = append(o.Keys, needs)
					if v.Admits {
						v.Deletes = append(v.Deletes, permitted)
					}
				}
				w.Operations = append(w.Operations, o)
				want = append(want, v)
				names = append(names, fmt.Sprintf("DeleteObjects (bypass %v)", bypass))
			}
			got, err := spec.AskWorld(w)
			if err != nil {
				t.Fatal(err)
			}
			for i := range want {
				compared++
				if !reflect.DeepEqual(got[i], want[i]) {
					t.Fatalf("%s as %s under %q:\n   Lean specification: %+v\nisolation specification: %+v", names[i], u.principal(), text, got[i], want[i])
				}
			}
		}
	}
	if err := spec.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d operations agree", compared)
}

// isoRoutes is how each operation of the catalog is asked for: the method
// and the query that select it. TestOperationsEnforceIsolation fails for an
// operation that is routed and missing here, and checks through the
// observer that each entry reaches the operation it is listed under.
var isoRoutes = map[string][2]string{
	s3op.OpListBuckets:                        {"GET", ""},
	s3op.OpHeadBucket:                         {"HEAD", ""},
	s3op.OpListObjects:                        {"GET", ""},
	s3op.OpListObjectsV2:                      {"GET", "list-type=2"},
	s3op.OpListObjectVersions:                 {"GET", "versions"},
	s3op.OpListMultipartUploads:               {"GET", "uploads"},
	s3op.OpGetBucketLocation:                  {"GET", "location"},
	s3op.OpGetBucketAcl:                       {"GET", "acl"},
	s3op.OpGetBucketPolicy:                    {"GET", "policy"},
	s3op.OpGetBucketPolicyStatus:              {"GET", "policyStatus"},
	s3op.OpGetBucketCors:                      {"GET", "cors"},
	s3op.OpGetBucketLifecycleConfiguration:    {"GET", "lifecycle"},
	s3op.OpGetBucketReplication:               {"GET", "replication"},
	s3op.OpGetBucketTagging:                   {"GET", "tagging"},
	s3op.OpGetBucketNotificationConfiguration: {"GET", "notification"},
	s3op.OpGetBucketLogging:                   {"GET", "logging"},
	s3op.OpGetBucketVersioning:                {"GET", "versioning"},
	s3op.OpGetBucketEncryption:                {"GET", "encryption"},
	s3op.OpGetBucketOwnershipControls:         {"GET", "ownershipControls"},
	s3op.OpGetPublicAccessBlock:               {"GET", "publicAccessBlock"},
	s3op.OpGetObjectLockConfiguration:         {"GET", "object-lock"},
	s3op.OpDeleteObjects:                      {"POST", "delete"},
	s3op.OpPostObject:                         {"POST", ""},
	s3op.OpCreateBucket:                       {"PUT", ""},
	s3op.OpDeleteBucket:                       {"DELETE", ""},
	s3op.OpPutBucketAcl:                       {"PUT", "acl"},
	s3op.OpPutBucketPolicy:                    {"PUT", "policy"},
	s3op.OpDeleteBucketPolicy:                 {"DELETE", "policy"},
	s3op.OpPutBucketCors:                      {"PUT", "cors"},
	s3op.OpDeleteBucketCors:                   {"DELETE", "cors"},
	s3op.OpPutBucketLifecycleConfiguration:    {"PUT", "lifecycle"},
	s3op.OpDeleteBucketLifecycle:              {"DELETE", "lifecycle"},
	s3op.OpPutBucketReplication:               {"PUT", "replication"},
	s3op.OpDeleteBucketReplication:            {"DELETE", "replication"},
	s3op.OpPutBucketTagging:                   {"PUT", "tagging"},
	s3op.OpDeleteBucketTagging:                {"DELETE", "tagging"},
	s3op.OpPutBucketNotificationConfiguration: {"PUT", "notification"},
	s3op.OpPutBucketLogging:                   {"PUT", "logging"},
	s3op.OpPutBucketVersioning:                {"PUT", "versioning"},
	s3op.OpPutObjectLockConfiguration:         {"PUT", "object-lock"},
	s3op.OpPutBucketEncryption:                {"PUT", "encryption"},
	s3op.OpDeleteBucketEncryption:             {"DELETE", "encryption"},
	s3op.OpPutBucketOwnershipControls:         {"PUT", "ownershipControls"},
	s3op.OpDeleteBucketOwnershipControls:      {"DELETE", "ownershipControls"},
	s3op.OpPutPublicAccessBlock:               {"PUT", "publicAccessBlock"},
	s3op.OpDeletePublicAccessBlock:            {"DELETE", "publicAccessBlock"},
	s3op.OpGetObject:                          {"GET", ""},
	s3op.OpHeadObject:                         {"HEAD", ""},
	s3op.OpGetObjectAttributes:                {"GET", "attributes"},
	s3op.OpPutObject:                          {"PUT", ""},
	s3op.OpCopyObject:                         {"PUT", ""},
	s3op.OpDeleteObject:                       {"DELETE", ""},
	s3op.OpGetObjectTagging:                   {"GET", "tagging"},
	s3op.OpPutObjectTagging:                   {"PUT", "tagging"},
	s3op.OpDeleteObjectTagging:                {"DELETE", "tagging"},
	s3op.OpGetObjectAcl:                       {"GET", "acl"},
	s3op.OpPutObjectAcl:                       {"PUT", "acl"},
	s3op.OpGetObjectRetention:                 {"GET", "retention"},
	s3op.OpPutObjectRetention:                 {"PUT", "retention"},
	s3op.OpGetObjectLegalHold:                 {"GET", "legal-hold"},
	s3op.OpPutObjectLegalHold:                 {"PUT", "legal-hold"},
	s3op.OpCreateMultipartUpload:              {"POST", "uploads"},
	s3op.OpUploadPart:                         {"PUT", "partNumber=1&uploadId=u"},
	s3op.OpUploadPartCopy:                     {"PUT", "partNumber=1&uploadId=u"},
	s3op.OpCompleteMultipartUpload:            {"POST", "uploadId=u"},
	s3op.OpAbortMultipartUpload:               {"DELETE", "uploadId=u"},
	s3op.OpListParts:                          {"GET", "uploadId=u"},
}

// a value each action-bearing header is sent with
var isoHeaderValues = map[string]string{
	"x-amz-tagging":                       "team=a",
	"x-amz-object-lock-mode":              "GOVERNANCE",
	"x-amz-object-lock-retain-until-date": "2099-01-01T00:00:00Z",
	"x-amz-object-lock-legal-hold":        "ON",
	"x-amz-bypass-governance-retention":   "true",
}

// isoRequests lists the requests of one operation: each key, with and
// without a version where the catalog distinguishes it, with each header
// that adds an action, and for a copy from each bucket of the world and
// one that does not exist.
func isoRequests(op s3op.Operation) []isoRequest {
	keys := []string{""}
	if op.Scope == s3op.ScopeObject || op.Name == s3op.OpPostObject {
		keys = []string{"pub/k", "priv/k"}
	}
	var hasVersion, isCopy bool
	headers := []string{""}
	for _, a := range op.Authorizations {
		switch {
		case a.On == s3op.OnCopySource:
			isCopy = true
		case a.On != s3op.OnTarget:
		case a.Header != "":
			headers = append(headers, a.Header)
		case a.VersionAction != "":
			hasVersion = true
		}
	}
	var out []isoRequest
	for _, key := range keys {
		for _, header := range headers {
			r := isoRequest{op: op, key: key, header: header}
			if !isCopy {
				out = append(out, r)
				if hasVersion {
					r.version = true
					out = append(out, r)
				}
				continue
			}
			for _, src := range []string{"b", "b2", "c", "nx"} {
				r.copyBucket = src
				r.copyVersion = false
				out = append(out, r)
				r.copyVersion = true
				out = append(out, r)
			}
		}
	}
	return out
}

// isoSigned is a request signed once and served to every world.
type isoSigned struct {
	user   isoUser
	req    isoRequest
	method string
	target string
	header http.Header
	body   []byte
}

func (s *isoSigned) request() *http.Request {
	r := httptest.NewRequest(s.method, s.target, bytes.NewReader(s.body))
	r.Header = s.header.Clone()
	return r
}

const isoOrigin = "https://app.example.com"

func signIsoRequest(t *testing.T, u isoUser, r isoRequest, now time.Time) *isoSigned {
	t.Helper()
	if r.op.Name == s3op.OpPostObject {
		return signIsoPost(t, u, r, now)
	}
	route := isoRoutes[r.op.Name]
	target := "http://s3.example.com/"
	if r.op.Scope != s3op.ScopeService {
		target += "b"
	}
	if r.key != "" {
		target += "/" + r.key
	}
	query := route[1]
	if r.version {
		query = strings.TrimPrefix(query+"&versionId=v1", "&")
	}
	if query != "" {
		target += "?" + query
	}
	emptyHash := sha256.Sum256(nil)
	req := signedRequest(t, route[0], target, nil, hex.EncodeToString(emptyHash[:]), now,
		aws.Credentials{AccessKeyID: u.keyID(), SecretAccessKey: u.secret()},
		func(req *http.Request) {
			req.Header.Set("Origin", isoOrigin)
			if r.header != "" {
				req.Header.Set(r.header, isoHeaderValues[r.header])
			}
			if r.copyBucket != "" {
				src := "/" + r.copyBucket + "/" + isoCopyKey
				if r.copyVersion {
					src += "?versionId=v1"
				}
				req.Header.Set("x-amz-copy-source", src)
			}
		})
	return &isoSigned{user: u, req: r, method: route[0], target: target, header: req.Header}
}

// signIsoPost builds the browser upload form of the request: the signed
// policy covers the bucket, the key and the action-bearing field, which a
// form carries under the header's name.
func signIsoPost(t *testing.T, u isoUser, r isoRequest, now time.Time) *isoSigned {
	t.Helper()
	now = now.UTC()
	scopeDate, amzDate := now.Format("20060102"), now.Format("20060102T150405Z")
	cred := u.keyID() + "/" + scopeDate + "/us-east-1/s3/aws4_request"
	fields := [][2]string{{"key", r.key}}
	if r.header != "" {
		fields = append(fields, [2]string{r.header, isoHeaderValues[r.header]})
	}
	conds := []string{
		`{"bucket": "b"}`,
		`{"x-amz-algorithm": "AWS4-HMAC-SHA256"}`,
		`{"x-amz-credential": "` + cred + `"}`,
		`{"x-amz-date": "` + amzDate + `"}`,
	}
	for _, f := range fields {
		conds = append(conds, `["starts-with", "$`+f[0]+`", ""]`)
	}
	doc := `{"expiration": "` + now.Add(time.Hour).Format("2006-01-02T15:04:05.000Z") +
		`", "conditions": [` + strings.Join(conds, ",") + `]}`
	b64 := base64.StdEncoding.EncodeToString([]byte(doc))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.SetBoundary("isolationisolationisolation")
	mw.WriteField("x-amz-algorithm", "AWS4-HMAC-SHA256")
	mw.WriteField("x-amz-credential", cred)
	mw.WriteField("x-amz-date", amzDate)
	mw.WriteField("policy", b64)
	mw.WriteField("x-amz-signature", signPost(u.secret(), scopeDate, "us-east-1", b64))
	for _, f := range fields {
		mw.WriteField(f[0], f[1])
	}
	fw, err := mw.CreateFormFile("file", "f")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte("x"))
	mw.Close()
	header := http.Header{}
	header.Set("Content-Type", mw.FormDataContentType())
	header.Set("Origin", isoOrigin)
	return &isoSigned{user: u, req: r, method: "POST", target: "http://s3.example.com/b", header: header, body: buf.Bytes()}
}

// isoAuthorizer stands where the hooks are: it records the operation that
// got past the policies and, unless admit is set, stops it there, so that a
// request's fate is read from the response without a working backend.
type isoAuthorizer struct {
	admit bool
	op    *s3gw.Op
}

const isoReached = 418

func (a *isoAuthorizer) Authorize(_ context.Context, op *s3gw.Op) error {
	a.op = op
	if a.admit {
		return nil
	}
	return s3err.New(isoReached, "Reached", "the request passed authorization")
}

// isoGateway is a gateway over one world, with what a request did to it.
type isoGateway struct {
	gw    *s3gw.Gateway
	stub  *stubBackend
	auth  *isoAuthorizer
	route string
}

func newIsoGateway(t *testing.T, w isoWorld) *isoGateway {
	t.Helper()
	g := &isoGateway{stub: &stubBackend{}, auth: &isoAuthorizer{}}
	g.gw = gatewayFor(t, isoStore(t, w), g.stub)
	g.gw.SetAuthorizer(g.auth)
	// the request id is the one part of a response that differs by design
	g.gw.SetRequestID(func(*http.Request) string { return "isolation" })
	g.gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { g.route = info.Route })
	return g
}

func (g *isoGateway) serve(s *isoSigned) *httptest.ResponseRecorder {
	*g.stub = stubBackend{}
	g.auth.op, g.route = nil, ""
	rec := httptest.NewRecorder()
	g.gw.Handler().ServeHTTP(rec, s.request())
	return rec
}

// isoWorlds are the policies of the bucket under test. The hand-written
// ones each hold a rule of the specification against the others; one more
// per action of the catalog grants tb/bob that action alone, so every
// operation is seen needing its own action and passing on no other.
func isoWorlds() [][]isoStatement {
	worlds, perAction := isoWorldSets()
	return append(worlds, perAction...)
}

// isoWorldSets returns the hand-written worlds and the per-action ones
// apart: the latter differ only in what tb/bob is granted.
func isoWorldSets() (worlds, perAction [][]isoStatement) {
	allow := func(principal, action, resource string) isoStatement {
		return isoStatement{effect: "Allow", principal: principal, action: action, resource: resource}
	}
	deny := func(principal, action, resource string) isoStatement {
		return isoStatement{effect: "Deny", principal: principal, action: action, resource: resource}
	}
	worlds = [][]isoStatement{
		nil,
		{allow("tb/bob", "s3:*", "b"), allow("tb/bob", "s3:*", "b/*")},
		{allow("tb/*", "s3:*", "b"), allow("tb/*", "s3:*", "b/*")},
		{allow("*", "s3:*", "b"), allow("*", "s3:*", "b/*"), deny("tb/bob", "s3:*", "b/priv/*")},
		{allow("tb/carol", "s3:*", "b/*")},
		{allow("tb/bob", "s3:*", "b/pub/*"), deny("*", "s3:Delete*", "b/*")},
		{deny("ta/alice", "s3:PutObject", "b/*")},
		{deny("ta/*", "s3:*", "b/priv/*"), deny("ta/*", "s3:Get*", "b")},
		{allow("tb/*", "s3:*", "b/*"), {effect: "Deny", principal: "tb/bob", notPrincipal: true, action: "s3:*", resource: "b/*"}},
		// a Deny naming tb grants it nothing and shows it nothing
		{deny("tb/bob", "s3:*", "b/*")},
		// deleting is one action and bypassing governance retention another
		{allow("tb/bob", "s3:DeleteObject*", "b/*")},
		{allow("tb/*", "s3:*", "b/*"), deny("*", "s3:BypassGovernanceRetention", "b/priv/*")},
	}
	seen := map[string]bool{}
	for _, op := range s3op.Operations() {
		for _, a := range op.Authorizations {
			for _, action := range []string{a.Action, a.VersionAction} {
				if action == "" || seen[action] {
					continue
				}
				seen[action] = true
				perAction = append(perAction, []isoStatement{allow("tb/bob", action, "b"), allow("tb/bob", action, "b/*")})
			}
		}
	}
	return worlds, perAction
}

// TestOperationsEnforceIsolation sends every operation of the catalog, as
// every user, to every world, and requires of each response:
//
//   - the request passed authorization exactly when the specification
//     admits it, with the actions the catalog lists, and was answered
//     AccessDenied otherwise;
//   - nothing reached the backend before that point;
//   - to a requester the bucket is not visible to, the response is byte for
//     byte the one the same request gets when the bucket does not exist.
func TestOperationsEnforceIsolation(t *testing.T) {
	routed := s3gw.RouteOperations()
	routed = append(routed, s3op.OpListBuckets, s3op.OpPostObject)
	for _, name := range routed {
		if _, ok := isoRoutes[name]; !ok {
			t.Errorf("operation %s is routed and has no request in isoRoutes", name)
		}
	}

	now := time.Now()
	var requests []*isoSigned
	for name := range isoRoutes {
		op, ok := s3op.Lookup(name)
		if !ok {
			t.Fatalf("isoRoutes names %s, which is not in the catalog", name)
		}
		for _, r := range isoRequests(op) {
			for _, u := range isoUsers {
				requests = append(requests, signIsoRequest(t, u, r, now))
			}
		}
	}

	// what the other buckets grant is fixed: each lets anyone read it, so a
	// copy from another tenant's bucket is refused for being another
	// tenant's, not for being unreadable
	others := isoWorld{
		"b2": {owner: "ta", policy: []isoStatement{{effect: "Allow", principal: "*", action: "s3:GetObject*", resource: "b2/*"}}},
		"c":  {owner: "tb", policy: []isoStatement{{effect: "Allow", principal: "*", action: "s3:GetObject*", resource: "c/*"}}},
	}
	worlds, perAction := isoWorldSets()
	run := func(statements []isoStatement, bobOnly bool) {
		t.Run(isoPolicyText(statements), func(t *testing.T) {
			t.Parallel()
			absent := newIsoGateway(t, others)
			world := isoWorld{"b": {owner: "ta", policy: statements}}
			maps.Copy(world, others)
			g := newIsoGateway(t, world)
			text := isoPolicyText(statements)
			for _, s := range requests {
				if bobOnly && s.user.name != "bob" {
					continue
				}
				u, r := s.user, s.req
				fail := func(format string, args ...any) {
					t.Helper()
					t.Fatalf("%s as %s under %q: "+format, append([]any{r, u.principal(), text}, args...)...)
				}
				rec := g.serve(s)
				body := rec.Body.String()

				if g.route != r.op.Name {
					fail("routed to %q", g.route)
				}
				if !reflect.ValueOf(*g.stub).IsZero() {
					fail("the backend was called before the hooks admitted the request")
				}
				if strings.Contains(body, "backend-") {
					fail("the response names a backend bucket: %s", body)
				}

				if r.op.Scope == s3op.ScopeService {
					// no bucket: the user policy alone decides
					checkIsoOutcome(fail, g, rec, u, r, specUserAllows(u, s3op.ActionListAllMyBuckets), "")
					continue
				}
				checkIsoOutcome(fail, g, rec, u, r, world.admitted(u, "b", r), "ta")

				if !world.visible(u, "b") {
					twin := absent.serve(s)
					if rec.Code != twin.Code || !reflect.DeepEqual(rec.Header(), twin.Header()) || body != twin.Body.String() {
						fail("a hidden bucket answers differently from a missing one:\n hidden: %d %v %s\nmissing: %d %v %s",
							rec.Code, rec.Header(), body, twin.Code, twin.Header(), twin.Body)
					}
				}
			}
		})
	}
	for _, statements := range worlds {
		run(statements, false)
	}
	// only bob's grant differs between these, so only bob asks
	for _, statements := range perAction {
		run(statements, true)
	}
	t.Logf("%d worlds for %d requests, %d more for the %d of tb/bob", len(worlds), len(requests), len(perAction), len(requests)/len(isoUsers))
}

func checkIsoOutcome(fail func(string, ...any), g *isoGateway, rec *httptest.ResponseRecorder, u isoUser, r isoRequest, admitted bool, owner string) {
	op := g.auth.op
	if !admitted {
		if op != nil {
			fail("passed authorization, which the specification refuses")
		}
		if rec.Code != http.StatusForbidden {
			fail("refused with %d, want 403: %s", rec.Code, rec.Body)
		}
		if rec.Body.Len() > 0 && !strings.Contains(rec.Body.String(), "<Code>AccessDenied</Code>") {
			fail("refused with %s", rec.Body)
		}
		return
	}
	if op == nil || rec.Code != isoReached {
		fail("answered %d %s, which the specification admits", rec.Code, rec.Body)
	}
	want := r.targetActions()
	if a := r.copyAction(); a != "" {
		want = append(want, a)
	}
	if op.Operation != r.op.Name || op.Tenant != u.tenant || op.User != u.name || op.BucketOwner != owner ||
		!slices.Equal(op.Actions, want) {
		fail("the hooks were given %s by %s/%s on %s's bucket with %v, want %v",
			op.Operation, op.Tenant, op.User, op.BucketOwner, op.Actions, want)
	}
}

// TestDeleteObjectsEnforcesIsolationPerKey: DeleteObjects is authorized key
// by key, after the hooks, for the delete action of each entry and, when
// the request carries the bypass header, for that action too. Only the keys
// the specification permits reach the backend, each of the others is
// answered AccessDenied.
func TestDeleteObjectsEnforcesIsolationPerKey(t *testing.T) {
	entries := []struct {
		key, version string
	}{
		{"pub/k", ""}, {"priv/k", ""}, {"pub/v", "v1"}, {"priv/v", "v1"},
	}
	var doc strings.Builder
	doc.WriteString("<Delete>")
	for _, e := range entries {
		doc.WriteString("<Object><Key>" + e.key + "</Key>")
		if e.version != "" {
			doc.WriteString("<VersionId>" + e.version + "</VersionId>")
		}
		doc.WriteString("</Object>")
	}
	doc.WriteString("</Delete>")
	body := []byte(doc.String())
	hash, sum := sha256.Sum256(body), md5.Sum(body)

	now := time.Now()
	for _, statements := range isoWorlds() {
		world := isoWorld{"b": {owner: "ta", policy: statements}}
		g := newIsoGateway(t, world)
		text := isoPolicyText(statements)
		for _, u := range isoUsers {
			for _, bypass := range []bool{false, true} {
				req := signedRequest(t, "POST", "http://s3.example.com/b?delete", body, hex.EncodeToString(hash[:]), now,
					aws.Credentials{AccessKeyID: u.keyID(), SecretAccessKey: u.secret()},
					func(req *http.Request) {
						req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
						if bypass {
							req.Header.Set("x-amz-bypass-governance-retention", "true")
						}
					})
				s := &isoSigned{method: "POST", target: "http://s3.example.com/b?delete", header: req.Header, body: body}
				*g.stub = stubBackend{delObjsOut: &s3.DeleteObjectsOutput{}}
				g.auth.op, g.auth.admit = nil, true
				rec := httptest.NewRecorder()
				g.gw.Handler().ServeHTTP(rec, s.request())

				var want []string
				for _, e := range entries {
					action := s3op.ActionDeleteObject
					if e.version != "" {
						action = s3op.ActionDeleteObjectVersion
					}
					// the header asks for one more action on every key
					if world.permits(u, "b", action, "b/"+e.key) &&
						(!bypass || world.permits(u, "b", s3op.ActionBypassGovernanceRetention, "b/"+e.key)) {
						want = append(want, e.key)
					}
				}
				var got []string
				if in := g.stub.delObjsIn; in != nil {
					for _, o := range in.Delete.Objects {
						got = append(got, aws.ToString(o.Key))
					}
				}
				if !slices.Equal(got, want) {
					t.Fatalf("%s (bypass %v) under %q: the backend was asked to delete %v, the specification permits %v", u.principal(), bypass, text, got, want)
				}
				if !world.visible(u, "b") {
					if rec.Code != http.StatusForbidden {
						t.Fatalf("%s (bypass %v) under %q: hidden bucket answered %d", u.principal(), bypass, text, rec.Code)
					}
					continue
				}
				if denied := strings.Count(rec.Body.String(), "<Code>AccessDenied</Code>"); rec.Code != http.StatusOK || denied != len(entries)-len(want) {
					t.Fatalf("%s (bypass %v) under %q: %d with %d keys denied, want %d: %s", u.principal(), bypass, text, rec.Code, denied, len(entries)-len(want), rec.Body)
				}
			}
		}
	}
}

// TestListBucketsShowsOwnTenantOnly: whatever another tenant's bucket
// grants, it is not listed.
func TestListBucketsShowsOwnTenantOnly(t *testing.T) {
	world := isoWorld{
		"b": {owner: "ta", policy: []isoStatement{{effect: "Allow", principal: "*", action: "s3:*", resource: "b/*"}}},
		"c": {owner: "tb", policy: []isoStatement{{effect: "Allow", principal: "*", action: "s3:*", resource: "c/*"}}},
	}
	g := newIsoGateway(t, world)
	g.auth.admit = true
	now := time.Now()
	op, _ := s3op.Lookup(s3op.OpListBuckets)
	for _, u := range isoUsers {
		if !specUserAllows(u, s3op.ActionListAllMyBuckets) {
			continue
		}
		rec := httptest.NewRecorder()
		g.gw.Handler().ServeHTTP(rec, signIsoRequest(t, u, isoRequest{op: op}, now).request())
		body := rec.Body.String()
		for name, b := range world {
			if listed := strings.Contains(body, "<Name>"+name+"</Name>"); listed != (b.owner == u.tenant) {
				t.Errorf("%s: bucket %s of %s listed = %v: %s", u.principal(), name, b.owner, listed, body)
			}
		}
	}
}
