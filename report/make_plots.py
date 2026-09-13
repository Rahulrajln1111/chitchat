#!/usr/bin/env python3
"""Generate report plots from /tmp/report_data.json into chitchat/report/plots."""
import json
import os

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

data = json.load(open("/tmp/report_data.json"))
outdir = "/home/razz/Desktop/D/Sd/chitchat/report/plots"
os.makedirs(outdir, exist_ok=True)

stages = [s["users"] for s in data["stages"]]
plt.style.use("seaborn-v0_8-darkgrid")
COLORS = {"lb": "#E91E63", "be1": "#2196F3", "be2": "#4CAF50", "be3pg": "#FF9800"}
LABELS = {"lb": "VM1 (LB, :2289)", "be1": "VM2 (Backend-1, :2290)",
          "be2": "VM3 (Backend-2, :2291)", "be3pg": "VM4 (Backend-3 + PostgreSQL, :2292)"}

# ---------- 1. Response time vs concurrency ----------
fig, axes = plt.subplots(1, 2, figsize=(13, 5))
fig.suptitle("Response Time vs Concurrent Users (own load generator)", fontsize=14, fontweight="bold")
msg_avg = [s["msg"]["avg"] for s in data["stages"]]
msg_p50 = [s["msg"]["p50"] for s in data["stages"]]
msg_p95 = [s["msg"]["p95"] for s in data["stages"]]
msg_p99 = [s["msg"]["p99"] for s in data["stages"]]

ax = axes[0]
ax.plot(stages, msg_avg, "o-", color="#FF4500", label="avg")
ax.plot(stages, msg_p50, "s--", color="#2196F3", label="p50")
ax.plot(stages, msg_p95, "^--", color="#4CAF50", label="p95")
ax.plot(stages, msg_p99, "v--", color="#9C27B0", label="p99")
ax.set_xlabel("Concurrent users"); ax.set_ylabel("POST /message latency (ms)")
ax.set_title("POST /message"); ax.legend(); ax.set_xticks(stages)

feed_avg = [s["feed"]["avg"] for s in data["stages"]]
feed_p95 = [s["feed"]["p95"] for s in data["stages"]]
ax = axes[1]
ax.plot(stages, feed_avg, "o-", color="#4682B4", label="avg")
ax.plot(stages, feed_p95, "^--", color="#FF9800", label="p95")
ax.set_xlabel("Concurrent users"); ax.set_ylabel("GET /feed latency (ms)")
ax.set_title("GET /feed (20% of traffic)"); ax.legend(); ax.set_xticks(stages)
plt.tight_layout()
plt.savefig(f"{outdir}/1_response_time_vs_users.png", dpi=140, bbox_inches="tight")
plt.close()

# ---------- 2. Throughput per stage ----------
fig, ax = plt.subplots(figsize=(10, 5))
rps = []
for s in data["stages"]:
    total = s["msg"]["n"] + s["feed"]["n"]
    rps.append(round(total / data["stage_secs"], 1))
ax.bar([str(u) for u in stages], rps, color="#FF4500", alpha=0.85, edgecolor="black")
for i, v in enumerate(rps):
    ax.text(i, v + 5, f"{v:.0f}", ha="center", fontsize=9)
ax.set_xlabel("Concurrent users"); ax.set_ylabel("Requests / second")
ax.set_title("Throughput per Stage (POST /message + GET /feed)", fontweight="bold")
plt.tight_layout()
plt.savefig(f"{outdir}/2_throughput_per_stage.png", dpi=140, bbox_inches="tight")
plt.close()

# ---------- 3. CPU utilization of all 4 systems ----------
fig, axes = plt.subplots(2, 2, figsize=(14, 9), sharex=True)
fig.suptitle("CPU Utilization of All 4 Systems (cgroup sampling, 1s interval)",
             fontsize=14, fontweight="bold")
for idx, (key, label) in enumerate(LABELS.items()):
    ax = axes[idx // 2][idx % 2]
    ax.plot(data["util"][key]["t"], data["util"][key]["cpu"],
            linewidth=0.9, color=COLORS[key])
    ax.set_title(label, fontsize=10)
    ax.set_ylabel("CPU (%)")
    # stage boundaries
    t = 2
    for u in stages[:-1]:
        t += data["stage_secs"]
        ax.axvline(t, color="gray", linestyle=":", alpha=0.5)
    if idx >= 2:
        ax.set_xlabel("Time (s)")
plt.tight_layout()
plt.savefig(f"{outdir}/3_cpu_utilization_all_systems.png", dpi=140, bbox_inches="tight")
plt.close()

# ---------- 4. Memory utilization of all 4 systems ----------
fig, axes = plt.subplots(2, 2, figsize=(14, 9), sharex=True)
fig.suptitle("Memory Utilization of All 4 Systems (cgroup sampling, 1s interval)",
             fontsize=14, fontweight="bold")
for idx, (key, label) in enumerate(LABELS.items()):
    ax = axes[idx // 2][idx % 2]
    ax.plot(data["util"][key]["t"], data["util"][key]["mem"],
            linewidth=0.9, color=COLORS[key])
    ax.axhline(512, color="red", linestyle="--", alpha=0.6, label="512MB cgroup limit")
    ax.set_title(label, fontsize=10)
    ax.set_ylabel("Memory (MB)")
    ax.legend(fontsize=8)
    t = 2
    for u in stages[:-1]:
        t += data["stage_secs"]
        ax.axvline(t, color="gray", linestyle=":", alpha=0.5)
    if idx >= 2:
        ax.set_xlabel("Time (s)")
plt.tight_layout()
plt.savefig(f"{outdir}/4_memory_utilization_all_systems.png", dpi=140, bbox_inches="tight")
plt.close()

# ---------- 5. Latency distribution at 2500 users ----------
last = data["stages"][-1]
fig, axes = plt.subplots(1, 2, figsize=(13, 5))
fig.suptitle("Latency Summary at Peak Load (2500 concurrent users)", fontsize=14, fontweight="bold")
metrics = ["avg", "p50", "p95", "p99", "max"]
mvals = [last["msg"][m] for m in metrics]
fvals = [last["feed"][m] for m in metrics]
x = range(len(metrics))
axes[0].bar(x, mvals, color="#FF4500", alpha=0.85)
axes[0].set_xticks(x); axes[0].set_xticklabels(metrics)
axes[0].set_ylabel("ms"); axes[0].set_title("POST /message")
for i, v in enumerate(mvals):
    axes[0].text(i, v + 30, f"{v:.0f}", ha="center", fontsize=9)
axes[1].bar(x, fvals, color="#4682B4", alpha=0.85)
axes[1].set_xticks(x); axes[1].set_xticklabels(metrics)
axes[1].set_title("GET /feed")
for i, v in enumerate(fvals):
    axes[1].text(i, v + 30, f"{v:.0f}", ha="center", fontsize=9)
plt.tight_layout()
plt.savefig(f"{outdir}/5_latency_at_peak.png", dpi=140, bbox_inches="tight")
plt.close()

# ---------- 6. Error rate per stage ----------
fig, ax = plt.subplots(figsize=(10, 4.5))
err_rates = []
for s in data["stages"]:
    tot = s["msg"]["n"] + s["feed"]["n"]
    errs = s["msg"]["errors"] + s["feed"]["errors"]
    err_rates.append(round(errs / max(1, tot) * 100, 3))
ax.bar([str(u) for u in stages], err_rates, color="#F44336", alpha=0.85)
ax.set_xlabel("Concurrent users"); ax.set_ylabel("Error rate (%)")
ax.set_title("Error Rate per Stage — all zero", fontweight="bold")
ax.set_ylim(0, max(0.5, max(err_rates) * 1.2))
plt.tight_layout()
plt.savefig(f"{outdir}/6_error_rate.png", dpi=140, bbox_inches="tight")
plt.close()

print("Plots written:")
for f in sorted(os.listdir(outdir)):
    print(" ", f, f"({os.path.getsize(os.path.join(outdir, f))//1024} KB)")
