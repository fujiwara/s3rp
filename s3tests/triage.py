#!/usr/bin/env python3
"""Classify a ceph/s3-tests junit XML result into compatibility categories.

Usage: triage.py results.xml [harness.log [expected-localbackend.tsv]] > report.md

Exits 1 when the run needs a person: it is contaminated, or a failure
matched no rule. The third argument is for a run on the in-process backend
(s3gw/localbackend): the exact list of the failures that are that
backend's, which then stands in for the "Backend" rules below, and a
listed test that does not fail that way needs a person too.

Failures are bucketed by the first matching rule. The rules encode the
hand-verified findings of docs/s3-tests.md (see s3tests/AGENTS.md for the
verification method): categories named "Deliberate" are the documented
design surface, "Backend" ones were confirmed against the backend
directly. Name-based rules are heuristics — when a run against a new
backend or suite revision shifts the failure mix, spot-check them. The
UNMATCHED bucket is the interesting one: failures no known pattern
explains, to be triaged by hand.
"""

import re
import sys
import xml.etree.ElementTree as ET
from collections import Counter, defaultdict

CLIENT_ERROR = re.compile(
    r"An error occurred \((\w+)\) when calling the (\w+) operation")

CATEGORIES = [
    ("contaminated", "RUN CONTAMINATED: connection closed (backend or proxy died — rerun)"),
    ("upstream_bug", "Upstream s3-tests bug"),
    ("not_implemented", "Deliberate: unimplemented operation (501)"),
    ("input_probe_501", "Deliberate: 501 before input validation of an unimplemented operation"),
    ("sigv4_only", "Deliberate: SigV4-only / no anonymous access"),
    ("acl_write", "Deliberate: ACL writes refused (ACL-disabled bucket model)"),
    ("acl_read", "Deliberate: ACL-disabled model stubs (fixed FULL_CONTROL ACL, BucketOwnerEnforced ownership; name-heuristic)"),
    ("anti_probing", "Deliberate: 403 AccessDenied instead of 404 (anti-probing)"),
    ("access_denied", "AccessDenied — cross-tenant/policy semantics (name-heuristic)"),
    ("naming", "Deliberate: stricter bucket-name charset"),
    ("header_edge", "Deliberate/platform: request edge cases (Expect 417, chunked TE, negative presign expiry)"),
    ("s3_answer", "Deliberate: Amazon S3's answer where the suite expects RGW's"),
    ("backend_sse", "Backend: SSE/encrypted-copy behavior"),
    ("backend_checksum", "Backend: checksum/part-ETag semantics"),
    ("backend_conditional", "Backend: partial conditional-write enforcement"),
    ("local_s3", "In-process backend: Amazon S3's answer where the suite expects RGW's"),
    ("local_rgw", "In-process backend: an RGW behavior it does not imitate"),
    ("local_gateway", "In-process backend: the gateway's doing, sorted here by the test's name"),
    ("rgw_extension", "RGW extension API (out of scope)"),
    ("conf_artifact", "Harness/conf artifact (create semantics, api_name, leftovers)"),
    ("investigate", "UNMATCHED — triage by hand (see s3tests/AGENTS.md)"),
]

EXPECT_404 = re.compile(r"NoSuchBucket|NoSuchKey|404", re.I)  # against failure text
NAME_NONEXIST = re.compile(r"nonexist|not_?exist|no_?such", re.I)  # against test name
ACLISH = re.compile(r"acl|grant|ownership", re.I)
CHECKSUM = re.compile(r"[Cc]hecksum|cksum|x-amz-checksum")
CONDITIONAL = re.compile(r"if_?match|ifmatch|ifnonmatch|ifnonematch|ifmodifiedsince|delete_marker|conditional", re.I)
RGW_EXTENSION = re.compile(r"usage|account|head_extended|bucket_logging|x-rgw", re.I)
HEADER_EDGE = re.compile(r"bad_expect|chunked_transfer|bad_content(length|type)|bad_authorization|bad_date|bad_ua", re.I)
CONF_ARTIFACT = re.compile(r"bucket_recreate|get_location|list_buckets_paginated", re.I)


# Individually-documented cases (docs/s3-tests.md) that no general rule
# covers: this test's "expired" presign is a negative X-Amz-Expires,
# refused with 400 like AWS where RGW answers 403; and more than ten
# object tags are refused by the gateway with S3's BadRequest, where the
# suite expects RGW's InvalidTag.
KNOWN = {
    "test_object_raw_put_authenticated_expired": "header_edge",
    "test_put_excess_tags": "s3_answer",
}


# The categories that describe the backend. Their rules are name heuristics
# verified against Ceph RGW; for the in-process backend an exact list takes
# their place (expected-localbackend.tsv), since that backend's behavior
# changes with this repository's code and a heuristic would hide a
# regression in it.
BACKEND_KEYS = {"backend_sse", "backend_checksum", "backend_conditional", "investigate"}


def load_expected(path):
    expected = {}
    for line in open(path):
        if line.strip() and not line.startswith("#"):
            name, reason = line.split()[:2]
            if "local_" + reason not in dict(CATEGORIES):
                sys.exit(f"{path}: unknown reason {reason!r} for {name}")
            expected[name] = "local_" + reason
    return expected


def classify(name, text):
    if "ConnectionClosedError" in text or "ConnectionRefusedError" in text:
        return "contaminated", None
    if name in KNOWN:
        return KNOWN[name], None
    # test_bucket_create_exists reads e.status, which ClientError does not
    # have; it fails on any backend answering BucketAlreadyOwnedByYou
    if "object has no attribute 'status'" in text:
        return "upstream_bug", None
    m = CLIENT_ERROR.search(text)
    code, op = (m.group(1), m.group(2)) if m else (None, None)
    if code == "NotImplemented" or "NotImplemented" in text:
        return "not_implemented", op
    # input-validation probes of unimplemented operations expect 4xx and
    # meet the loud 501 first
    if re.search(r"assert 501 ==", text):
        return "input_probe_501", op
    # this suite revision signs every POST-object test with SigV2, and
    # anonymous requests have no principal in s3rp
    if name.startswith("test_post_object") or "anon" in name:
        return "sigv4_only", op
    if code == "AccessControlListNotSupported":
        return "acl_write", op
    if code == "InvalidBucketName":
        return "naming", op
    if code == "AccessDenied" or re.search(r"assert 403 ==", text):
        if NAME_NONEXIST.search(name) or EXPECT_404.search(text):
            return "anti_probing", op
        if ACLISH.search(name):
            return "acl_read", op
        return "access_denied", op
    # the fixed ownership stub answers BucketOwnerEnforced whatever
    # ObjectOwnership the test asked CreateBucket for
    if code is None and (ACLISH.search(name) or "BucketOwnerEnforced" in text):
        return "acl_read", op
    if HEADER_EDGE.search(name):
        return "header_edge", op
    if RGW_EXTENSION.search(name):
        return "rgw_extension", op
    # copy-of-encrypted refusals and unconfigured SSE flavors are the
    # backend's answer, verified by probing it directly
    if re.search(r"sse|_enc\b|copy_enc|copy_part_enc|encrypt", name):
        return "backend_sse", op
    if CHECKSUM.search(name) or (code is None and CHECKSUM.search(text)):
        return "backend_checksum", op
    if CONDITIONAL.search(name):
        return "backend_conditional", op
    if CONF_ARTIFACT.search(name) or code == "BucketAlreadyOwnedByYou":
        return "conf_artifact", op
    # part reads answer the part's ETag on RGW where AWS answers the
    # object's
    if re.search(r"get_part|multipart", name):
        return "backend_checksum", op
    return "investigate", code or "assertion"


def excerpt_of(text):
    lines = [l.strip() for l in text.splitlines() if l.strip()]
    for l in lines:
        if CLIENT_ERROR.search(l) or l.startswith("assert") or "Error" in l:
            return l[:200]
    return lines[0][:200] if lines else ""


def main():
    xmlfile = sys.argv[1]
    logfile = sys.argv[2] if len(sys.argv) > 2 else None
    # given for a run on the in-process backend
    expected = load_expected(sys.argv[3]) if len(sys.argv) > 3 else None
    hidden = BACKEND_KEYS - {"investigate"} if expected is not None else {"local_s3", "local_rgw", "local_gateway"}
    matched = set()
    root = ET.parse(xmlfile).getroot()

    total = passed = skipped = 0
    buckets = defaultdict(list)  # category -> [(test, subgroup, excerpt)]
    for case in root.iter("testcase"):
        total += 1
        name = case.get("name", "")
        fail = case.find("failure")
        err = case.find("error")
        if case.find("skipped") is not None:
            skipped += 1
            continue
        node = fail if fail is not None else err
        if node is None:
            passed += 1
            continue
        text = (node.get("message") or "") + "\n" + (node.text or "")
        cat, sub = classify(name, text)
        if expected is not None and cat in BACKEND_KEYS:
            cat = expected.get(name, "investigate")
            matched.add(name)
        buckets[cat].append((name, sub, excerpt_of(text)))

    failed = total - passed - skipped
    contaminated = buckets.get("contaminated", [])
    unmatched = buckets.get("investigate", [])
    # a listed test that does not fail any more, or fails for a reason of
    # the gateway's that the rules sort elsewhere
    stale = sorted(set(expected or {}) - matched)

    print("# s3-tests triage report\n")
    print(f"**{passed} passed / {failed} failed / {skipped} skipped** "
          f"(of {total} selected)\n")

    # ---- what to do, first ------------------------------------------------
    if contaminated:
        print(f"## 🛑 Rerun needed: the run is contaminated ({len(contaminated)})\n")
        print("The backend or the proxy stopped answering mid-run, so every")
        print("result after that point is unreliable — including the counts")
        print("above. Find the first casualty in `harness.log`, fix the")
        print("environment (a full MicroCeph answers `InsufficientCapacity`;")
        print("recreate it with `setup-microceph.sh`), and rerun before")
        print("reading anything else in this report.\n")
        for name, _, excerpt in sorted(contaminated):
            print(f"- `{name}` — {excerpt}")
        print()
    if unmatched:
        print(f"## ⚠️ Triage these by hand ({len(unmatched)})\n")
        print("No known pattern explains these failures: each one is either a")
        print("new incompatibility (possibly an s3rp bug) or a pattern the")
        print("classifier does not know yet. Follow s3tests/AGENTS.md — read")
        print("the failure, probe the backend directly to decide proxy vs")
        print("backend, then fix or teach triage.py the verdict.\n")
        for name, _, excerpt in sorted(unmatched):
            print(f"- `{name}` — {excerpt}")
        print()
    if stale:
        print(f"## ⚠️ Listed as expected, but not failing that way ({len(stale)})\n")
        print("These are in the in-process backend's list and passed, did not")
        print("run, or failed in a way the gateway's rules explain: remove")
        print("them from the list, or find out what moved.\n")
        for name in stale:
            print(f"- `{name}`")
        print()
    if not contaminated and not unmatched and not stale:
        print("## ✅ No action needed\n")
        print("Every failure matches the verified classification: the")
        print("deliberate design surface and documented backend behavior")
        print("(docs/s3-tests.md). Worth a look only if the pass count above")
        print("moved against the previous run — then check which expected")
        print("category below changed size, and spot-check its list: these")
        print("rules are name heuristics, and a *newly failing* test landing")
        print("in an old category is exactly what they can misfile.\n")

    # ---- the expected failures, collapsed ---------------------------------
    print("## Expected failures (no action)\n")
    print("| category | count |")
    print("|---|---|")
    for key, title in CATEGORIES:
        if key in ("contaminated", "investigate") or key in hidden:
            continue
        print(f"| {title} | {len(buckets.get(key, []))} |")
    print()
    for key, title in CATEGORIES:
        if key in ("contaminated", "investigate") or key in hidden:
            continue
        cases = buckets.get(key, [])
        if not cases:
            continue
        print(f"<details><summary>{title} ({len(cases)})</summary>\n")
        if key == "not_implemented":
            ops = Counter(sub or "?" for _, sub, _ in cases)
            print("by operation:", ", ".join(f"{o}×{n}" for o, n in ops.most_common()))
            print()
        for name, _, _ in sorted(cases):
            print(f"- `{name}`")
        print("\n</details>\n")

    if logfile:
        print(f"\nHarness/gateway request log: `{logfile}` "
              "(JSON lines; correlate by bucket name / x-amz-request-id).")

    # a run that needs a person fails, so a scheduled run says so
    sys.exit(1 if contaminated or unmatched or stale else 0)


if __name__ == "__main__":
    main()
