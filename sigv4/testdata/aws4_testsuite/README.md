# AWS Signature Version 4 test suite

AWS's known-answer vectors for SigV4, copied unmodified from
[botocore](https://github.com/boto/botocore/tree/925bf5dde9fc19388d1d7f7b9653ebbed81e535f/tests/unit/auth/aws4_testsuite)
(`tests/unit/auth/aws4_testsuite`). Licensed under the Apache License 2.0;
see `LICENSE` and `NOTICE`.

Each case holds the request (`.req`), its canonical request (`.creq`),
string to sign (`.sts`), `Authorization` value (`.authz`) and the signed
request (`.sreq`). `TestAWS4TestSuite` replays every case; this file,
`LICENSE` and `NOTICE` aside, do not edit anything here.
