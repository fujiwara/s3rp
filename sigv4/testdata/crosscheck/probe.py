#!/usr/bin/env python3
"""Probe how an S3 endpoint canonicalizes non-canonical wire queries.

Companion to docs/sigv4-canonicalization.md, which records what this
measured in 2026-08 against AWS S3, versitygw and Ceph RGW. Re-run it when
the ecosystem may have moved — a new backend, a new Ceph release, a
suspicion that AWS changed behavior.

Every case is a ListBuckets GET (prefix is a real ListBuckets parameter)
whose query is written raw into the request line and transmitted verbatim
with http.client.putrequest, so no client-side re-encoding happens. The
signature is computed either by botocore's sign-as-sent canonicalization or
over an explicitly chosen canonical query string. The discriminator is the
response: SignatureDoesNotMatch means the endpoint's canonicalization
disagrees with the one signed; anything else (200, AccessDenied, ...) means
the signature was accepted.

    # real AWS, credentials from the default chain (aws sso login first):
    python3 probe.py

    # any S3-compatible endpoint:
    python3 probe.py --endpoint http://127.0.0.1:7480 \
        --access-key backendkey --secret-key backendsecret

Requires botocore (pip install botocore).
"""

import argparse
import http.client
import re
import sys
from urllib.parse import urlsplit

import botocore.session
from botocore.auth import S3SigV4Auth
from botocore.awsrequest import AWSRequest
from botocore.credentials import Credentials

REGION = "us-east-1"


class FixedCanonicalQuery(S3SigV4Auth):
    """Signs with an explicitly chosen canonical query string."""

    def __init__(self, credentials, service_name, region_name, canonical_query):
        super().__init__(credentials, service_name, region_name)
        self.canonical_query = canonical_query

    def _canonical_query_string(self, request):
        return self.canonical_query


# (name, wire URI, canonical query — None signs as botocore does, over the
# query as sent). The dup-key matrix varies wire order and canonical order
# independently: あ is %E3%81%82, and '~' (0x7E) sorts after '%' (0x25) raw
# but before 0xE3 decoded, so the orderings disagree.
CASES = [
    ("baseline-canonical", "/?prefix=canonical", None),
    ("raw-slash", "/?prefix=photos/", None),
    ("raw-plus", "/?prefix=a+b", None),
    ("raw-star", "/?prefix=*", None),
    ("noncanonical-escape", "/?prefix=%7E", None),
    ("dup-A-wire-utf8-canon-encsort", "/?prefix=%E3%81%82&prefix=~", None),
    ("dup-B-wire-tilde-canon-encsort", "/?prefix=~&prefix=%E3%81%82", "prefix=%E3%81%82&prefix=~"),
    ("dup-C-wire-tilde-canon-wire", "/?prefix=~&prefix=%E3%81%82", "prefix=~&prefix=%E3%81%82"),
    ("dup-D-wire-utf8-canon-decsort", "/?prefix=%E3%81%82&prefix=~", "prefix=~&prefix=%E3%81%82"),
    ("dup-E-wire-utf8-canon-wire", "/?prefix=%E3%81%82&prefix=~", "prefix=%E3%81%82&prefix=~"),
]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--endpoint", default=f"https://s3.{REGION}.amazonaws.com",
                    help="endpoint URL (default: real AWS %(default)s)")
    ap.add_argument("--access-key")
    ap.add_argument("--secret-key")
    args = ap.parse_args()

    if args.access_key:
        creds = Credentials(args.access_key, args.secret_key)
    else:
        found = botocore.session.get_session().get_credentials()
        if found is None:
            sys.exit("no AWS credentials found; pass --access-key/--secret-key or configure the default chain")
        creds = found.get_frozen_credentials()

    split = urlsplit(args.endpoint)
    hostport = split.netloc
    conn_class = http.client.HTTPSConnection if split.scheme == "https" else http.client.HTTPConnection

    for name, uri, canonical in CASES:
        if canonical is None:
            signer = S3SigV4Auth(creds, "s3", REGION)
        else:
            signer = FixedCanonicalQuery(creds, "s3", REGION, canonical)
        req = AWSRequest(method="GET", url=f"{split.scheme}://{hostport}{uri}", data=b"")
        signer.add_auth(req)
        sent = req.url[req.url.index(hostport) + len(hostport):]
        if sent != uri:
            print(f"{name}: SKIP (botocore rewrote the URI to {sent!r})")
            continue

        conn = conn_class(hostport, timeout=15)
        conn.putrequest("GET", uri, skip_host=True, skip_accept_encoding=True)
        conn.putheader("Host", hostport)
        for k, v in req.headers.items():
            conn.putheader(k, v)
        conn.endheaders()
        resp = conn.getresponse()
        body = resp.read(4096).decode("utf-8", "replace")
        code = m.group(1) if (m := re.search(r"<Code>([^<]+)</Code>", body)) else ""
        verdict = "REJECTED (sig)" if code == "SignatureDoesNotMatch" else "ACCEPTED"
        print(f"{name:32s} {uri:30s} -> HTTP {resp.status} {code or 'OK':24s} {verdict}")
        conn.close()


if __name__ == "__main__":
    main()
