package s3gw

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// SetVirtualHostSuffix enables virtual-hosted-style addressing alongside the
// path style: a request whose Host is "<bucket>" + suffix (with or without a
// port) addresses that bucket, and its whole path is the object key. Any
// other Host keeps path-style addressing, so both forms are served at once,
// as Amazon S3 does; unset (the default), only the path style exists.
//
// The suffix is the endpoint's own host name with a leading dot ("." is
// added when missing), so the bare endpoint stays path-style and only a
// proper subdomain names a bucket. Bucket names contain no dot, so the
// label is a single one; a dotted label names no bucket. Signature
// verification is unaffected: the signed Host is whatever the client sent.
func (g *Gateway) SetVirtualHostSuffix(suffix string) {
	suffix = strings.ToLower(strings.TrimSuffix(suffix, "."))
	if suffix != "" && !strings.HasPrefix(suffix, ".") {
		suffix = "." + suffix
	}
	g.vhostSuffix = suffix
}

// target is where a request points: the front bucket, the object key, and
// whether the client used virtual-hosted-style addressing, which the URLs
// the gateway hands back mirror.
type target struct {
	bucket string
	key    string
	vhost  bool
}

// requestTarget resolves the bucket and key a request addresses; see
// SetVirtualHostSuffix for the two forms.
func (g *Gateway) requestTarget(r *http.Request) (target, error) {
	if bucket, ok := g.virtualHostBucket(r.Host); ok {
		key, err := unescapeKey(strings.TrimPrefix(r.URL.EscapedPath(), "/"))
		if err != nil {
			return target{}, err
		}
		return target{bucket: bucket, key: key, vhost: true}, nil
	}
	bucket, key, err := splitPath(r.URL.EscapedPath())
	if err != nil {
		return target{}, err
	}
	return target{bucket: bucket, key: key}, nil
}

// virtualHostBucket returns the bucket a Host header names under the
// configured suffix, and false when the request is path-style.
func (g *Gateway) virtualHostBucket(hostport string) (string, bool) {
	if g.vhostSuffix == "" {
		return "", false
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	label, ok := strings.CutSuffix(host, g.vhostSuffix)
	if !ok || label == "" || strings.Contains(label, ".") {
		return "", false
	}
	return label, true
}

// objectURL is the front URL of an object as the client addressed the
// bucket: under the bucket's host name when the request was
// virtual-hosted, under the endpoint's path otherwise. It never reveals the
// backend.
func objectURL(r *http.Request, t target, key string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	path := "/" + key
	if !t.vhost {
		path = "/" + t.bucket + path
	}
	return scheme + "://" + r.Host + (&url.URL{Path: path}).EscapedPath()
}
