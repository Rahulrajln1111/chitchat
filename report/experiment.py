#!/usr/bin/env python3
"""Report experiment harness: staged load experiment + utilization sampling
of all 4 VMs over SSH.

Stages: 100 -> 250 -> 500 -> 1000 -> 1500 -> 2000 -> 2500 concurrent users.
Each stage: N users (closed-loop) POST /message with RANDOM-LENGTH messages
at RANDOM intervals (0-50ms think time), plus 20% GET /feed, for STAGE_SECS.

Utilization sampled every 1s on all 4 VMs (cgroup CPU %, memory MB, loadavg).
Outputs: /tmp/report_data.json with per-stage latency stats and utilization
timelines for the 4 systems (lb-2289, be1-2290, be2-2291, be3+pg-2292).
"""
import asyncio
import aiohttp
import json
import random
import threading
import time
import paramiko

BASE = "http://10.1.75.51:3289"
PASSWORD = "WqyU7484"
HOST = "10.1.75.51"
STAGES = [100, 250, 500, 1000, 1500, 2000, 2500]
STAGE_SECS = 20

VMS = [(2289, "lb"), (2290, "be1"), (2291, "be2"), (2292, "be3pg")]

util = {name: {"t": [], "cpu": [], "mem": []} for _, name in VMS}
util_stop = threading.Event()
util_lock = threading.Lock()


def _ssh(port):
    ssh = paramiko.SSHClient()
    ssh.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    ssh.connect(HOST, port=port, username="student", password=PASSWORD, timeout=15)
    return ssh


def sample_vm(port, name):
    """Sample cgroup CPU% and memory MB every second on one VM."""
    ssh = _ssh(port)
    prev = None
    while not util_stop.is_set():
        try:
            _, o, _ = ssh.exec_command(
                "cat /sys/fs/cgroup/memory.current; cat /sys/fs/cgroup/cpu.stat",
                timeout=8)
            toks = o.read().decode().split()
            # tokens: [mem_bytes, usage_usec, N, user_usec, N, ...]
            mem_mb = int(toks[0]) / 1048576
            use_us = int(toks[2])
            now = time.monotonic()
            if prev is not None:
                duse = use_us - prev[0]
                dt = (now - prev[1]) * 1_000_000
                cpu_pct = (duse / dt * 100) if dt > 0 else 0.0
            else:
                cpu_pct = 0.0
            prev = (use_us, now)
            ts = now - T0
            with util_lock:
                util[name]["t"].append(round(ts, 1))
                util[name]["cpu"].append(round(min(cpu_pct, 110.0), 1))
                util[name]["mem"].append(round(mem_mb, 1))
        except Exception as e:
            print(f"  [sampler {name}] {e}", flush=True)
        time.sleep(1)
    ssh.close()


lat = {"msg": [], "feed": []}
sent = {}
lat_lock = threading.Lock()

WORDS = ["hello", "chat", "load", "test", "message", "group", "secure",
         "system", " distributed ", "network", "server", "client", "ping"]


def random_msg():
    """Random/variable message length: 5-300 chars from word pool."""
    n = random.randint(1, 25)
    return " ".join(random.choice(WORDS) for _ in range(n))[:300]


async def user_loop(uid, sem, deadline):
    """One user: random message at random intervals (closed loop)."""
    async with sem:
        while time.monotonic() < deadline:
            msg = random_msg()
            t0 = time.monotonic()
            try:
                async with aiohttp.ClientSession(
                        connector=aiohttp.TCPConnector(limit=0, force_close=True)) as one:
                    if random.random() < 0.2:
                        async with one.get(f"{BASE}/feed",
                                           timeout=aiohttp.ClientTimeout(total=30)) as r:
                            await r.read()
                            dt = (time.monotonic() - t0) * 1000
                            with lat_lock:
                                lat["feed"].append((round(dt, 1), r.status))
                    else:
                        async with one.post(f"{BASE}/message",
                                            json={"client-name": f"user-{uid}", "msg": msg},
                                            timeout=aiohttp.ClientTimeout(total=30)) as r:
                            b = await r.json()
                            dt = (time.monotonic() - t0) * 1000
                            with lat_lock:
                                lat["msg"].append((round(dt, 1), r.status))
                                if r.status == 200 and b.get("id"):
                                    sent[b["id"]] = msg
            except Exception:
                dt = (time.monotonic() - t0) * 1000
                with lat_lock:
                    (lat["feed"] if random.random() < 0.2 else lat["msg"]).append((round(dt, 1), 0))
            # random/variable interval between messages: 0-50 ms
            await asyncio.sleep(random.uniform(0, 0.05))


def pct(sorted_vals, p):
    if not sorted_vals:
        return 0
    return sorted_vals[min(int(len(sorted_vals) * p), len(sorted_vals) - 1)]


T0 = time.monotonic()

async def run_stage(conc):
    sem = asyncio.Semaphore(conc)
    deadline = time.monotonic() + STAGE_SECS
    before = {k: len(v) for k, v in lat.items()}
    await asyncio.gather(*[user_loop(i, sem, deadline) for i in range(conc)])
    out = {}
    for k in ("msg", "feed"):
        rows = lat[k][before[k]:]
        vals = sorted(r[0] for r in rows)
        errs = sum(1 for _, s in rows if s != 200)
        out[k] = {
            "n": len(vals), "errors": errs,
            "avg": round(sum(vals) / len(vals), 1) if vals else 0,
            "p50": pct(vals, 0.50), "p95": pct(vals, 0.95), "p99": pct(vals, 0.99),
            "max": vals[-1] if vals else 0,
        }
    return out


async def main():
    results = {"stages": [], "stage_secs": STAGE_SECS}
    threads = [threading.Thread(target=sample_vm, args=(p, n), daemon=True)
               for p, n in VMS]
    for t in threads:
        t.start()
    time.sleep(2)

    for conc in STAGES:
        print(f"stage users={conc} ...", flush=True)
        r = await run_stage(conc)
        r["users"] = conc
        results["stages"].append(r)
        m = r["msg"]
        print(f"  msg n={m['n']} err={m['errors']} avg={m['avg']}ms "
              f"p50={m['p50']} p95={m['p95']} p99={m['p99']}", flush=True)

    time.sleep(2)
    util_stop.set()
    time.sleep(2)
    with util_lock:
        results["util"] = {k: dict(v) for k, v in util.items()}
    results["sent_count"] = len(sent)
    json.dump(results, open("/tmp/report_data.json", "w"))
    json.dump(sent, open("/tmp/report_sent.json", "w"))
    print(f"TOTAL accepted: {len(sent)}")


asyncio.run(main())
