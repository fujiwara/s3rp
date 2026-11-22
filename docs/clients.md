# Using S3 clients with the gateway

What to set in a client, and what does not work, when its endpoint is the
gateway. It is drawn from the [client compatibility run](client-compat.md),
which runs each client below through the same scenarios: small and awkward
keys, a server-side copy, a listing of more than a thousand keys, a
multipart upload and its download, and a presigned GET; aws-cli, `mc` and
s3cmd also read the bucket's lifecycle configuration. What a client does
outside those — versioning, tags, Object Lock, server-side encryption — is
not covered here.

The run's backend is Ceph RGW. A finding is listed under
[Whatever the backend](#whatever-the-backend) when it comes from the
client or the gateway, and under [On Ceph RGW](#on-ceph-rgw) when it
comes from what RGW answers. No other backend is run.

## Clients that work as they are

With the endpoint, path-style addressing and credentials set, and nothing
else:

- the AWS SDKs for JavaScript, Go, .NET, PHP and Rust, and the AWS SDK for
  Python (boto3) apart from [presigned URLs](#boto3-presigned-urls)
- aws-cli, with either transfer client (`preferred_transfer_client`)
- `mc`, restic, kopia, s5cmd, DuckDB (httpfs), Bun's `S3Client`, WinSCP

The other clients are below.

## Whatever the backend

### boto3: presigned URLs

Against an endpoint that is not Amazon's, `generate_presigned_url` signs
with Signature Version 2 unless the client is told otherwise, and the
gateway verifies Signature Version 4 only: the URL is answered 403.
Configure the client:

```python
boto3.client("s3", endpoint_url=..., config=Config(signature_version="s3v4"))
```

### s3cmd

- **Set `acl_public = False`.** Left unset, `s3cmd cp` reads the source's
  ACL and writes it to the copy. The gateway refuses every ACL write
  ([ACLs](s3-api.md#acls)), so the copy is made and the command still
  fails.
- **`s3cmd signurl` does not work.** It signs with Signature Version 2 and
  has no setting for another; make presigned URLs with another client.

### OpenDAL: a private certificate authority

OpenDAL has no setting for a CA certificate, so it cannot reach an https
endpoint whose certificate a private CA issued. A certificate from a
public CA is not affected.

### Keys with an empty segment

A key such as `dir//x.txt` is a key of its own in S3, distinct from
`dir/x.txt`. Some clients do not send it as given:

| client | what it does with `dir//x.txt` |
|---|---|
| rclone, s3fs, OpenDAL, Cyberduck (`duck`) | stores `dir/x.txt`, without an error |
| obstore (`object_store`) | refuses the key before sending anything |

The object is then not where another client looks for it. The same
happens against Amazon S3; it matters only where an application writes
such keys.

### A backend reached over http

This is about the deployment, and its users meet it as failing uploads.
The AWS SDKs send a checksum as a trailer by default, and the gateway
forwards a trailing checksum to an https backend only. With an http
backend, a multipart upload through the transfer manager of the SDK for
Python, Go or Java was refused by the backend (seen on versitygw). Reach
backends over https ([Checksums](s3-api.md#checksums)).

## On Ceph RGW

### AWS SDKs for Ruby and Java: downloading a multipart object

The transfer managers of these two SDKs download an object that was
uploaded in parts part by part (`GetObject` with `partNumber`), and read
each part's position from the `Content-Range` of the response. RGW sends
none, where S3 does ([Ceph #83398](https://tracker.ceph.com/issues/83398);
seen on v21.2.0 and v20.2.5), and the download fails: in Ruby,
`NoMethodError` in `FileDownloader#extract_range`. The gateway cannot
supply the header, since the part's position is not in RGW's answer.

A plain `GetObject` is not affected, nor are uploads. In Ruby, a download
mode that does not read by part works:

```ruby
transfer.download_file(path, bucket: bucket, key: key, mode: "get_range", chunk_size: 8 * 1024 * 1024)
# or, in one request
transfer.download_file(path, bucket: bucket, key: key, mode: "single_request")
```

In Java it is the asynchronous client with multipart enabled that reads by
part.
