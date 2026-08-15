#!/usr/bin/env python3
"""Generate SigV4 cross-check vectors signed by curl.

curl's --aws-sigv4 (lib/http_aws_sigv4.c) is a from-scratch C
implementation that shares no code with any AWS SDK, and it ships on
effectively every machine — a systematically different lineage from both
botocore (generate.py) and the aws-sdk-go-v2 signer the verifier re-signs
with. Every request it signs must verify.

Vectors are captured off the wire: curl signs and sends a real request to
a local capture socket, and the vector records exactly what curl sent.
Request shapes are deterministic per seed, but curl picks the signing time
itself and the User-Agent names the curl build, so regeneration is not
byte-identical (unlike generate.py); the vector records the X-Amz-Date
curl chose and the test pins the verifier clock to it.

    python3 generate_curl.py --seed 20260815 --count 96 --out curl.json

curl limitations vs generate.py: header-signing only (curl cannot
presign), and no session tokens (curl has no --aws-sigv4 token support;
a hand-added x-amz-security-token header is not part of its credential
handling).
"""

import argparse
import json
import random
import socket
import string
import subprocess

# The same shape sets as generate.py, minus dot segments: curl's URL parser
# resolves them client-side (RFC 3986 dedotdotify) before signing, so they
# never reach the wire and would only shrink path diversity.
PATH_TOKENS = [
    "photos", "backup-2026", "a", "0", "~user", "_x", "file.txt",
    "", "%2F", "%20", "%E3%81%82", "%00", "%7F", "%25",
    "a*b", "a'b", "(1)", "!x", "$y", "&z", "=eq", ":c", "@h", "a+b",
    "%2Fnested%2Fkey", "trailing.", "-lead",
]
QUERY_KEYS = ["prefix", "list-type", "marker", "a", "response-content-type", "q k", "empty"]
QUERY_VALUES = ["", "2", "photos/", "a/b", "あ", "text/plain", "a+b", "*", "~", "100%", "a b"]
HEADER_NAMES = ["x-amz-meta-note", "X-Amz-Meta-Tag", "Content-Type", "Cache-Control", "x-amz-meta-empty"]
HEADER_VALUES = ["hello world", "a  b   c", "text/plain; charset=utf-8", "", "no-cache, no-store", "=?utf-8?B?44GC?="]
REGIONS = ["us-east-1", "ap-northeast-1", "eu-central-1", "moon-crater-7"]
METHODS = ["GET", "PUT", "POST", "DELETE", "HEAD"]
HOSTS = ["s3.example.com", "s3.example.com:8080", "storage.internal"]

from urllib.parse import quote


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
    return akid, secret


def capture(listener, cmd):
    proc = subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    try:
        conn, _ = listener.accept()
    except socket.timeout:
        proc.kill()
        _, err = proc.communicate()
        raise RuntimeError(f"curl never connected: {err.decode()}\n{cmd}")
    with conn:
        conn.settimeout(10)
        buf = b""
        while b"\r\n\r\n" not in buf:
            chunk = conn.recv(65536)
            if not chunk:
                break
            buf += chunk
        head = buf.partition(b"\r\n\r\n")[0]
        conn.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
    _, err = proc.communicate(timeout=10)
    if proc.returncode != 0:
        raise RuntimeError(f"curl exited {proc.returncode}: {err.decode()}\n{cmd}")
    return head


def parse_head(head):
    lines = head.decode("iso-8859-1").split("\r\n")
    method, uri, _ = lines[0].split(" ", 2)
    host, headers = None, []
    for line in lines[1:]:
        k, _, v = line.partition(":")
        v = v.strip(" \t")  # OWS, as an HTTP server parses field values
        if k.lower() == "host":
            host = v
        else:
            headers.append([k, v])
    return method, uri, host, headers


def header_value(headers, name):
    for k, v in headers:
        if k.lower() == name:
            return v
    raise RuntimeError(f"curl sent no {name} header")


def sign_vector(rng, listener, lport, i):
    method = rng.choice(METHODS)
    host = rng.choice(HOSTS)
    path = make_path(rng)
    query = make_query(rng)
    region = rng.choice(REGIONS)
    akid, secret = make_credentials(rng)

    headers = {}
    for _ in range(rng.randint(0, 3)):
        headers[rng.choice(HEADER_NAMES)] = rng.choice(HEADER_VALUES)

    hostname, _, port = host.partition(":")
    url = f"http://{host}{path}"
    if query:
        url += "?" + query
    cmd = [
        "curl", "-sS", "-o", "/dev/null", "--max-time", "10",
        "--aws-sigv4", f"aws:amz:{region}:s3",
        "--user", f"{akid}:{secret}",
        "--connect-to", f"{hostname}:{port or 80}:127.0.0.1:{lport}",
    ]
    for k, v in headers.items():
        # "name;" is curl's syntax for an empty-valued header; "name: " is a removal
        cmd += ["-H", f"{k};" if v == "" else f"{k}: {v}"]
    cmd += ["--head"] if method == "HEAD" else ["-X", method]
    cmd.append(url)

    wire_method, uri, wire_host, wire_headers = parse_head(capture(listener, cmd))
    if wire_method != method or wire_host != host:
        raise RuntimeError(f"curl sent {wire_method} {wire_host}, expected {method} {host}")
    d = header_value(wire_headers, "x-amz-date")
    return {
        "name": f"{i:03d}-header-{method}",
        "type": "header",
        "method": method,
        "host": host,
        "uri": uri,
        "headers": wire_headers,
        "access_key_id": akid,
        "secret_access_key": secret,
        "session_token": "",
        "region": region,
        "timestamp": f"{d[0:4]}-{d[4:6]}-{d[6:8]}T{d[9:11]}:{d[11:13]}:{d[13:15]}Z",
        "payload_hash": header_value(wire_headers, "x-amz-content-sha256"),
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seed", type=int, default=20260815)
    ap.add_argument("--count", type=int, default=96)
    ap.add_argument("--out", default="curl.json")
    args = ap.parse_args()

    curl_version = subprocess.check_output(["curl", "--version"]).decode().splitlines()[0]

    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    listener.listen()
    listener.settimeout(15)
    lport = listener.getsockname()[1]

    rng = random.Random(args.seed)
    vectors = [sign_vector(rng, listener, lport, i) for i in range(args.count)]
    listener.close()
    doc = {
        "generator": "sigv4/testdata/crosscheck/generate_curl.py",
        "curl_version": curl_version,
        "seed": args.seed,
        "vectors": vectors,
    }
    with open(args.out, "w") as f:
        json.dump(doc, f, indent=1, ensure_ascii=True)
        f.write("\n")
    print(f"{len(vectors)} vectors -> {args.out} ({curl_version})")


if __name__ == "__main__":
    main()
