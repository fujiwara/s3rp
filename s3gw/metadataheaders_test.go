package s3gw_test

import (
	"bufio"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Metadata keys must reach the client lowercase (x-amz-meta-meta1), as S3
// sends them: clients strip the prefix case-insensitively and index the
// parsed metadata by the remaining suffix, so a canonicalized
// X-Amz-Meta-Meta1 surfaces as the key "Meta1". Go clients canonicalize
// received header names too, hiding the wire form — so this test reads the
// raw response bytes.
func TestMetadataHeadersAreLowercaseOnTheWire(t *testing.T) {
	stub := &stubBackend{headOut: &s3.HeadObjectOutput{
		ContentLength: aws.Int64(0),
		// what the SDK yields after Go canonicalized the backend's header
		Metadata: map[string]string{"Meta1": "v1"},
	}}
	client, _ := newTestProxy(t, stub)

	presigner := s3.NewPresignClient(client)
	req, err := presigner.PresignHeadObject(t.Context(), &s3.HeadObjectInput{
		Bucket: aws.String("testbucket"),
		Key:    aws.String("k"),
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(req.URL)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("HEAD " + u.RequestURI() + " HTTP/1.1\r\nHost: " + u.Host + "\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}

	var rawHeaders []string
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		rawHeaders = append(rawHeaders, line)
	}

	found := false
	for _, l := range rawHeaders {
		name, _, _ := strings.Cut(l, ":")
		if strings.EqualFold(name, "x-amz-meta-meta1") {
			found = true
			if name != "x-amz-meta-meta1" {
				t.Errorf("metadata header name not lowercase on the wire: %q", name)
			}
		}
	}
	if !found {
		t.Fatalf("no metadata header in response: %q", rawHeaders)
	}
}
