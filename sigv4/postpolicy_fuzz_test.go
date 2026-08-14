package sigv4_test

import (
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// Fuzz targets for browser-based POST uploads. The policy document is only
// parsed after its signature verifies, so raw fuzzing never reaches the
// parser — these targets sign what they feed it, which is exactly what a
// credential holder can do. The properties:
//
//   - Completeness: a policy signed with the key, whose conditions cover and
//     match every submitted field, verifies.
//   - Soundness: mutating the signature, the document, a constrained value
//     or the field set is refused.
//   - Two-way evaluation: a form field no condition covers is refused
//     whatever the (correctly signed) document says — the guarantee that a
//     signed policy cannot be replayed with extra fields bolted on.

const (
	fuzzPostDate    = "20260801"
	fuzzPostAmzDate = "20260801T120000Z"
	fuzzPostRegion  = "us-east-1"
)

// fuzzPostFieldName reduces arbitrary input to a usable form field name:
// lower-cased (the caller lower-cases them, so a condition could never match
// otherwise) and never one of the fields carrying the authentication itself,
// which the driver sets.
func fuzzPostFieldName(s string) string {
	n := strings.ToLower(keepBytes(s, func(b byte) bool { return 0x21 <= b && b <= 0x7e }, 40))
	switch n {
	case "", "policy", "x-amz-signature", "x-amz-credential", "x-amz-algorithm",
		"x-amz-date", "x-amz-security-token":
		return ""
	}
	return n
}

// fuzzPostValue keeps values printable ASCII so they survive a JSON round
// trip byte for byte: invalid UTF-8 would be replaced on marshal, and the
// policy would then no longer say what the form says — a driver artifact,
// not a property of the verifier.
func fuzzPostValue(s string) string {
	return keepBytes(s, func(b byte) bool { return 0x20 <= b && b <= 0x7e }, 128)
}

// buildPostPolicy writes a policy covering exactly the given fields, with a
// starts-with condition (on the first half of the value) for the fields
// selected by startsWith and an eq condition for the rest. Keys are sorted
// so one fuzz input always produces one document: a crasher must reproduce.
func buildPostPolicy(fields map[string]string, startsWith map[string]bool, expiration time.Time) string {
	conds := make([]any, 0, len(fields))
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		switch k {
		case "policy", "x-amz-signature":
			continue
		}
		v := fields[k]
		if startsWith[k] {
			conds = append(conds, []any{"starts-with", "$" + k, v[:len(v)/2]})
		} else {
			conds = append(conds, map[string]string{k: v})
		}
	}
	doc, err := json.Marshal(map[string]any{
		"expiration": expiration.Format(time.RFC3339),
		"conditions": conds,
	})
	if err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(doc)
}

// signPostFields adds the policy built from the form's own fields and the
// signature over it.
func signPostFields(fields map[string]string, startsWith map[string]bool, expiration time.Time) {
	b64 := buildPostPolicy(fields, startsWith, expiration)
	fields["policy"] = b64
	fields["x-amz-signature"] = signPostPolicy(testSecret, fuzzPostDate, fuzzPostRegion, b64)
}

// authPostFields is the authentication the driver always supplies, matching
// testTime and the key the shared lookup knows.
func authPostFields() map[string]string {
	return map[string]string{
		"x-amz-credential": testAccessKeyID + "/" + fuzzPostDate + "/" + fuzzPostRegion + "/s3/aws4_request",
		"x-amz-algorithm":  "AWS4-HMAC-SHA256",
		"x-amz-date":       fuzzPostAmzDate,
	}
}

func FuzzVerifyPostRoundtrip(f *testing.F) {
	f.Add("key", "user/uploads/photo.jpg", "content-type", "image/jpeg", uint8(0), uint8(0), 0, uint8(0))
	f.Add("x-amz-meta-note", `he said "hi" \ bye`, "success_action_status", "201", uint8(3), uint8(1), 3, uint8(2))
	f.Add("$weird", "", "bucket", "photos", uint8(1), uint8(4), -7, uint8(5))
	f.Add("key", "a", "acl", "private", uint8(2), uint8(2), 11, uint8(6))

	f.Fuzz(func(t *testing.T, n1, v1, n2, v2 string, sel, mutSel uint8, mutPos int, mutBit uint8) {
		fields := authPostFields()
		startsWith := map[string]bool{}
		var userFields []string
		for i, nv := range [][2]string{{n1, v1}, {n2, v2}} {
			name := fuzzPostFieldName(nv[0])
			if name == "" {
				continue
			}
			if _, dup := fields[name]; dup {
				continue
			}
			fields[name] = fuzzPostValue(nv[1])
			if sel&(1<<uint(i)) != 0 {
				startsWith[name] = true
			}
			userFields = append(userFields, name)
		}
		if sel&0x40 != 0 {
			startsWith["x-amz-credential"] = true
		}
		expiration := testTime.Add(time.Hour)
		signPostFields(fields, startsWith, expiration)

		r := httptest.NewRequest("POST", "http://s3.example.com/bucket", nil)

		// completeness: a form whose every field is covered and satisfied
		// by the signed policy must verify
		got, pp, s3e := postVerifier(testTime).VerifyPost(r, maps.Clone(fields), lookup)
		if s3e != nil {
			t.Fatalf("self-signed, self-covering form refused: %v (fields %v)", s3e, slices.Sorted(maps.Keys(fields)))
		}
		if got.AccessKeyID != testAccessKeyID || got.Region != fuzzPostRegion {
			t.Fatalf("verified facts do not match what was signed: %+v", got)
		}
		if !pp.Expiration.Equal(expiration) {
			t.Fatalf("expiration %v, want %v", pp.Expiration, expiration)
		}

		// soundness: every mutation below changes what was signed, what the
		// conditions constrain, or the field set they must cover
		mut := maps.Clone(fields)
		switch mutSel % 6 {
		case 0:
			mut["x-amz-signature"] = flipBit(mut["x-amz-signature"], mutPos, mutBit)
		case 1:
			mut["policy"] = flipBit(mut["policy"], mutPos, mutBit)
		case 2:
			// a field no condition covers: the two-way rule must refuse it
			mut["zz-uncovered"] = "anything"
		case 3:
			// break a value an eq condition pins; starts-with fields and the
			// empty value are skipped, appending cannot break either
			var target string
			for _, n := range userFields {
				if !startsWith[n] {
					target = n
					break
				}
			}
			if target == "" {
				return
			}
			mut[target] += "!"
		case 4:
			// drop a field a condition references
			if len(userFields) == 0 {
				return
			}
			delete(mut, userFields[mutPos2(mutPos, len(userFields))])
		case 5:
			mut["x-amz-credential"] = flipBit(mut["x-amz-credential"], mutPos, mutBit)
		}
		if _, _, s3e := postVerifier(testTime).VerifyPost(r, mut, lookup); s3e == nil {
			t.Fatalf("mutation %d accepted (fields %v)", mutSel%6, slices.Sorted(maps.Keys(mut)))
		}
	})
}

func mutPos2(pos, n int) int {
	p := pos % n
	if p < 0 {
		p += n
	}
	return p
}

// FuzzVerifyPostDocument signs an arbitrary document, which is the only way
// to reach the policy parser at all, and asserts what must hold however the
// document reads: an uncovered form field is always refused, the parser
// never panics, and a verified policy always reports a usable length range.
func FuzzVerifyPostDocument(f *testing.F) {
	f.Add(`{"expiration":"2026-08-01T13:00:00Z","conditions":[{"key":"a"}]}`)
	f.Add(`{"expiration":"2026-08-01T13:00:00Z","conditions":[["starts-with","$key",""],["content-length-range",0,100]]}`)
	f.Add(`{"expiration":"2026-08-01T13:00:00Z","conditions":[["content-length-range",-1,-2]]}`)
	f.Add(`{"conditions":[]}`)
	f.Add(`{"expiration":"2026-08-01T13:00:00Z","conditions":[["unknown","$key","a"]]}`)
	f.Add(`[]`)
	f.Add(``)

	f.Fuzz(func(t *testing.T, doc string) {
		if len(doc) > 32*1024 {
			doc = doc[:32*1024]
		}
		b64 := base64.StdEncoding.EncodeToString([]byte(doc))
		base := authPostFields()
		base["policy"] = b64
		base["x-amz-signature"] = signPostPolicy(testSecret, fuzzPostDate, fuzzPostRegion, b64)
		r := httptest.NewRequest("POST", "http://s3.example.com/bucket", nil)

		// whatever the document says, a field it cannot have covered is
		// refused: a signed policy must not be replayable with extras
		withExtra := maps.Clone(base)
		withExtra["zz-uncovered"] = "anything"
		if _, _, s3e := postVerifier(testTime).VerifyPost(r, withExtra, lookup); s3e == nil {
			t.Fatalf("uncovered field accepted for document %q", doc)
		}

		// and the plain form must not panic; if it verifies, the range it
		// reports has to be usable
		_, pp, s3e := postVerifier(testTime).VerifyPost(r, base, lookup)
		if s3e == nil && pp.MinLength > pp.MaxLength {
			t.Fatalf("verified policy reports an empty range [%d, %d] for document %q",
				pp.MinLength, pp.MaxLength, doc)
		}
	})
}

// FuzzVerifyPostAdversarial submits form fields nobody signed: nothing may
// verify, and nothing may panic.
func FuzzVerifyPostAdversarial(f *testing.F) {
	f.Add("AWS4-HMAC-SHA256", testAccessKeyID+"/20260801/us-east-1/s3/aws4_request",
		fuzzPostAmzDate, "eyJleHBpcmF0aW9uIjoiMjAyNi0wOC0wMVQxMzowMDowMFoifQ==", "0", "token")
	f.Add("", "", "", "", "", "")
	f.Add("AWS4-HMAC-SHA256", "a/b/c/d/aws4_request", "not-a-date", "!!!", strings.Repeat("f", 64), "")

	f.Fuzz(func(t *testing.T, alg, cred, date, policy, sig, token string) {
		fields := map[string]string{
			"x-amz-algorithm":  fuzzPostValue(alg),
			"x-amz-credential": fuzzPostValue(cred),
			"x-amz-date":       fuzzPostValue(date),
			"policy":           fuzzPostValue(policy),
			"x-amz-signature":  fuzzPostValue(sig),
		}
		if token != "" {
			fields["x-amz-security-token"] = fuzzPostValue(token)
		}
		r := httptest.NewRequest("POST", "http://s3.example.com/bucket", nil)
		if got, _, s3e := postVerifier(testTime).VerifyPost(r, fields, lookup); s3e == nil {
			t.Fatalf("unsigned form verified as %+v", got)
		}
	})
}
