package s3gw_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/store"
)

// stubPost is a backend that answers PutObject and DeleteObject, consuming
// the body the way the real SDK does — so a body-read error (the
// content-length-range bound) surfaces exactly as it would in production.
type stubPost struct {
	s3gw.BackendClient
	putIn   *s3.PutObjectInput
	putOut  *s3.PutObjectOutput // nil = a default output
	putBody []byte
	delIn   *s3.DeleteObjectInput
}

func (s *stubPost) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	s.putIn = in
	b, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	s.putBody = b
	if s.putOut != nil {
		return s.putOut, nil
	}
	return &s3.PutObjectOutput{
		ETag:      aws.String(`"post-etag"`),
		VersionId: aws.String("v1"),
	}, nil
}

func (s *stubPost) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	s.delIn = in
	return &s3.DeleteObjectOutput{}, nil
}

// signPost signs a POST policy the way a service backend does, from first
// principles.
func signPost(secret, date, region, policyB64 string) string {
	h := func(key []byte, s string) []byte {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(s))
		return m.Sum(nil)
	}
	k := h([]byte("AWS4"+secret), date)
	k = h(k, region)
	k = h(k, "s3")
	k = h(k, "aws4_request")
	return hex.EncodeToString(h(k, policyB64))
}

// postForm builds a browser-style multipart upload form with a signed
// policy. The auth conditions and fields are added automatically; extra
// conditions and fields come from the test.
type postForm struct {
	conditions []string    // raw JSON conditions, beyond the auth ones
	fields     [][2]string // ordered extra fields (mixed case, like a real form)
	filename   string
	content    string
}

func (f *postForm) request(t *testing.T) *http.Request {
	t.Helper()
	now := time.Now().UTC()
	scopeDate := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")
	cred := testAccessKeyID + "/" + scopeDate + "/us-east-1/s3/aws4_request"
	conds := append([]string{
		`{"bucket": "testbucket"}`,
		`{"x-amz-algorithm": "AWS4-HMAC-SHA256"}`,
		`{"x-amz-credential": "` + cred + `"}`,
		`{"x-amz-date": "` + amzDate + `"}`,
	}, f.conditions...)
	doc := `{"expiration": "` + now.Add(time.Hour).Format("2006-01-02T15:04:05.000Z") +
		`", "conditions": [` + strings.Join(conds, ",") + `]}`
	b64 := base64.StdEncoding.EncodeToString([]byte(doc))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	// mixed-case names exercise the case-insensitivity of POST fields
	mw.WriteField("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	mw.WriteField("X-Amz-Credential", cred)
	mw.WriteField("X-Amz-Date", amzDate)
	mw.WriteField("Policy", b64)
	mw.WriteField("X-Amz-Signature", signPost(testSecretAccessKey, scopeDate, "us-east-1", b64))
	for _, kv := range f.fields {
		mw.WriteField(kv[0], kv[1])
	}
	fw, err := mw.CreateFormFile("file", f.filename)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte(f.content))
	// a field after the file is ignored, as on AWS
	mw.WriteField("submit", "Upload")
	mw.Close()

	req := httptest.NewRequest("POST", "http://s3.example.com/testbucket", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func TestPostObject(t *testing.T) {
	gw := newTestGateway(t)
	stub := &stubPost{}
	if err := gw.SetBackend("testbucket", stub); err != nil {
		t.Fatal(err)
	}
	var ops []*s3gw.Op
	gw.Use(func(ctx context.Context, op *s3gw.Op, next func() error) error {
		err := next()
		ops = append(ops, op)
		return err
	})

	form := &postForm{
		conditions: []string{
			`["starts-with", "$key", "user/"]`,
			`["starts-with", "$Content-Type", "text/"]`,
			`{"x-amz-meta-color": "blue"}`,
			`{"success_action_status": "201"}`,
		},
		fields: [][2]string{
			{"key", "user/${filename}"},
			{"Content-Type", "text/plain"},
			{"x-amz-meta-color", "blue"},
			{"success_action_status", "201"},
			{"x-ignore-note", "not signed, not forwarded"},
		},
		filename: "hello.txt",
		content:  "hello post",
	}
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, form.request(t))

	if w.Code != http.StatusCreated {
		t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// the response names the front bucket and the substituted key
	for _, want := range []string{`<PostResponse xmlns="`, "<Bucket>testbucket</Bucket>", "<Key>user/hello.txt</Key>", "post-etag"} {
		if !strings.Contains(body, want) {
			t.Errorf("expect %s in the 201 body: %s", want, body)
		}
	}
	if got := w.Header().Get("ETag"); got != `"post-etag"` {
		t.Errorf("unexpected ETag %q", got)
	}
	if stub.putIn == nil {
		t.Fatal("expect the upload to reach the backend")
	}
	if aws.ToString(stub.putIn.Bucket) != "backend-testbucket" || aws.ToString(stub.putIn.Key) != "user/hello.txt" {
		t.Errorf("unexpected backend input %s/%s", aws.ToString(stub.putIn.Bucket), aws.ToString(stub.putIn.Key))
	}
	if aws.ToString(stub.putIn.ContentType) != "text/plain" {
		t.Errorf("unexpected content type %v", stub.putIn.ContentType)
	}
	if stub.putIn.Metadata["color"] != "blue" {
		t.Errorf("expect the x-amz-meta-* field as metadata, got %v", stub.putIn.Metadata)
	}
	if string(stub.putBody) != "hello post" {
		t.Errorf("unexpected body %q", stub.putBody)
	}
	if len(ops) != 1 {
		t.Fatalf("expect the hooks to run once, got %d", len(ops))
	}
	op := ops[0]
	if op.Method != "POST" || op.Action != "s3:PutObject" || op.Bucket != "testbucket" || op.Key != "user/hello.txt" {
		t.Errorf("unexpected op %+v", op)
	}
	if op.Tenant != "testtenant" || op.User != "testuser" {
		t.Errorf("expect the identity on the op, got %+v", op)
	}
	if op.BytesIn <= int64(len("hello post")) {
		t.Errorf("expect the form framing to be counted, got %d", op.BytesIn)
	}
}

func TestPostObjectDefaultStatusAndRedirect(t *testing.T) {
	t.Run("default 204", func(t *testing.T) {
		gw := newTestGateway(t)
		stub := &stubPost{}
		if err := gw.SetBackend("testbucket", stub); err != nil {
			t.Fatal(err)
		}
		form := &postForm{
			conditions: []string{`{"key": "a.txt"}`},
			fields:     [][2]string{{"key", "a.txt"}},
			filename:   "x", content: "data",
		}
		w := httptest.NewRecorder()
		gw.Handler().ServeHTTP(w, form.request(t))
		if w.Code != http.StatusNoContent {
			t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
		}
		if w.Header().Get("ETag") == "" {
			t.Error("expect an ETag header")
		}
	})

	t.Run("redirect", func(t *testing.T) {
		gw := newTestGateway(t)
		stub := &stubPost{}
		if err := gw.SetBackend("testbucket", stub); err != nil {
			t.Fatal(err)
		}
		form := &postForm{
			conditions: []string{
				`{"key": "a.txt"}`,
				`{"success_action_redirect": "https://app.example.com/done"}`,
			},
			fields: [][2]string{
				{"key", "a.txt"},
				{"success_action_redirect", "https://app.example.com/done"},
			},
			filename: "x", content: "data",
		}
		w := httptest.NewRecorder()
		gw.Handler().ServeHTTP(w, form.request(t))
		if w.Code != http.StatusSeeOther {
			t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
		}
		loc := w.Header().Get("Location")
		if !strings.HasPrefix(loc, "https://app.example.com/done?") ||
			!strings.Contains(loc, "bucket=testbucket") || !strings.Contains(loc, "key=a.txt") {
			t.Errorf("unexpected redirect location %q", loc)
		}
	})
}

func TestPostObjectContentLengthRange(t *testing.T) {
	t.Run("too large", func(t *testing.T) {
		gw := newTestGateway(t)
		stub := &stubPost{}
		if err := gw.SetBackend("testbucket", stub); err != nil {
			t.Fatal(err)
		}
		form := &postForm{
			conditions: []string{`{"key": "a.txt"}`, `["content-length-range", 1, 4]`},
			fields:     [][2]string{{"key", "a.txt"}},
			filename:   "x", content: "way past the limit",
		}
		w := httptest.NewRecorder()
		gw.Handler().ServeHTTP(w, form.request(t))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "EntityTooLarge") {
			t.Errorf("expect EntityTooLarge, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("too small deletes the stored object", func(t *testing.T) {
		gw := newTestGateway(t)
		stub := &stubPost{}
		if err := gw.SetBackend("testbucket", stub); err != nil {
			t.Fatal(err)
		}
		form := &postForm{
			conditions: []string{`{"key": "a.txt"}`, `["content-length-range", 100, 1000]`},
			fields:     [][2]string{{"key", "a.txt"}},
			filename:   "x", content: "tiny",
		}
		w := httptest.NewRecorder()
		gw.Handler().ServeHTTP(w, form.request(t))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "EntityTooSmall") {
			t.Errorf("expect EntityTooSmall, got %d: %s", w.Code, w.Body.String())
		}
		if stub.delIn == nil || aws.ToString(stub.delIn.Key) != "a.txt" {
			t.Fatal("expect the undersized object to be deleted from the backend")
		}
		// on a versioned bucket, the exact version just written must go —
		// a plain delete would only add a delete marker on top of it
		if aws.ToString(stub.delIn.VersionId) != "v1" {
			t.Errorf("expect the delete to target the written version, got %v", stub.delIn.VersionId)
		}
	})
}

func TestPostObjectRefusals(t *testing.T) {
	// a gateway whose bucket policy denies PutObject for the user
	denyingGateway := func(t *testing.T) (*s3gw.Gateway, *stubPost) {
		t.Helper()
		text := `{"Statement":[{"Effect":"Deny","Principal":{"S3RP":["testtenant/testuser"]},"Action":["s3:PutObject"],"Resource":["testbucket/*"]}]}`
		p, err := policy.Parse("testbucket", text)
		if err != nil {
			t.Fatal(err)
		}
		pathStyle := true
		gw := s3gw.New(memStore{
			keys: map[string]*store.Key{
				testAccessKeyID: {
					AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey,
					Tenant: "testtenant", User: "testuser",
				},
			},
			buckets: map[string]*store.Bucket{
				"testbucket": {
					Tenant: "testtenant", Name: "testbucket",
					Backend: &store.Backend{
						Endpoint: "http://backend.invalid", Region: "us-east-1",
						Bucket: "backend-testbucket", AccessKeyID: "bk", SecretAccessKey: "bs",
						UsePathStyle: &pathStyle,
					},
					PolicyText: text, Policy: p,
				},
			},
		})
		stub := &stubPost{}
		if err := gw.SetBackend("testbucket", stub); err != nil {
			t.Fatal(err)
		}
		return gw, stub
	}

	cases := []struct {
		name string
		gw   func(*testing.T) (*s3gw.Gateway, *stubPost)
		form *postForm
		code string
	}{
		{
			name: "bucket policy deny",
			gw:   denyingGateway,
			form: &postForm{
				conditions: []string{`{"key": "a.txt"}`},
				fields:     [][2]string{{"key", "a.txt"}},
				filename:   "x", content: "data",
			},
			code: "AccessDenied",
		},
		{
			name: "unsupported canned acl",
			form: &postForm{
				conditions: []string{`{"key": "a.txt"}`, `{"acl": "public-read"}`},
				fields:     [][2]string{{"key", "a.txt"}, {"acl", "public-read"}},
				filename:   "x", content: "data",
			},
			code: "AccessControlListNotSupported",
		},
		{
			name: "unknown form field fails loudly",
			form: &postForm{
				conditions: []string{`{"key": "a.txt"}`, `{"x-amz-website-redirect-location": "/foo"}`},
				fields:     [][2]string{{"key", "a.txt"}, {"x-amz-website-redirect-location", "/foo"}},
				filename:   "x", content: "data",
			},
			code: "NotImplemented",
		},
		{
			name: "missing key field",
			form: &postForm{
				conditions: []string{},
				fields:     [][2]string{},
				filename:   "x", content: "data",
			},
			code: "InvalidArgument",
		},
		{
			// refused before the hooks run, like the header path
			name: "unsupported encryption method",
			form: &postForm{
				conditions: []string{`{"key": "a.txt"}`, `{"x-amz-server-side-encryption": "rot13"}`},
				fields:     [][2]string{{"key", "a.txt"}, {"x-amz-server-side-encryption", "rot13"}},
				filename:   "x", content: "data",
			},
			code: "InvalidArgument",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gw *s3gw.Gateway
			var stub *stubPost
			if tc.gw != nil {
				gw, stub = tc.gw(t)
			} else {
				gw = newTestGateway(t)
				stub = &stubPost{}
				if err := gw.SetBackend("testbucket", stub); err != nil {
					t.Fatal(err)
				}
			}
			w := httptest.NewRecorder()
			gw.Handler().ServeHTTP(w, tc.form.request(t))
			if !strings.Contains(w.Body.String(), tc.code) {
				t.Errorf("expect %s, got %d: %s", tc.code, w.Code, w.Body.String())
			}
			if stub.putIn != nil {
				t.Error("a refused upload must not reach the backend")
			}
		})
	}
}
