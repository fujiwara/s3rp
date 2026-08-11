#!/usr/bin/env python3
"""Classify a ceph/s3-tests junit XML result into compatibility categories.

Usage: triage.py results.xml [harness.log] > report.md

Failures are bucketed by the first matching rule; the buckets marked
"needs confirmation" use name heuristics and deserve a manual pass. The
"needs investigation" bucket is the interesting one: candidate bugs.
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
    ("acl_write", "Deliberate: ACL writes refused (ACL-disabled bucket model)"),
    ("acl_read", "Deliberate: ACL read stub (fixed FULL_CONTROL) — needs confirmation"),
    ("anti_probing", "Deliberate: 403 AccessDenied instead of 404 (anti-probing) — needs confirmation"),
    ("access_denied", "AccessDenied — cross-tenant/policy semantics, needs confirmation"),
    ("naming", "Deliberate: stricter bucket-name charset"),
    ("checksum", "Backend limitation: ceph demo RGW does not store checksums"),
    ("investigate", "NEEDS INVESTIGATION (candidate bugs)"),
]

EXPECT_404 = re.compile(r"NoSuchBucket|NoSuchKey|404", re.I)  # against failure text
NAME_NONEXIST = re.compile(r"nonexist|not_?exist|no_?such", re.I)  # against test name
ACLISH = re.compile(r"acl|grant|ownership", re.I)
CHECKSUM = re.compile(r"[Cc]hecksum|x-amz-checksum")


def classify(name, text):
    if "ConnectionClosedError" in text or "ConnectionRefusedError" in text:
        return "contaminated", None
    # test_bucket_create_exists reads e.status, which ClientError does not
    # have; it fails on any backend answering BucketAlreadyOwnedByYou
    if "object has no attribute 'status'" in text:
        return "upstream_bug", None
    m = CLIENT_ERROR.search(text)
    code, op = (m.group(1), m.group(2)) if m else (None, None)
    if code == "NotImplemented":
        return "not_implemented", op
    if code == "AccessControlListNotSupported":
        return "acl_write", op
    if code == "InvalidBucketName":
        return "naming", op
    if code == "AccessDenied":
        if NAME_NONEXIST.search(name) or EXPECT_404.search(text):
            return "anti_probing", op
        if ACLISH.search(name):
            return "acl_read", op
        return "access_denied", op
    if code is None and ACLISH.search(name):
        return "acl_read", op
    if CHECKSUM.search(name) or (code is None and CHECKSUM.search(text)):
        return "checksum", op
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
        buckets[cat].append((name, sub, excerpt_of(text)))

    print("# s3-tests triage report\n")
    print(f"- total: {total}  passed: {passed}  skipped: {skipped}  "
          f"failed/errored: {total - passed - skipped}\n")
    print("| category | count |")
    print("|---|---|")
    for key, title in CATEGORIES:
        print(f"| {title} | {len(buckets.get(key, []))} |")
    print()

    for key, title in CATEGORIES:
        cases = buckets.get(key, [])
        if not cases:
            continue
        print(f"## {title} ({len(cases)})\n")
        if key == "not_implemented":
            ops = Counter(sub or "?" for _, sub, _ in cases)
            print("by operation:", ", ".join(f"{o}×{n}" for o, n in ops.most_common()))
            print()
        for name, sub, excerpt in sorted(cases):
            if key == "investigate":
                print(f"- `{name}` — {excerpt}")
            else:
                print(f"- `{name}`")
        print()

    if logfile:
        print(f"\nHarness/gateway request log: `{logfile}` "
              "(JSON lines; correlate by bucket name / x-amz-request-id).")


if __name__ == "__main__":
    main()
