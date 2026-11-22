# /// script
# requires-python = "==3.15.*"
# dependencies = ["boto3==1.44.9"]
# ///
"""The verbs of defs/sdk-python.yml, on boto3."""
import os
import sys
from pathlib import Path

import boto3
from botocore.config import Config

env = os.environ
s3 = boto3.client(
    "s3",
    endpoint_url=env["ENDPOINT"],
    aws_access_key_id=env["ACCESS_KEY"],
    aws_secret_access_key=env["SECRET_KEY"],
    region_name="us-east-1",
    verify=env["CA_FILE"],
    # s3v4: against a custom endpoint boto3 presigns with SigV2 otherwise,
    # which the gateway does not verify
    config=Config(signature_version="s3v4", s3={"addressing_style": "path"}),
)
bucket = env["BUCKET"]

verb, args = sys.argv[1], sys.argv[2:]
match verb:
    case "put":
        # the transfer manager, so a large file goes up in parts
        s3.upload_file(args[0], bucket, args[1])
    case "get":
        s3.download_file(bucket, args[0], args[1])
    case "list":
        for page in s3.get_paginator("list_objects_v2").paginate(Bucket=bucket, Prefix=args[0] + "/"):
            for obj in page.get("Contents", []):
                print(obj["Key"])
    case "delete":
        s3.delete_object(Bucket=bucket, Key=args[0])
    case "copy":
        s3.copy({"Bucket": bucket, "Key": args[0]}, bucket, args[1])
    case "presign":
        print(s3.generate_presigned_url("get_object", Params={"Bucket": bucket, "Key": args[0]}, ExpiresIn=3600))
    case "lifecycle":
        for rule in s3.get_bucket_lifecycle_configuration(Bucket=bucket)["Rules"]:
            prefix = rule.get("Filter", {}).get("Prefix", "")
            expire = rule.get("Expiration", {}).get("Days", "-")
            abort = rule.get("AbortIncompleteMultipartUpload", {}).get("DaysAfterInitiation", "-")
            print(f"{rule['ID']} prefix={prefix} expire={expire} abort={abort}")
    case "replication":
        for rule in s3.get_bucket_replication(Bucket=bucket)["ReplicationConfiguration"]["Rules"]:
            prefix = rule.get("Filter", {}).get("Prefix", "")
            print(f"{rule['ID']} prefix={prefix} status={rule['Status']} destination={rule['Destination']['Bucket']}")
    case "tags":
        for tag in s3.get_bucket_tagging(Bucket=bucket)["TagSet"]:
            print(f"{tag['Key']}={tag['Value']}")
    case "notification":
        for topic in s3.get_bucket_notification_configuration(Bucket=bucket).get("TopicConfigurations", []):
            print(f"{topic['Id']} topic={topic['TopicArn']} events={','.join(topic['Events'])}")
    case "put_tree":
        for path in sorted(Path(args[0]).iterdir()):
            s3.upload_file(str(path), bucket, f"{args[1]}/{path.name}")
    case _:
        sys.exit(f"unknown verb {verb}")
