# Changelog

## [v0.0.3](https://github.com/fujiwara/s3rp/compare/v0.0.2...v0.0.3) - 2026-08-02

- Bound the backend client cache with an LRU and make cache sizes adjustable by @fujiwara in https://github.com/fujiwara/s3rp/pull/50
- Pass opaque store metadata through to the hooks on Op by @fujiwara in https://github.com/fujiwara/s3rp/pull/52
- Remove the sqlite store implementation by @fujiwara in https://github.com/fujiwara/s3rp/pull/53
- Add optional signing-region pinning to the verifier by @fujiwara in https://github.com/fujiwara/s3rp/pull/54
- Let a service contribute s3.Options to backend clients by @fujiwara in https://github.com/fujiwara/s3rp/pull/55
- Make the policy surface syntax a parse-time Dialect by @fujiwara in https://github.com/fujiwara/s3rp/pull/56
- Support browser-based POST uploads (SigV4 POST policy) by @fujiwara in https://github.com/fujiwara/s3rp/pull/57
- Pass SSE-S3/SSE-KMS through to the backend, refuse SSE-C loudly by @fujiwara in https://github.com/fujiwara/s3rp/pull/58
- Document the production boundaries: lifecycle, quota, pre-auth cost by @fujiwara in https://github.com/fujiwara/s3rp/pull/59
- Expose cache statistics for sizing the client and signer caches by @fujiwara in https://github.com/fujiwara/s3rp/pull/60
- Accept temporary credentials (session tokens) by @fujiwara in https://github.com/fujiwara/s3rp/pull/61
- Harden the aws-chunked decoder against three review findings by @fujiwara in https://github.com/fujiwara/s3rp/pull/62
- Give the client canonical error messages, not the backend's by @fujiwara in https://github.com/fujiwara/s3rp/pull/63
- Fix two stale comments, drop six that restate the code by @fujiwara in https://github.com/fujiwara/s3rp/pull/64
- Move the gateway tests into s3gw by @fujiwara in https://github.com/fujiwara/s3rp/pull/65
- Measure coverage in CI by @fujiwara in https://github.com/fujiwara/s3rp/pull/66
- Test checksum and s3xml directly by @fujiwara in https://github.com/fujiwara/s3rp/pull/67

## [v0.0.2](https://github.com/fujiwara/s3rp/compare/v0.0.1...v0.0.2) - 2026-08-01

- Lead the README with the design rationale by @fujiwara in https://github.com/fujiwara/s3rp/pull/25
- Add Object Lock support by @fujiwara in https://github.com/fujiwara/s3rp/pull/27
- Name subresource query keys as constants by @fujiwara in https://github.com/fujiwara/s3rp/pull/28
- Make request dispatch table-driven by @fujiwara in https://github.com/fujiwara/s3rp/pull/29
- Match policy actions case-insensitively by @fujiwara in https://github.com/fujiwara/s3rp/pull/30
- Broaden policy and presigned-URL test coverage by @fujiwara in https://github.com/fujiwara/s3rp/pull/31
- Support the "?" single-character wildcard in policies by @fujiwara in https://github.com/fujiwara/s3rp/pull/32
- Add per-user operation policies by @fujiwara in https://github.com/fujiwara/s3rp/pull/33
- Bound and optimize policy authorization by @fujiwara in https://github.com/fujiwara/s3rp/pull/34
- Address codex review: integrity, isolation, and hardening fixes by @fujiwara in https://github.com/fujiwara/s3rp/pull/35
- Follow-up review fixes: hot-path cost, unused skip, fail-open hash check by @fujiwara in https://github.com/fujiwara/s3rp/pull/36
- Resolve optional backend fields in every store by @fujiwara in https://github.com/fujiwara/s3rp/pull/38
- Give each access key its own SigV4 signer by @fujiwara in https://github.com/fujiwara/s3rp/pull/37
- Extract checksum/ and s3err/ as reusable packages by @fujiwara in https://github.com/fujiwara/s3rp/pull/39
- Extract SigV4 verification into sigv4/ by @fujiwara in https://github.com/fujiwara/s3rp/pull/40
- Extract CORS evaluation into cors/, and fix allowed_headers wildcards by @fujiwara in https://github.com/fujiwara/s3rp/pull/41
- Extract the S3 XML wire types into s3xml/ by @fujiwara in https://github.com/fujiwara/s3rp/pull/42
- Reference the extracted packages directly instead of aliasing by @fujiwara in https://github.com/fujiwara/s3rp/pull/43
- Keep the cause of a failure for the log by @fujiwara in https://github.com/fujiwara/s3rp/pull/44
- Let a service intervene in each operation by @fujiwara in https://github.com/fujiwara/s3rp/pull/45
- Move the S3 API into s3gw/ by @fujiwara in https://github.com/fujiwara/s3rp/pull/46
- Document building a service on the gateway by @fujiwara in https://github.com/fujiwara/s3rp/pull/47
- Report requests to an observer instead of logging them by @fujiwara in https://github.com/fujiwara/s3rp/pull/48
- Document what a TLS terminator in front must not do by @fujiwara in https://github.com/fujiwara/s3rp/pull/49

## [v0.0.1](https://github.com/fujiwara/s3rp/commits/v0.0.1) - 2026-07-31

- Make backend endpoint and credentials optional by @fujiwara in https://github.com/fujiwara/s3rp/pull/5
- Add multipart upload support by @fujiwara in https://github.com/fujiwara/s3rp/pull/7
- Add presigned URL support by @fujiwara in https://github.com/fujiwara/s3rp/pull/8
- Add DeleteObjects, CopyObject, UploadPartCopy, ListObjects v1 and GetBucketLocation by @fujiwara in https://github.com/fujiwara/s3rp/pull/9
- Add object tagging support by @fujiwara in https://github.com/fujiwara/s3rp/pull/10
- Add versioning support by @fujiwara in https://github.com/fujiwara/s3rp/pull/11
- Refactor access key indexes and query parameter checks by @fujiwara in https://github.com/fujiwara/s3rp/pull/12
- Restructure around tenants with tenant-level keys and a Store interface by @fujiwara in https://github.com/fujiwara/s3rp/pull/13
- Mimic ACL-disabled buckets and introduce users as stable identities by @fujiwara in https://github.com/fujiwara/s3rp/pull/14
- Add bucket policy support by @fujiwara in https://github.com/fujiwara/s3rp/pull/15
- Add CORS support handled by the proxy by @fujiwara in https://github.com/fujiwara/s3rp/pull/16
- Run integration tests against Ceph RGW in addition to versitygw by @fujiwara in https://github.com/fujiwara/s3rp/pull/17
- Add CLAUDE.md and clarify PoC status in README by @fujiwara in https://github.com/fujiwara/s3rp/pull/18
- Pass checksums through end-to-end by @fujiwara in https://github.com/fujiwara/s3rp/pull/19
- Update all GitHub Actions to the latest versions by @fujiwara in https://github.com/fujiwara/s3rp/pull/20
- Add a sqlite-backed read-only store with separate write tooling by @fujiwara in https://github.com/fujiwara/s3rp/pull/21
- Set the semver in version.go for tagpr by @fujiwara in https://github.com/fujiwara/s3rp/pull/22
- Fix stale README references by @fujiwara in https://github.com/fujiwara/s3rp/pull/24

## [v0.0.1](https://github.com/fujiwara/s3rp/commits/v0.0.1) - 2026-07-31

- Make backend endpoint and credentials optional by @fujiwara in https://github.com/fujiwara/s3rp/pull/5
- Add multipart upload support by @fujiwara in https://github.com/fujiwara/s3rp/pull/7
- Add presigned URL support by @fujiwara in https://github.com/fujiwara/s3rp/pull/8
- Add DeleteObjects, CopyObject, UploadPartCopy, ListObjects v1 and GetBucketLocation by @fujiwara in https://github.com/fujiwara/s3rp/pull/9
- Add object tagging support by @fujiwara in https://github.com/fujiwara/s3rp/pull/10
- Add versioning support by @fujiwara in https://github.com/fujiwara/s3rp/pull/11
- Refactor access key indexes and query parameter checks by @fujiwara in https://github.com/fujiwara/s3rp/pull/12
- Restructure around tenants with tenant-level keys and a Store interface by @fujiwara in https://github.com/fujiwara/s3rp/pull/13
- Mimic ACL-disabled buckets and introduce users as stable identities by @fujiwara in https://github.com/fujiwara/s3rp/pull/14
- Add bucket policy support by @fujiwara in https://github.com/fujiwara/s3rp/pull/15
- Add CORS support handled by the proxy by @fujiwara in https://github.com/fujiwara/s3rp/pull/16
- Run integration tests against Ceph RGW in addition to versitygw by @fujiwara in https://github.com/fujiwara/s3rp/pull/17
- Add CLAUDE.md and clarify PoC status in README by @fujiwara in https://github.com/fujiwara/s3rp/pull/18
- Pass checksums through end-to-end by @fujiwara in https://github.com/fujiwara/s3rp/pull/19
- Update all GitHub Actions to the latest versions by @fujiwara in https://github.com/fujiwara/s3rp/pull/20
- Add a sqlite-backed read-only store with separate write tooling by @fujiwara in https://github.com/fujiwara/s3rp/pull/21
