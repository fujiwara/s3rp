# /// script
# requires-python = "==3.15.*"
# dependencies = ["opendal==0.47.12"]
# ///
"""The verbs of defs/opendal.yml, on Apache OpenDAL (signer: reqsign)."""
import os
import shutil
import sys
from pathlib import Path

import opendal

env = os.environ
op = opendal.Operator(
    "s3",
    bucket=env["BUCKET"],
    endpoint=env["ENDPOINT"],
    access_key_id=env["ACCESS_KEY"],
    secret_access_key=env["SECRET_KEY"],
    region="us-east-1",
)
COPY_BUFFER = 8 * 1024 * 1024


def put(key: str, path: Path) -> None:
    # streamed, so a large file goes up in parts
    with open(path, "rb") as src, op.open(key, "wb") as dst:
        shutil.copyfileobj(src, dst, COPY_BUFFER)


verb, args = sys.argv[1], sys.argv[2:]
match verb:
    case "put":
        put(args[1], Path(args[0]))
    case "get":
        with op.open(args[0], "rb") as src, open(args[1], "wb") as dst:
            shutil.copyfileobj(src, dst, COPY_BUFFER)
    case "list":
        for entry in op.scan(args[0] + "/"):
            if not entry.path.endswith("/"):
                print(entry.path)
    case "delete":
        op.delete(args[0])
    case "copy":
        op.copy(args[0], args[1])
    case "put_tree":
        directory, prefix = args
        for path in sorted(Path(directory).iterdir()):
            put(f"{prefix}/{path.name}", path)
    case _:
        sys.exit(f"unknown verb {verb}")
