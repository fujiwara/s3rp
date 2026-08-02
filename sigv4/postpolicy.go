package sigv4

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/fujiwara/s3rp/s3err"
)

// Browser-based POST uploads are the third SigV4 mechanism next to the
// Authorization header and presigned URLs: the credential holder signs a
// POST policy document, and the browser submits it as form fields together
// with the file. The signature covers the base64 policy string, so
// verification is a single HMAC with the scope-derived signing key; the
// conditions of the policy then constrain what the form may contain.
// Specified in "Browser-Based Uploads Using POST (AWS Signature Version 4)"
// in the S3 API reference (cited by title: AWS's deep links to it rot).

const (
	// maxPostPolicyBytes caps the decoded policy document, matching AWS's
	// POST policy size limit (20 KB). Like bucket policies, POST policies
	// arrive from the network and must be bounded before parsing.
	maxPostPolicyBytes = 20 * 1024
	// maxPostPolicyConditions caps the conditions of one policy document.
	maxPostPolicyConditions = 100
)

// PostPolicy carries the verified facts of a POST policy that the caller
// still has to act on: the deadline it declared, and the file size range it
// permits — the file's size is not known at verification time, so the
// caller enforces the range while reading it.
type PostPolicy struct {
	Expiration time.Time
	// MinLength and MaxLength are the content-length-range bounds
	// (0 and math.MaxInt64 when the policy has no range condition).
	MinLength, MaxLength int64
}

// postCondition is one eq/starts-with condition, normalized: field names
// are lower-cased and stripped of the "$" form-field reference.
type postCondition struct {
	op    string
	field string
	value string
}

// VerifyPost authenticates a browser-based POST upload from its form
// fields. The map holds every form field before the file part, keys
// lower-cased (POST form field names are case-insensitive); the caller adds
// the target "bucket" as a pseudo-field so the policy binds the bucket, and
// substitutes ${filename} into the key before calling. The signature is
// verified before the policy document is parsed, so an unsigned document is
// never unmarshaled. Conditions are then evaluated both ways: every
// condition must hold, and every submitted field (except the policy and the
// signature themselves) must be covered by a condition, as AWS requires.
func (v *Verifier) VerifyPost(r *http.Request, fields map[string]string, lookup SecretLookup) (*Verified, *PostPolicy, *s3err.Error) {
	invalid := func(format string, args ...any) *s3err.Error {
		return s3err.New(http.StatusBadRequest, "InvalidArgument", fmt.Sprintf(format, args...))
	}
	if alg := fields["x-amz-algorithm"]; alg != sigV4Algorithm {
		return nil, nil, invalid("X-Amz-Algorithm only supports %q", sigV4Algorithm)
	}
	credElems := strings.Split(fields["x-amz-credential"], "/")
	if len(credElems) != 5 || credElems[4] != "aws4_request" {
		return nil, nil, invalid("Error parsing the X-Amz-Credential form field; the Credential is mal-formed")
	}
	akid, scopeDate, region, service := credElems[0], credElems[1], credElems[2], credElems[3]
	if service != "s3" {
		return nil, nil, invalid("Error parsing the X-Amz-Credential form field; incorrect service")
	}
	if v.region != "" && region != v.region {
		return nil, nil, invalid("Error parsing the X-Amz-Credential form field; the region '%s' is wrong; expecting '%s'", region, v.region)
	}
	t, err := time.Parse(amzDateFormat, fields["x-amz-date"])
	if err != nil {
		return nil, nil, invalid("X-Amz-Date must be in the ISO8601 Long Format")
	}
	if t.Format("20060102") != scopeDate {
		return nil, nil, invalid("Invalid credential date. Date is not the same as X-Amz-Date.")
	}
	now := v.now()
	if t.After(now.Add(maxClockSkew)) {
		return nil, nil, s3err.New(http.StatusForbidden, "AccessDenied", "Request is not valid yet")
	}
	policyB64 := fields["policy"]
	if policyB64 == "" {
		return nil, nil, invalid("Bucket POST must contain a field named 'policy'.")
	}
	// bound the base64 form before the decoded document is bounded below
	if len(policyB64) > maxPostPolicyBytes*4/3+4 {
		return nil, nil, invalid("the policy document exceeds the maximum allowed size (%d bytes)", maxPostPolicyBytes)
	}
	sig := fields["x-amz-signature"]
	if sig == "" {
		return nil, nil, invalid("Bucket POST must contain a field named 'x-amz-signature'.")
	}

	secret, s3e := lookupSecret(r, lookup, akid)
	if s3e != nil {
		return nil, nil, s3e
	}
	key := deriveSigningKey(secret, scopeDate, region)
	want := hex.EncodeToString(hmacSHA256(key, []byte(policyB64)))
	if subtle.ConstantTimeCompare([]byte(want), []byte(sig)) != 1 {
		return nil, nil, s3err.SignatureDoesNotMatch().WithCause(fmt.Errorf("post policy signature mismatch"))
	}

	// the document is proven to come from the key holder; now it may be parsed
	raw, err := base64.StdEncoding.DecodeString(policyB64)
	if err != nil {
		return nil, nil, invalid("the policy form field is not valid base64")
	}
	if len(raw) > maxPostPolicyBytes {
		return nil, nil, invalid("the policy document exceeds the maximum allowed size (%d bytes)", maxPostPolicyBytes)
	}
	pp, conds, s3e := parsePostPolicy(raw)
	if s3e != nil {
		return nil, nil, s3e
	}
	if now.After(pp.Expiration) {
		return nil, nil, s3err.New(http.StatusForbidden, "AccessDenied", "Invalid according to Policy: Policy expired.")
	}
	if s3e := evaluatePostPolicy(conds, fields); s3e != nil {
		return nil, nil, s3e
	}
	return &Verified{
		AccessKeyID:     akid,
		SecretAccessKey: secret,
		Signature:       sig,
		SigningTime:     t,
		Scope:           strings.Join([]string{scopeDate, region, service, "aws4_request"}, "/"),
		Region:          region,
		PayloadHash:     "UNSIGNED-PAYLOAD",
	}, pp, nil
}

func invalidPolicyDocument(format string, args ...any) *s3err.Error {
	return s3err.New(http.StatusBadRequest, "InvalidPolicyDocument",
		"Invalid Policy: "+fmt.Sprintf(format, args...))
}

// parsePostPolicy parses the decoded policy JSON into the caller-facing
// facts and the conditions to evaluate. content-length-range conditions
// fold into the returned PostPolicy (intersected when repeated).
func parsePostPolicy(raw []byte) (*PostPolicy, []postCondition, *s3err.Error) {
	var doc struct {
		Expiration string            `json:"expiration"`
		Conditions []json.RawMessage `json:"conditions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, invalidPolicyDocument("malformed JSON: %v", err)
	}
	if doc.Expiration == "" {
		return nil, nil, invalidPolicyDocument("the 'expiration' element is required")
	}
	exp, err := time.Parse(time.RFC3339, doc.Expiration)
	if err != nil {
		return nil, nil, invalidPolicyDocument("malformed 'expiration': %v", err)
	}
	if len(doc.Conditions) > maxPostPolicyConditions {
		return nil, nil, invalidPolicyDocument("%d conditions, at most %d are allowed", len(doc.Conditions), maxPostPolicyConditions)
	}
	pp := &PostPolicy{Expiration: exp, MaxLength: math.MaxInt64}
	var conds []postCondition
	for _, rc := range doc.Conditions {
		trimmed := strings.TrimSpace(string(rc))
		switch {
		case strings.HasPrefix(trimmed, "{"):
			// {"field": "value"} is shorthand for ["eq", "$field", "value"]
			var obj map[string]string
			if err := json.Unmarshal(rc, &obj); err != nil {
				return nil, nil, invalidPolicyDocument("malformed condition %s", trimmed)
			}
			for k, v := range obj {
				conds = append(conds, postCondition{op: "eq", field: strings.ToLower(k), value: v})
			}
		case strings.HasPrefix(trimmed, "["):
			cond, rng, s3e := parseArrayCondition(rc)
			if s3e != nil {
				return nil, nil, s3e
			}
			if rng != nil {
				pp.MinLength = max(pp.MinLength, rng[0])
				pp.MaxLength = min(pp.MaxLength, rng[1])
			} else {
				conds = append(conds, cond)
			}
		default:
			return nil, nil, invalidPolicyDocument("malformed condition %s", trimmed)
		}
	}
	if pp.MinLength > pp.MaxLength {
		return nil, nil, invalidPolicyDocument("empty content-length-range")
	}
	return pp, conds, nil
}

// parseArrayCondition parses ["eq"|"starts-with", "$field", "value"] or
// ["content-length-range", min, max] (returned as rng).
func parseArrayCondition(rc json.RawMessage) (postCondition, *[2]int64, *s3err.Error) {
	var elems []json.RawMessage
	if err := json.Unmarshal(rc, &elems); err != nil || len(elems) != 3 {
		return postCondition{}, nil, invalidPolicyDocument("malformed condition %s", string(rc))
	}
	var op string
	if err := json.Unmarshal(elems[0], &op); err != nil {
		return postCondition{}, nil, invalidPolicyDocument("malformed condition %s", string(rc))
	}
	switch strings.ToLower(op) {
	case "eq", "starts-with":
		var field, value string
		if err := json.Unmarshal(elems[1], &field); err != nil || !strings.HasPrefix(field, "$") {
			return postCondition{}, nil, invalidPolicyDocument("condition field must reference a form field with $: %s", string(rc))
		}
		if err := json.Unmarshal(elems[2], &value); err != nil {
			return postCondition{}, nil, invalidPolicyDocument("malformed condition %s", string(rc))
		}
		return postCondition{op: strings.ToLower(op), field: strings.ToLower(field[1:]), value: value}, nil, nil
	case "content-length-range":
		var minLen, maxLen int64
		if json.Unmarshal(elems[1], &minLen) != nil || json.Unmarshal(elems[2], &maxLen) != nil ||
			minLen < 0 || maxLen < minLen {
			return postCondition{}, nil, invalidPolicyDocument("malformed content-length-range %s", string(rc))
		}
		return postCondition{}, &[2]int64{minLen, maxLen}, nil
	default:
		// an unknown condition type must not silently pass
		return postCondition{}, nil, invalidPolicyDocument("unknown condition type %q", op)
	}
}

// evaluatePostPolicy checks the conditions both ways: every condition must
// hold against the form fields, and every submitted field must be covered
// by a condition — except the policy and signature themselves, which cannot
// meaningfully constrain themselves.
func evaluatePostPolicy(conds []postCondition, fields map[string]string) *s3err.Error {
	denied := func(format string, args ...any) *s3err.Error {
		return s3err.New(http.StatusForbidden, "AccessDenied",
			"Invalid according to Policy: "+fmt.Sprintf(format, args...))
	}
	covered := make(map[string]bool, len(conds))
	for _, c := range conds {
		covered[c.field] = true
		v, ok := fields[c.field]
		if !ok {
			return denied("Policy Condition failed: %q on missing field %q", c.op, c.field)
		}
		switch c.op {
		case "eq":
			if v != c.value {
				return denied("Policy Condition failed: [%q, %q, %q]", "eq", "$"+c.field, c.value)
			}
		case "starts-with":
			if !strings.HasPrefix(v, c.value) {
				return denied("Policy Condition failed: [%q, %q, %q]", "starts-with", "$"+c.field, c.value)
			}
		}
	}
	for name := range fields {
		switch name {
		case "policy", "x-amz-signature":
			continue
		}
		if !covered[name] {
			return denied("Extra input fields: %s", name)
		}
	}
	return nil
}
