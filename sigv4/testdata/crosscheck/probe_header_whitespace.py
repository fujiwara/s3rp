#!/usr/bin/env python3
"""Probe how an S3 endpoint canonicalizes a tab inside a signed header value.

Companion to docs/sigv4-canonicalization.md ("Header values"). Signs a PUT
whose x-amz-meta-t value contains a tab two ways and reports which one the
endpoint accepts:

- boto: botocore's Trimall, which collapses every whitespace run (tabs
  included) to one space. The CRT signs the same way.
- go:   aws-sdk-go-v2's, which collapses runs of spaces only and keeps tabs.

SignatureDoesNotMatch means the endpoint canonicalizes differently from the
form signed. The PUT writes one object named "tabprobe".

    # real AWS, credentials from the default chain (aws sso login first):
    python3 probe_header_whitespace.py --bucket my-test-bucket

    # any S3-compatible endpoint (path-style):
    python3 probe_header_whitespace.py --endpoint http://127.0.0.1:7480 \\
        --access-key backendkey --secret-key backendsecret --bucket tabprobe

Requires botocore (pip install botocore).
"""

import argparse
import http.client
import re
from urllib.parse import urlsplit

import botocore.session
from botocore.auth import S3SigV4Auth
from botocore.awsrequest import AWSRequest
from botocore.credentials import Credentials

BOTO = S3SigV4Auth._header_value


def go_style(self, value):
    return re.sub(" {2,}", " ", value.strip(" ")).strip()


def probe(scheme, host, port, path, creds, region, value, style):
    S3SigV4Auth._header_value = go_style if style == "go" else BOTO
    netloc = host if port in (80, 443) else f"{host}:{port}"
    req = AWSRequest(method="PUT", url=f"{scheme}://{netloc}{path}", data=b"x",
                     headers={"x-amz-meta-t": value, "x-amz-content-sha256": "UNSIGNED-PAYLOAD"})
    S3SigV4Auth(creds, "s3", region).add_auth(req)
    cls = http.client.HTTPSConnection if scheme == "https" else http.client.HTTPConnection
    conn = cls(host, port, timeout=15)
    conn.putrequest("PUT", path, skip_host=True, skip_accept_encoding=True)
    conn.putheader("Host", netloc)
    for k, v in req.headers.items():
        conn.putheader(k, v)
    conn.putheader("Content-Length", "1")
    conn.endheaders(b"x")
    r = conn.getresponse()
    body = r.read()
    code = body.split(b"<Code>")[1].split(b"</Code>")[0].decode() if b"<Code>" in body else ""
    return f"{r.status} {code}".strip()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--endpoint", help="S3-compatible endpoint (path-style); default: AWS")
    ap.add_argument("--access-key")
    ap.add_argument("--secret-key")
    ap.add_argument("--region", default=None)
    ap.add_argument("--bucket", required=True)
    args = ap.parse_args()

    if args.endpoint:
        u = urlsplit(args.endpoint)
        scheme, host = u.scheme, u.hostname
        port = u.port or (443 if scheme == "https" else 80)
        path = f"/{args.bucket}/tabprobe"
        creds = Credentials(args.access_key, args.secret_key)
        region = args.region or "us-east-1"
    else:
        sess = botocore.session.Session()
        c = sess.get_credentials().get_frozen_credentials()
        creds = Credentials(c.access_key, c.secret_key, c.token)
        region = args.region or sess.get_config_variable("region") or "us-east-1"
        scheme, host, port, path = "https", f"{args.bucket}.s3.{region}.amazonaws.com", 443, "/tabprobe"

    for label, value in [("tab", "a\tb"), ("space+tab", "a \t b")]:
        for style in ("boto", "go"):
            print(f"{label:10} {style:5} {probe(scheme, host, port, path, creds, region, value, style)}")


if __name__ == "__main__":
    main()
