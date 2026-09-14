#!/usr/bin/env python3
"""chitchat load generator (assignment deliverable).

Features (per assignment requirements):
  - variable number of concurrent users (--users, or --ramp 100,500,1000)
  - random/variable message lengths (--msg-min/--msg-max, mixed content styles)
  - random/variable time intervals between messages per user (--think-min/--think-max ms)
  - exercises the two required routes: POST /message and GET /feed
  - per-request latency capture -> p50/p90/p95/p99 summary + raw JSON export
  - optional utilization sampling of all 4 backend/LB VMs over SSH (--sample-util)

Examples:
  python3 loadgen.py --url http://10.1.75.51:3289 --users 500 --duration 60
  python3 loadgen.py --ramp 100,500,1000 --duration 40 --sample-util --output results.json
"""
import argparse
import asyncio
import json
import os
import random
import string
import time

try:
    import aiohttp
except ImportError:
    raise SystemExit("pip install aiohttp")

# ---------------------------------------------------------------- message pool
WORDS = ["hello", "world", "chitchat", "load", "test", "message", "room",
         "chat", "system", "performance", "-latency-", "ünïcödé", "中文", "🎉"]
HTMLISH = "<b>bold</b> &amp; {n}"
QUOTY = 'she said "hi" and it\'s fine {n}'


def random_message(rng: random.Random, lo: int, hi: int) -> str:
    """Random-length message mixing plain words, unicode, quotes, HTML, newlines."""
    style = rng.randrange(5)
    target = rng.randint(lo, hi)
    if style == 0:  # word salad
        parts = []
        n = 0
        while n < target:
            w = rng.choice(WORDS)
            parts.append(w)
            n += len(w) + 1
        return " ".join(parts)[:target]
    if style == 1:  # random printable
        return "".join(rng.choice(string.ascii_letters + string.digits + " ")
                       for _ in range(target))
    if style == 2:  # multiline
        lines = target // 12 + 1
        return "\n".join(f"line{j} " + "x" * rng.randint(1, 8)
                         for j in range(lines))[:target]
    if style == 3:
        return (QUOTY.format(n=rng.randrange(10**6)) + " " +
                "y" * max(0, target - 40))[:target]
    return (HTMLISH.format(n=rng.randrange(10**6)) + " " +
            "z" * max(0, target - 30))[:target]


# ---------------------------------------------------------------- stats
class Stats:
    def __init__(self):
        self.ok = 0
        self.err = 0
        self.errs = {}
        self.lat = []          # seconds, successful posts
        self.feed_lat = []     # seconds, successful feeds
        self.ids = []          # accepted message ids (for completeness checks)

    def add_err(self, key):
        self.err += 1
        self.errs[key] = self.errs.get(key, 0) + 1

    def summary(self, wall):
        lat_ms = sorted(x * 1000 for x in self.lat)
        feed_ms = sorted(x * 1000 for x in self.feed_lat)

        def pct(arr, p):
            if not arr:
                return None
            i = min(len(arr) - 1, int(len(arr) * p / 100))
            return round(arr[i], 1)

        total = self.ok + self.err
        return {
            "window_seconds": round(wall, 1),
            "requests": total,
            "success": self.ok,
            "errors": self.err,
            "error_rate": round(self.err / total, 4) if total else 0.0,
            "throughput_rps": round(total / wall, 1) if wall else 0.0,
            "message_latency_ms": {
                "avg": round(sum(lat_ms) / len(lat_ms), 1) if lat_ms else None,
                "p50": pct(lat_ms, 50), "p90": pct(lat_ms, 90),
                "p95": pct(lat_ms, 95), "p99": pct(lat_ms, 99),
                "max": lat_ms[-1] if lat_ms else None,
            },
            "feed_latency_ms": {
                "avg": round(sum(feed_ms) / len(feed_ms), 1) if feed_ms else None,
                "p50": pct(feed_ms, 50), "p99": pct(feed_ms, 99),
            },
            "error_breakdown": self.errs,
            "accepted_ids": len(self.ids),
        }


# ---------------------------------------------------------------- util sampler
def sample_util_once(port, password):
    """cgroup cpu%, memory MB for one VM. Returns dict or None."""
    import paramiko
    try:
        s = paramiko.SSHClient()
        s.set_missing_host_key_policy(paramiko.AutoAddPolicy())
        s.connect("10.1.75.51", port=port, username="student",
                  password=password, timeout=8)
        _, out, _ = s.exec_command(
            "cat /sys/fs/cgroup/memory.current;"
            "cat /sys/fs/cgroup/cpu.stat", timeout=8)
        lines = out.read().decode().split()
        s.close()
        mem = int(lines[0])
        cpu = {}
        for i in range(1, len(lines), 2):
            cpu[lines[i]] = int(lines[i + 1])
        return {"mem": mem, "usage_usec": cpu.get("usage_usec", 0)}
    except Exception:
        return None


async def util_sampler(ports_names, password, interval, store, stop_evt, loop):
    prev = {n: sample_util_once(p, password) for p, n in ports_names}
    t_prev = loop.time()
    while not stop_evt.is_set():
        await asyncio.sleep(interval)
        now = {}
        t_now = loop.time()
        dt = t_now - t_prev
        for p, n in ports_names:
            cur = sample_util_once(p, password)
            if cur and prev.get(n):
                du = cur["usage_usec"] - prev[n]["usage_usec"]
                cpu_pct = (du / 1e6) / dt * 100.0
                store[n].append({"t": round(t_now, 1),
                                 "cpu_pct": round(cpu_pct, 1),
                                 "mem_mb": round(cur["mem"] / 1e6, 1)})
            prev[n] = cur
        t_prev = t_now


# ---------------------------------------------------------------- workers
async def user_worker(idx, sess, url, think_lo, think_hi, msg_lo, msg_hi,
                      feed_every, stats, deadline, rng):
    while time.monotonic() < deadline:
        payload = {
            "client-name": f"lg-user{idx % 200}",
            "msg": random_message(rng, msg_lo, msg_hi),
        }
        t0 = time.monotonic()
        try:
            async with sess.post(url + "/message", json=payload) as r:
                await r.read()
                dt = time.monotonic() - t0
                if r.status == 200:
                    stats.ok += 1
                    stats.lat.append(dt)
                    try:
                        mid = (await r.json())["id"]
                        stats.ids.append(mid)
                    except Exception:
                        pass
                else:
                    stats.add_err(f"http_{r.status}")
        except Exception as e:
            stats.add_err(type(e).__name__)

        if feed_every and idx % feed_every == 0:
            t0 = time.monotonic()
            try:
                async with sess.get(url + "/feed") as r:
                    await r.read()
                    dt = time.monotonic() - t0
                    if r.status == 200:
                        stats.feed_lat.append(dt)
                    else:
                        stats.add_err(f"feed_{r.status}")
            except Exception as e:
                stats.add_err("feed_" + type(e).__name__)

        # random think time between messages
        if think_hi > 0:
            await asyncio.sleep(rng.uniform(think_lo, think_hi) / 1000.0)


async def run(args, stats):
    ramp = [int(x) for x in args.ramp.split(",")] if args.ramp else [args.users]
    stop_evt = asyncio.Event()
    util_store = {n: [] for _, n in U_PORTS} if args.sample_util else None

    tasks = []
    if args.sample_util:
        tasks.append(asyncio.create_task(util_sampler(
            U_PORTS, args.ssh_password, 1.0, util_store, stop_evt,
            asyncio.get_event_loop())))

    conn = aiohttp.TCPConnector(limit=0, force_close=args.no_keepalive)
    to = aiohttp.ClientTimeout(total=args.timeout, sock_connect=10)
    async with aiohttp.ClientSession(connector=conn, timeout=to) as sess:
        for wave, users in enumerate(ramp):
            deadline = time.monotonic() + args.duration
            print(f"[wave {wave + 1}/{len(ramp)}] {users} users for "
                  f"{args.duration}s (think {args.think_min}-{args.think_max}ms, "
                  f"msg {args.msg_min}-{args.msg_max}B)", flush=True)
            rng = random.Random(wave * 7919 + 13)
            workers = [asyncio.create_task(user_worker(
                i + wave * 100000, sess, args.url, args.think_min,
                args.think_max, args.msg_min, args.msg_max, args.feed_every,
                stats, deadline, rng)) for i in range(users)]
            await asyncio.gather(*workers)
            s = stats.summary(args.duration)
            print("  ->", json.dumps({k: s[k] for k in
                  ("requests", "success", "errors", "throughput_rps")}),
                  "| p50/p95/p99:",
                  s["message_latency_ms"]["p50"],
                  s["message_latency_ms"]["p95"],
                  s["message_latency_ms"]["p99"], flush=True)
    stop_evt.set()
    await asyncio.gather(*tasks, return_exceptions=True)

    out = {"args": vars(args), "summary": stats.summary(args.duration),
           "utilization": util_store}
    if args.output:
        with open(args.output, "w") as f:
            json.dump(out, f)
        print("wrote", args.output)
    return out


U_PORTS = [(2289, "lb"), (2290, "be1"), (2291, "be2"), (2292, "be3_pg")]


def main():
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--url", default="http://10.1.75.51:3289")
    p.add_argument("--users", type=int, default=100)
    p.add_argument("--ramp", default=None,
                   help="comma list of user waves, e.g. 100,500,1000")
    p.add_argument("--duration", type=int, default=30,
                   help="seconds per wave")
    p.add_argument("--think-min", type=int, default=0,
                   help="min ms between messages per user")
    p.add_argument("--think-max", type=int, default=50,
                   help="max ms between messages per user")
    p.add_argument("--msg-min", type=int, default=10,
                   help="min message length (bytes)")
    p.add_argument("--msg-max", type=int, default=500,
                   help="max message length (bytes)")
    p.add_argument("--feed-every", type=int, default=25,
                   help="each Nth user also GETs /feed (0 disables)")
    p.add_argument("--timeout", type=int, default=30)
    p.add_argument("--no-keepalive", action="store_true",
                   help="fresh TCP connection per request")
    p.add_argument("--sample-util", action="store_true",
                   help="sample cgroup CPU/mem of all 4 VMs (needs paramiko)")
    p.add_argument("--ssh-password", default=os.environ.get("VM_PW", ""))
    p.add_argument("--output", default=None, help="write JSON results here")
    args = p.parse_args()

    if args.sample_util and not args.ssh_password:
        raise SystemExit("--sample-util needs --ssh-password or VM_PW env")

    stats = Stats()
    asyncio.run(run(args, stats))


if __name__ == "__main__":
    main()
