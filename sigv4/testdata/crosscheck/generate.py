#!/usr/bin/env python3
"""Generate SigV4 cross-check vectors signed by botocore.

The verifier re-signs a clone of the request with aws-sdk-go-v2's own
signer, so a request that the Go SDK signed exercises none of the
asymmetries: both sides share the signing code. botocore is an independent
SigV4 implementation carried by every boto3/aws-cli client, so requests it
signs are what non-Go clients actually send — and every one of them must
verify. The vectors record the request exactly as it would appear on the
wire plus the credentials, and sigv4/crosscheck_test.go replays them
against the verifier.

Deterministic per seed, so the committed corpus regenerates identically
from the same botocore version:

    python3 generate.py --seed 20260815 --count 96 --out botocore.json
"""

import argparse
import datetime
import json
import random
import string
from urllib.parse import quote

import botocore
import botocore.auth
from botocore.auth import S3SigV4Auth, S3SigV4QueryAuth
from botocore.awsrequest import AWSRequest
from botocore.credentials import Credentials

SIGNING_TIME = datetime.datetime(2026, 8, 1, 12, 0, 0, tzinfo=datetime.timezone.utc)
EMPTY_SHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

# What paths are built from. Raw characters cover the pchar set that clients
# send unencoded; encoded tokens cover what they escape — including %2F,
# which is part of a key and not a separator, and dot segments, which S3
# treats as opaque.
PATH_TOKENS = [
    "photos", "backup-2026", "a", "0", "~user", "_x", "file.txt", "...",
    "..", ".", "", "%2F", "%20", "%E3%81%82", "%00", "%7F", "%25",
    "a*b", "a'b", "(1)", "!x", "$y", "&z", "=eq", ":c", "@h", "a+b",
    "%2Fnested%2Fkey", "trailing.", "-lead",
]
# Decoded query parts; make_query percent-encodes them the way client
# serializers do (only unreserved characters stay literal). The wire forms
# this deliberately avoids are a genuine SigV4 divergence the cross-check
# surfaced: botocore canonicalizes the query *as sent* while aws-sdk-go-v2
# decodes and re-encodes it, so raw reserved characters in a value
# (?a=photos/, ?a=a+b, ?a=*) and duplicate keys whose values sort
# differently raw vs decoded (?a=%E3%81%82&a=~) verify under one SDK's
# scheme and not the other. No mainstream client serializer emits those
# forms — every S3 SDK sends the canonical encoding — and s3rp sides with
# its own signer, aws-sdk-go-v2. Measured against real implementations in
# docs/sigv4-canonicalization.md (probe.py next to this file): AWS,
# versitygw and RGW all reject the raw-reserved-character forms too; the
# duplicate-key ordering is where the ecosystem disagrees, and s3rp's one
# accepted ordering sits between versitygw (rejects all) and AWS
# (accepts all).
QUERY_KEYS = ["prefix", "list-type", "marker", "a", "response-content-type", "q k", "empty"]
QUERY_VALUES = ["", "2", "photos/", "a/b", "あ", "text/plain", "a+b", "*", "~", "100%", "a b"]
HEADER_NAMES = ["x-amz-meta-note", "X-Amz-Meta-Tag", "Content-Type", "Cache-Control", "x-amz-meta-empty"]
HEADER_VALUES = ["hello world", "a  b   c", " padded ", "text/plain; charset=utf-8", "", "no-cache, no-store", "=?utf-8?B?44GC?="]
REGIONS = ["us-east-1", "ap-northeast-1", "eu-central-1", "moon-crater-7"]
METHODS = ["GET", "PUT", "POST", "DELETE", "HEAD"]


def make_path(rng):
    return "/" + "/".join(rng.choice(PATH_TOKENS) for _ in range(rng.randint(1, 4)))


def make_query(rng):
    parts = []
    keys = rng.sample(QUERY_KEYS, rng.randint(0, 3))
    for k in keys:
        v = rng.choice(QUERY_VALUES)
        ek, ev = quote(k, safe="-_.~"), quote(v, safe="-_.~")
        parts.append(f"{ek}={ev}" if v or rng.random() < 0.7 else ek)
    if parts and rng.random() < 0.2:
        parts.append(parts[0])  # the same pair repeated
    return "&".join(parts)


def make_credentials(rng):
    akid = "AKID" + "".join(rng.choices(string.ascii_uppercase + string.digits, k=16))
    secret = "".join(rng.choices(string.ascii_letters + string.digits + "+/", k=40))
    token = None
    if rng.random() < 0.33:
        token = "".join(rng.choices(string.ascii_letters + string.digits + "+/=", k=rng.randint(16, 200)))
    return akid, secret, token


def sign_vector(rng, i):
    method = rng.choice(METHODS)
    host = rng.choice(["s3.example.com", "s3.example.com:8080", "storage.internal"])
    path = make_path(rng)
    query = make_query(rng)
    region = rng.choice(REGIONS)
    akid, secret, token = make_credentials(rng)
    creds = Credentials(akid, secret, token)
    presigned = rng.random() < 0.5

    headers = {}
    for _ in range(rng.randint(0, 3)):
        headers[rng.choice(HEADER_NAMES)] = rng.choice(HEADER_VALUES)

    # over plain http botocore always signs the payload; an https URL is how
    # a client produces UNSIGNED-PAYLOAD. The scheme is not part of the
    # signature, so the vector replays the same either way.
    scheme = "https" if rng.random() < 0.5 else "http"
    url = f"{scheme}://{host}{path}"
    if query:
        url += "?" + query
    req = AWSRequest(method=method, url=url, headers=headers, data=b"")

    if presigned:
        expires = rng.randint(1, 604800)
        S3SigV4QueryAuth(creds, "s3", region, expires=expires).add_auth(req)
        payload_hash = "UNSIGNED-PAYLOAD"
    else:
        S3SigV4Auth(creds, "s3", region).add_auth(req)
        # record what botocore actually declared, not a prediction
        payload_hash = req.headers["X-Amz-Content-SHA256"]

    signed_url = req.url
    uri = signed_url[signed_url.index(host) + len(host):]
    return {
        "name": f"{i:03d}-{'presigned' if presigned else 'header'}-{method}",
        "type": "presigned" if presigned else "header",
        "method": method,
        "host": host,
        "uri": uri,
        "headers": [[k, v] for k, v in req.headers.items()],
        "access_key_id": akid,
        "secret_access_key": secret,
        "session_token": token or "",
        "region": region,
        "timestamp": SIGNING_TIME.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "payload_hash": payload_hash,
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seed", type=int, default=20260815)
    ap.add_argument("--count", type=int, default=96)
    ap.add_argument("--out", default="botocore.json")
    args = ap.parse_args()

    botocore.auth.get_current_datetime = lambda: SIGNING_TIME

    rng = random.Random(args.seed)
    vectors = [sign_vector(rng, i) for i in range(args.count)]
    doc = {
        "generator": "sigv4/testdata/crosscheck/generate.py",
        "botocore_version": botocore.__version__,
        "seed": args.seed,
        "vectors": vectors,
    }
    with open(args.out, "w") as f:
        json.dump(doc, f, indent=1, ensure_ascii=True)
        f.write("\n")
    print(f"{len(vectors)} vectors -> {args.out} (botocore {botocore.__version__})")


if __name__ == "__main__":
    main()
