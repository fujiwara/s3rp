# /// script
# requires-python = ">=3.11"
# dependencies = ["obstore==0.12.1"]
# ///
"""The verbs of defs/obstore.yml, on the Rust object_store crate."""
import os
import sys
from datetime import timedelta
from pathlib import Path

import obstore
from obstore.store import S3Store

env = os.environ
store = S3Store(
    env["BUCKET"],
    endpoint=env["ENDPOINT"],
    access_key_id=env["ACCESS_KEY"],
    secret_access_key=env["SECRET_KEY"],
    region="us-east-1",
    virtual_hosted_style_request=False,
    # object_store bundles its roots and reads no CA from the environment;
    # the run's certificate is self-signed
    client_options={"allow_http": True, "allow_invalid_certificates": True},
)

verb, args = sys.argv[1], sys.argv[2:]
match verb:
    case "put":
        file, key = args
        # a path, so a large file goes up in parts
        obstore.put(store, key, Path(file))
    case "get":
        key, out = args
        with open(out, "wb") as f:
            for chunk in obstore.get(store, key).stream():
                f.write(chunk)
    case "list":
        for batch in obstore.list(store, prefix=args[0]):
            for meta in batch:
                print(meta["path"])
    case "delete":
        obstore.delete(store, args[0])
    case "copy":
        obstore.copy(store, args[0], args[1])
    case "presign":
        print(obstore.sign(store, "GET", args[0], timedelta(hours=1)))
    case "put_tree":
        directory, prefix = args
        for path in sorted(Path(directory).iterdir()):
            obstore.put(store, f"{prefix}/{path.name}", path)
    case _:
        sys.exit(f"unknown verb {verb}")
