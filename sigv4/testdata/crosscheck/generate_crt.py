#!/usr/bin/env python3
"""Generate SigV4 cross-check vectors signed by the AWS CRT (aws-cli).

aws-cli v2 transfers through the AWS Common Runtime (aws-c-auth, a C
implementation) by default since 2.38. Its signer is a lineage of its own
and signs what botocore does not: notably "content-length:0" on bodiless
requests (CreateMultipartUpload, HeadObject, DeleteObject), which
aws-sdk-go-v2's signer cannot reproduce. Every request it signs must
verify.

Vectors are captured off the wire like generate_curl.py: each scenario
runs one `aws s3` command against a local capture socket, records the
first request, answers 500 and stops the command. The CRT picks the
signing time, so regeneration is not byte-identical.

    python3 generate_crt.py --aws /path/to/aws --out crt.json

Header-signed only (the s3 transfer commands do not presign), and without
session tokens.
"""

import argparse
import json
import os
import socket
import subprocess
import tempfile

AKID = "AKIDCRTEXAMPLE000001"
SECRET = "crtSecretKeyExample/0123456789+abcdefghij"
REGIONS = ["us-east-1", "ap-northeast-1"]

# (name, aws s3 arguments, local file size or None, requests to answer
# before the one recorded). A download first asks for the object's size
# outside the CRT; answering that lets the CRT send its own HeadObject.
SCENARIOS = [
    ("upload-multipart", ["cp", "{file}", "s3://bucket/dir/big file.bin"], 20 * 1024 * 1024, 0),
    ("upload-single", ["cp", "{file}", "s3://bucket/small.txt"], 1000, 0),
    ("upload-empty", ["cp", "{file}", "s3://bucket/empty"], 0, 0),
    ("download", ["cp", "s3://bucket/key%2Bplus/あ.txt", "{out}"], None, 1),
    ("delete", ["rm", "s3://bucket/a b/c.txt"], None, 0),
    # a tab in a signed header value: the CRT signs it collapsed to a space
    ("upload-metadata-tab", ["cp", "{file}", "s3://bucket/meta.txt", "--metadata", '{{"t": "a\\tb  c"}}'], 1000, 0),
]

HEAD_OK = (b"HTTP/1.1 200 OK\r\nContent-Length: 1000\r\nETag: \"0123456789abcdef0123456789abcdef\"\r\n"
           b"Last-Modified: Wed, 14 Oct 2026 00:00:00 GMT\r\nAccept-Ranges: bytes\r\n\r\n")


def read_head(listener, proc, cmd):
    try:
        conn, _ = listener.accept()
    except socket.timeout:
        proc.kill()
        _, err = proc.communicate()
        raise RuntimeError(f"aws never connected: {err.decode()}\n{cmd}")
    conn.settimeout(10)
    buf = b""
    while b"\r\n\r\n" not in buf:
        chunk = conn.recv(65536)
        if not chunk:
            break
        buf += chunk
    return conn, buf.partition(b"\r\n\r\n")[0]


def capture(listener, cmd, env, skip):
    proc = subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, env=env)
    for _ in range(skip):
        conn, _ = read_head(listener, proc, cmd)
        with conn:
            conn.sendall(HEAD_OK)
    conn, head = read_head(listener, proc, cmd)
    with conn:
        conn.sendall(b"HTTP/1.1 500 Internal Server Error\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
    # the CLI retries a 500; the request recorded is all the vector needs
    proc.kill()
    proc.communicate()
    return head


def parse_head(head):
    lines = head.decode("iso-8859-1").split("\r\n")
    method, uri, _ = lines[0].split(" ", 2)
    host, headers = None, []
    for line in lines[1:]:
        k, _, v = line.partition(":")
        v = v.strip(" \t")
        if k.lower() == "host":
            host = v
        else:
            headers.append([k, v])
    return method, uri, host, headers


def header_value(headers, name):
    for k, v in headers:
        if k.lower() == name:
            return v
    raise RuntimeError(f"no {name} header")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--aws", default="aws", help="aws-cli v2 binary (>= 2.38 for CRT by default)")
    ap.add_argument("--out", default="crt.json")
    args = ap.parse_args()

    version = subprocess.check_output([args.aws, "--version"]).decode().strip()
    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    listener.listen()
    listener.settimeout(30)
    lport = listener.getsockname()[1]

    vectors = []
    with tempfile.TemporaryDirectory() as tmp:
        config = os.path.join(tmp, "config")
        with open(config, "w") as f:
            f.write("[default]\ns3 =\n  preferred_transfer_client = crt\n")
        for region in REGIONS:
            env = {
                "PATH": os.environ.get("PATH", ""),
                "HOME": tmp,
                "AWS_CONFIG_FILE": config,
                "AWS_SHARED_CREDENTIALS_FILE": os.path.join(tmp, "credentials"),
                "AWS_ACCESS_KEY_ID": AKID,
                "AWS_SECRET_ACCESS_KEY": SECRET,
                "AWS_REGION": region,
                "AWS_ENDPOINT_URL": f"http://127.0.0.1:{lport}",
                "AWS_MAX_ATTEMPTS": "1",
            }
            for name, s3args, size, skip in SCENARIOS:
                path = os.path.join(tmp, "src.bin")
                if size is not None:
                    with open(path, "wb") as f:
                        f.write(os.urandom(size))
                argv = [a.format(file=path, out=os.path.join(tmp, "out.bin")) for a in s3args]
                cmd = [args.aws, "s3", *argv, "--only-show-errors"]
                method, uri, host, headers = parse_head(capture(listener, cmd, env, skip))
                d = header_value(headers, "x-amz-date")
                vectors.append({
                    "name": f"{len(vectors):03d}-{name}-{method}",
                    "type": "header",
                    "method": method,
                    "host": host,
                    "uri": uri,
                    "headers": headers,
                    "access_key_id": AKID,
                    "secret_access_key": SECRET,
                    "session_token": "",
                    "region": region,
                    "timestamp": f"{d[0:4]}-{d[4:6]}-{d[6:8]}T{d[9:11]}:{d[11:13]}:{d[13:15]}Z",
                    "payload_hash": header_value(headers, "x-amz-content-sha256"),
                })
    listener.close()
    doc = {
        "generator": "sigv4/testdata/crosscheck/generate_crt.py",
        "aws_cli_version": version,
        "vectors": vectors,
    }
    with open(args.out, "w") as f:
        json.dump(doc, f, indent=1, ensure_ascii=True)
        f.write("\n")
    print(f"{len(vectors)} vectors -> {args.out} ({version})")


if __name__ == "__main__":
    main()
