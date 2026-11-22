#!/usr/bin/env python3
"""Compares a fidelity-test run with the expected failures.

usage: triage.py REPORTS_DIR EXPECTED_TSV SELECTED_TXT > report.md

expected.tsv lists every test known to fail and why (check id, category),
so the report has two lists that need a person: failures the file does not
expect, and expected failures that passed. Exits 1 when either is
non-empty or a selected test produced no report. An entry whose third
column is "flaky" depends on timing and may pass.
"""

import json
import pathlib
import re
import sys

CATEGORIES = [
    ("backend", "The backend differs from S3"),
    ("message", "Cause-specific message: the backend's wording is not relayed"),
    ("deliberate", "Deliberate difference of the gateway"),
    ("out-of-scope", "Feature outside the gateway's scope"),
    ("gateway", "Gap in the gateway, not fixed yet"),
]


def detail_tail(check):
    detail = check.get("detail")
    if not isinstance(detail, str):
        detail = json.dumps(detail) if detail else ""
    detail = re.sub(r"\s+", " ", detail)
    found = re.search(r"(AssertionError|ClientError|KeyError)\b.*", detail)
    return (found.group(0) if found else detail[-200:])[:300].replace("|", "\\|")


def main():
    reports_dir, expected_path, selected_path = sys.argv[1:4]
    expected = {}
    flaky = set()
    for line in pathlib.Path(expected_path).read_text().splitlines():
        if line.strip() and not line.startswith("#"):
            fields = line.split("\t")
            check_id, category = fields[:2]
            expected[check_id] = category
            if fields[2:3] == ["flaky"]:
                flaky.add(check_id)
    known = {name for name, _ in CATEGORIES}
    unknown = sorted(set(expected.values()) - known)
    if unknown:
        sys.exit(f"expected.tsv uses unknown categories: {unknown}")

    checks = {}
    for path in sorted(pathlib.Path(reports_dir).glob("*.json")):
        check = json.loads(path.read_text())["check"]
        checks[check["check_id"]] = check
    selected = [line for line in pathlib.Path(selected_path).read_text().splitlines() if line]

    passed = {cid for cid, check in checks.items() if check["verdict"] == "passed"}
    failed = set(checks) - passed
    unmatched = sorted(failed - set(expected))
    fixed = sorted(passed & set(expected) - flaky)
    stale = sorted(set(expected) - set(checks))
    missing = len(selected) - len(checks)

    print("## AWS fidelity tests (S3)\n")
    print(f"- selected: {len(selected)}, reported: {len(checks)}")
    print(f"- passed: {len(passed)}, failed: {len(failed)}")
    print(f"- **unexpected failures: {len(unmatched)}**, expected failures that passed: {len(fixed)}\n")

    if missing:
        print(f"### {missing} selected tests produced no report\n\nSee the per-test logs.\n")
    if unmatched:
        print("### UNMATCHED — failures expected.tsv does not list\n")
        print("| test | failure |\n|---|---|")
        for cid in unmatched:
            print(f"| `{cid}` | {detail_tail(checks[cid])} |")
        print()
    if fixed:
        print("### Expected failures that passed — remove them from expected.tsv\n")
        for cid in fixed:
            print(f"- `{cid}` ({expected[cid]})")
        print()
    if stale:
        print("### Listed in expected.tsv but not run\n")
        for cid in stale:
            print(f"- `{cid}`")
        print()

    print("### Expected failures\n")
    print("| category | count |\n|---|---|")
    for name, title in CATEGORIES:
        count = sum(1 for cid in failed if expected.get(cid) == name)
        print(f"| {title} | {count} |")
    for name, title in CATEGORIES:
        rows = sorted(cid for cid in failed if expected.get(cid) == name)
        if not rows:
            continue
        print(f"\n<details><summary>{title} ({len(rows)})</summary>\n")
        print("| test | failure |\n|---|---|")
        for cid in rows:
            print(f"| `{cid}` | {detail_tail(checks[cid])} |")
        print("\n</details>")

    sys.exit(1 if unmatched or fixed or missing else 0)


if __name__ == "__main__":
    main()
