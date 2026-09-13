# Secure Group-Chat with Performance-Based Load Balancing — Project Report

**Student:** Rahul Razz (rahulrajln1111@gmail.com)
**Load Balancer URL (submission endpoint):** `http://10.1.75.51:3289`

---

## 1. Overview

This project extends our previous **secure, persistent group-chat application** ("ChitChat") and deploys its backend across **3 backend systems behind a custom Go load balancer** (a 4th system). Clients access **only** the Load Balancer URL — the required API routes `/message` and `/feed` are preserved at their exact paths, and **no existing functionality of the chat application was removed or simplified** to achieve load performance. All original features remain: registration/login with JWT sessions, chat rooms (create/join by room ID), real-time messaging over WebSockets across backends, and at-rest message encryption (AES-256-GCM with wrapped Ed25519 user signing keys, ported from the Java implementation).

The workload API required by the assignment:

| Route | Method | Behavior |
|---|---|---|
| `/message` | POST | Accepts `client-name` and `msg` (JSON), submits a message. Returns `{"id": "<uuid>", "status": "stored"}`. Optional client-supplied `id` makes retries idempotent. |
| `/feed` | GET | Retrieves all submitted messages as a JSON array (streamed). |

The LB URL also serves the full chat frontend at `/` and the chat WebSocket at `/ws`, so the same public endpoint hosts both the graded workload API and the original application.

## 2. Architecture

```
                    clients (load generator / browsers)
                              |
                              v
              http://10.1.75.51:3289  (NAT -> VM1:3000)
                    +----------------------+
                    | VM1 : Load Balancer  |   Go net/http reverse proxy
                    | - performance sched. |   least-load (power-of-two-choices)
                    | - active health chk  |   1s probes, 3 fails => down
                    | - passive breaker    |   conn-refused => instant eviction
                    | - failover retry     |   POST /message body-replay retry
                    +----------+-----------+
        +----------------------++----------------------+
        v                      v                      v
  VM2 : Backend-1        VM3 : Backend-2        VM4 : Backend-3
  Go chitchat-server     Go chitchat-server     Go chitchat-server
  + frontend dist        + frontend dist        + frontend dist
        |                      |                      |
        +----------+-----------+----------+-----------+
                   v                      v
              PostgreSQL (single shared instance on VM4)
              all backends -> 172.17.0.93:5432 (VM4 local)
```

**Data flow for `/message` (durable fast-ACK ingest):**
1. LB selects the least-loaded healthy backend and forwards.
2. Backend validates, assigns a UUID (or accepts client ID), appends the entry to a **local write-ahead journal** (page-cache write, microseconds), and returns **200 immediately**.
3. A background writer batches entries (~20 ms / 500 rows) into **one multi-row `INSERT ... ON CONFLICT (id) DO NOTHING`** against the shared PostgreSQL — this both amortizes the per-commit cost ~500x and **guarantees no duplicate rows** for retried/replayed IDs.
4. If a backend dies after ACK, its journal **replays on restart** (watchdog restarts it in ~2 s), so a 2xx always ends up in `/feed`.

**Why this design on 1-core / 512 MB cgroup VMs:** one PostgreSQL commit per message caps writes at a few hundred/s; batching removes that wall. Sharing a single PostgreSQL keeps consistency trivial (no split-brain) and was validated as the correct choice for this scale; distribution would add coordination cost without adding throughput on these quotas.

### Dynamic (performance-based) scheduling — not round-robin
- Each backend's **load score** = `2 x in-flight requests + EWMA(response time)/200`.
- `Next()` uses **power-of-two-choices**: sample two healthy backends (below the per-backend concurrency threshold), pick the lower score; **5% epsilon-greedy** exploration keeps scores honest.
- A backend exceeding its threshold, failing an active probe, or returning connection-refused mid-flight is **evicted from rotation instantly**.
- WebSocket upgrades are excluded from in-flight/latency accounting (a minutes-long WS session must not poison the scheduler).

### Failure handling (validated by chaos test)
| Failure | Mechanism | Client impact |
|---|---|---|
| Backend process killed mid-load | passive circuit breaker + LB retry with body replay + watchdog restart + journal replay | **0 failed requests** (measured) |
| PostgreSQL unreachable | batch writer retries until success; journal is the source of truth | requests still ACKed, persisted on recovery |
| LB process death | LB watchdog restarts with tuned env | ~2 s outage window |

## 3. Repository / Code Details

| Component | Repository | Key commits |
|---|---|---|
| Backend (Go port of the Java chat app + workload API) | `github.com/Rahulrajln1111/chitchat` (fork; team repo `Om-A-osc/chitchat` push-disabled after Sep 12) | `2090db3` journal ingest, `af87a1f` batched inserts, `5580858` log throttling, `0e75ddc` pgx + streaming feed |
| Load balancer | `github.com/Rahulrajln1111/load-balancer` | `a541891` performance scheduler, `b95ccef` transport sizing, `18a289d` WS metric fix, `d46be51` POST failover + circuit breaker, `9788c24` GC tuning |
| Own load generator (report) | `load_generator.py` in the load-balancer repo; staged experiment harness in `report/` | variable users, random message lengths, random intervals |

**Backend layout:** `cmd/server` (HTTP + WS wiring), `internal/handlers` (`/message`, `/feed`, chat routes), `internal/ingest` (journal + background batch writer), `internal/db` (pgx pool, streaming feed), `internal/rooms`, `internal/auth`, `internal/crypto` (AES-GCM at-rest encryption + wrapped Ed25519 keys), `chitchat-frontend/dist` (served by each backend).
**LB layout:** `cmd/lb`, `internal/server` (routing, retries, metrics), `internal/scheduler` (performance scheduling), `internal/backend` (health state, EWMA), `internal/health` (active probes), `internal/proxy` (transport).

**Deployment (per VM):** binaries run from `$HOME` with persistent env files (`backend.env`, `lb.env`) pinning DB DSN, `GOMEMLIMIT`, `GOMAXPROCS`, and frontend dir; `setsid`-detached watchdog scripts restart LB/backends automatically. Kernel TCP backlog hardening on VM1. NAT mapping: public 3289 → VM1:3000 (LB), 3290/3291/3292 → the three backends.

## 4. Own Load Generator

Two tools were developed (assignment requirement):

1. **`load-balancer/load_generator.py`** — closed-loop users; **variable user count**, **random/variable message lengths** (`--min-msg-len/--max-msg-len`), **random/variable intervals** (`--min-interval/--max-interval`), configurable feed ratio; produces latency plots, throughput, per-user activity, and LB/backend metric timelines.
2. **`report/experiment.py` harness** (data + plots in `report/`) — staged ramp **100 → 250 → 500 → 1000 → 1500 → 2000 → 2500 concurrent users** (20 s per stage), each user posting random-length messages (1–25 words, ≤300 chars) at random intervals (0–50 ms) with 20% `/feed` reads; **independently samples CPU% (cgroup `cpu.stat`), memory (cgroup `memory.current`), on all 4 VMs every second over SSH** during the run.

## 5. Experiment Results (own load generator, run Sep 14, 2026)

52,356 messages accepted across all stages; **0 errors at every stage**.

| Users | POST avg | POST p50 | POST p95 | POST p99 | Feed avg | Throughput (req/s) | Errors |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 100 | 114.9 ms | 66.3 | 394.6 | 947.7 | 121.5 | ~358 | 0 |
| 250 | 218.5 | 149.4 | 720.1 | 1356.7 | 224.8 | ~317 | 0 |
| 500 | 382.3 | 239.8 | 1229.7 | 1774.6 | 378.2 | ~337 | 0 |
| 1000 | 689.6 | 394.5 | 2311.9 | 4076.5 | 664.4 | ~443 | 0 |
| 1500 | 655.2 | 477.9 | 1782.8 | 2808.9 | 655.2 | ~583 | 0 |
| 2000 | 1397.5 | 914.7 | 5525.7 | 6831.3 | 1370.9 | ~559 | 0 |
| 2500 | 1418.1 | 955.7 | 4859.3 | 7307.1 | 1403.6 | ~590 | 0 |

*(Feed p95 and full JSON in `report/data.json`; burst-throughput experiments with keep-alive clients reach 1400–1900 req/s — the closed-loop think-time model above yields ~590 req/s at 2500 users.)*

### System utilization (peaks across the whole run, 1 s sampling)

| System | Role | CPU peak | Memory peak (512 MB cgroup limit) |
|---|---|---:|---:|
| VM1 :2289 | Load Balancer | 101% | 445 MB |
| VM2 :2290 | Backend-1 | 110% | 355 MB |
| VM3 :2291 | Backend-2 | 110% | 397 MB |
| VM4 :2292 | Backend-3 + PostgreSQL | 110% | 511 MB |

CPU >100% reflects cgroup CPU accounting (burst above the 1-core quota); all systems stayed inside their memory limits for the entire run (VM4's brief 511 MB touch is the PostgreSQL page cache — bounded by `shared_buffers=64MB` and the kernel, with `oom_kill` counter unchanged).

### Plots (in `report/plots/`)

| File | Content |
|---|---|
| `1_response_time_vs_users.png` | avg/p50/p95/p99 for POST /message and GET /feed vs concurrency |
| `2_throughput_per_stage.png` | requests/second at each stage |
| `3_cpu_utilization_all_systems.png` | **CPU of all 4 systems** over time, stage boundaries marked |
| `4_memory_utilization_all_systems.png` | **Memory of all 4 systems** vs the 512 MB cgroup limit |
| `5_latency_at_peak.png` | latency distribution at 2500 users |
| `6_error_rate.png` | error rate per stage (all zero) |

## 6. Duplicate Prevention & Persistence Guarantees

- **Unique message IDs:** UUIDv4 generated server-side; client-supplied IDs honored for idempotent retries.
- **No duplicates:** single primary key + `INSERT ... ON CONFLICT (id) DO NOTHING`; LB-side retry replays the same ID, so failover cannot double-store. Verified: 10x duplicate posts → exactly 1 row.
- **Persistence:** ACK only after the journal write; journal replay covers crashes (verified by SIGKILL mid-flood: 21,512/21,512 ACKed messages present in PostgreSQL after restart); `/feed` reads from the shared PostgreSQL so all backends return identical data (verified identical counts via each backend directly).
- **Feed correctness:** random-100 byte-for-byte verification after every load stage — 100/100 exact each time.

## 7. Evaluation History (official harness) & Optimizations

Progression of graded runs drove these changes: (1) OOM-killed PostgreSQL under load → memory budgets (GOMEMLIMIT, lean PG config, pool sizing); (2) feed gaps after backend death → journal replay + watchdogs; (3) WebSocket sessions poisoning scheduler metrics → metric exclusion + EWMA clamp; (4) SYN-drops at connection bursts → kernel backlog tuning + LB transport sizing; (5) per-row commits as throughput wall → batched inserts (819 → 1550+ req/s); (6) fsync stalls on shared disk → page-cache ACK design; (7) lib/pq → pgx + streamed `/feed` (+30% throughput).

Final validated capacity: **40,000 requests in 23 s (1741 req/s) via the LB with 0 errors; 60,000 requests across the full breakpoint profile (200→2500 × 5000) with 0 errors; backend SIGKILL mid-flood → 0 client-visible errors.**

## 8. Conclusion

The system meets all assignment requirements: exact `/message` and `/feed` routes through the single public LB URL, performance-based dynamic scheduling with health detection, persistent shared storage with duplicate protection, and a full-featured (unsimplified) secure group chat. Own load generator with variable users/message lengths/intervals produced the required response-time and 4-system utilization plots; the deployment sustains 2500+ concurrent users with zero errors within the 1-core/512 MB quotas of the four allotted systems.
