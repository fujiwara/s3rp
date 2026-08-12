#!/usr/bin/env python3
"""Aggregate warp analyze JSON + pidstat logs from bench/out into bench/report.md."""

import os
import platform
import re
import subprocess
from collections import defaultdict
from datetime import date

OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "out")
SCENARIOS = [
    ("put-proxy", "PUT (via s3rp)"),
    ("put-direct", "PUT (RGW direct)"),
    ("get-proxy", "GET (via s3rp)"),
    ("get-direct", "GET (RGW direct)"),
    ("multipart-proxy", "Multipart (via s3rp)"),
    ("multipart-direct", "Multipart (RGW direct)"),
]
MAIN_OP = {"put": "PUT", "get": "GET", "multipart": "PUTPART"}


def sh(*cmd):
    try:
        return subprocess.run(cmd, capture_output=True, text=True).stdout.strip()
    except OSError:
        return "unknown"


UNIT = {"KiB": 1 / 1024, "MiB": 1, "GiB": 1024, "TiB": 1 << 20}


def ms(value, unit):
    return float(value) * (1000 if unit == "s" else 1)


def load_warp(name):
    """Parse warp's stdout report (out/<name>.txt) for the main op.

    The v2 benchdata json only carries 10s-windowed request stats, so the
    whole-run latency percentiles exist only in warp's own printed report.
    """
    path = os.path.join(OUT, f"{name}.txt")
    if not os.path.exists(path):
        return None
    with open(path) as f:
        text = f.read()
    want = MAIN_OP[name.split("-")[0]]
    m = re.search(rf"^Report: {want}\b.*?(?=^Report: |\Z)", text,
                  re.MULTILINE | re.DOTALL)
    if m is None:
        return None
    section = m.group(0)
    tp = re.search(r"Average: ([\d.]+) (KiB|MiB|GiB|TiB)/s, ([\d.]+) obj/s", section)
    lat = re.search(r"Reqs: Avg: ([\d.]+)(ms|s), 50%: ([\d.]+)(ms|s), "
                    r"90%: ([\d.]+)(ms|s), 99%: ([\d.]+)(ms|s)", section)
    if tp is None:
        return None
    return {
        "mibps": float(tp.group(1)) * UNIT[tp.group(2)],
        "ops": float(tp.group(3)),
        "lat_avg": ms(lat.group(1), lat.group(2)) if lat else None,
        "lat_p50": ms(lat.group(3), lat.group(4)) if lat else None,
        "lat_p90": ms(lat.group(5), lat.group(6)) if lat else None,
        "lat_p99": ms(lat.group(7), lat.group(8)) if lat else None,
    }


def load_cpu(name):
    """Parse a pidstat -h -u log: per-command avg/max of summed %CPU per sample."""
    path = os.path.join(OUT, f"cpu-{name}.log")
    if not os.path.exists(path):
        return {}
    cols = None
    per_tick = defaultdict(lambda: defaultdict(float))  # time -> comm -> %CPU sum
    with open(path) as f:
        for line in f:
            line = line.strip()
            if line.startswith("#"):
                cols = line.lstrip("#").split()
                continue
            if not line or not cols:
                continue
            fields = line.split()
            if len(fields) < len(cols):
                continue
            row = dict(zip(cols, fields))
            try:
                cpu = float(row["%CPU"])
            except (KeyError, ValueError):
                continue
            comm = fields[-1]
            base = re.sub(r"[^a-z0-9].*$", "", comm) or comm  # radosgw stays radosgw
            per_tick[row.get("Time", fields[0])][base] += cpu
    stats = {}
    comms = {c for tick in per_tick.values() for c in tick}
    for comm in comms:
        # a missing sample means the process used ~0% that second
        vals = [tick.get(comm, 0.0) for tick in per_tick.values()]
        if vals:
            stats[comm] = (sum(vals) / len(vals), max(vals))
    return stats


def fmt(v, digits=1):
    return f"{v:.{digits}f}" if isinstance(v, (int, float)) else "-"


def cpu_cell(stats, comm):
    if comm not in stats:
        return "-"
    avg, mx = stats[comm]
    return f"{avg:.0f} / {mx:.0f}"


def main():
    cpu_model = "unknown"
    with open("/proc/cpuinfo") as f:
        for line in f:
            if line.startswith("model name"):
                cpu_model = line.split(":", 1)[1].strip()
                break
    nproc = os.cpu_count()
    commit = sh("git", "rev-parse", "--short", "HEAD")
    gover = sh("go", "version")

    env = {k: os.environ.get(k, d) for k, d in [
        ("DURATION", "20s"), ("CONCURRENT", "8"), ("OBJ_SIZE", "1MiB"),
        ("GET_OBJECTS", "500"), ("PART_SIZE", "5MiB"), ("PARTS", "50"),
        ("S3RP_LOG_LEVEL", "warn"),
    ]}

    lines = []
    add = lines.append
    add("# s3rp benchmark report")
    add("")
    add(f"- Date: {date.today().isoformat()}")
    add(f"- Host: {cpu_model} ({nproc} cores), kernel {platform.release()}")
    add(f"- s3rp: commit {commit}, {gover}")
    add("- Backend: microceph RGW at 127.0.0.1:7490 (same host, loopback-file OSDs)")
    add(f"- warp: obj.size={env['OBJ_SIZE']}, duration={env['DURATION']}, "
        f"concurrent={env['CONCURRENT']}, multipart part.size={env['PART_SIZE']} "
        f"x {env['PARTS']} parts/client, get objects={env['GET_OBJECTS']}")
    add(f"- s3rp log level: {env['S3RP_LOG_LEVEL']} (access log suppressed)")
    add("")
    add("## Results")
    add("")
    add("| Scenario | MiB/s | obj/s | avg (ms) | p50 | p90 | p99 | s3rp CPU% avg/max | radosgw CPU% avg/max | warp CPU% avg |")
    add("|---|---|---|---|---|---|---|---|---|---|")
    results = {}
    for name, label in SCENARIOS:
        w = load_warp(name)
        c = load_cpu(name)
        results[name] = w
        if w is None:
            add(f"| {label} | - | - | - | - | - | - | - | - | - |")
            continue
        warp_avg = fmt(c["warp"][0], 0) if "warp" in c else "-"
        add(f"| {label} | {fmt(w['mibps'])} | {fmt(w['ops'])} | {fmt(w['lat_avg'])} "
            f"| {fmt(w['lat_p50'])} | {fmt(w['lat_p90'])} | {fmt(w['lat_p99'])} "
            f"| {cpu_cell(c, 's3rp')} | {cpu_cell(c, 'radosgw')} | {warp_avg} |")
    add("")
    add("## Proxy overhead (via s3rp vs RGW direct)")
    add("")
    add("| Workload | direct MiB/s | via s3rp MiB/s | throughput ratio | direct p50 (ms) | via s3rp p50 (ms) |")
    add("|---|---|---|---|---|---|")
    for wl in ("put", "get", "multipart"):
        p, d = results.get(f"{wl}-proxy"), results.get(f"{wl}-direct")
        if not p or not d or not d["mibps"]:
            add(f"| {wl} | - | - | - | - | - |")
            continue
        add(f"| {wl} | {fmt(d['mibps'])} | {fmt(p['mibps'])} | {p['mibps'] / d['mibps']:.2f} "
            f"| {fmt(d['lat_p50'])} | {fmt(p['lat_p50'])} |")
    add("")
    add("## Notes")
    add("")
    add("- warp, s3rp and radosgw all run on the same host and compete for CPU; "
        "absolute numbers are indicative only.")
    add("- Proxy scenarios run before the direct baselines, so RGW background "
        "work (gc of the earlier scenarios' deleted data) can depress the "
        "later direct runs — a write ratio above 1.0 means that, not that "
        "the proxy adds throughput.")
    add("- Disk IO and network are not representative on this machine and are "
        "deliberately not measured.")
    add("- CPU% is per process (all threads summed), sampled by pidstat at 1s "
        "intervals; 100% = one core.")
    add("- Raw data: `bench/out/` (warp benchdata, per-scenario warp output, pidstat logs).")
    add("")

    path = os.path.join(os.path.dirname(OUT), "report.md")
    with open(path, "w") as f:
        f.write("\n".join(lines))
    print(f"wrote {path}")


if __name__ == "__main__":
    main()
