# 037i — The Jepsen

We test kv-go from the inside. Unit tests verify Raft state transitions, the apply loop, the propose path. Every episode since 036 proves its invariant with deterministic, single-process tests. But we've never tested the system from the outside — as a black box under concurrent load with real faults.

A stale leader can serve stale reads. We know this — 037a listed it as an open thread, and every episode since has deferred it. But we have no evidence. No test has ever partitioned the cluster, forced a stale leader, and observed the violation. Without evidence, the priority is opinion.

Jepsen changes this. It hammers a distributed system with concurrent operations, injects real faults (network partitions, process kills), records every operation, and checks whether the history is consistent with a correctness model. If kv-go claims linearizability, Jepsen can prove or disprove it. The violations Jepsen finds become the next episodes — failure tests drive the development order.

## Boundary

Introduce Jepsen as the external correctness testing framework for kv-go. When this sub-series is complete: a single command runs linearizability tests against a live cluster under fault injection, and every violation found has been fixed.

**Sub-series plan:**

| Episode | Delivers |
|---|---|
| **037j** | HTTP API bridge + Jepsen project scaffold + Docker setup + result visualization. `docker compose up && lein run test -w basic` works end-to-end, results browsable via `lein run serve` |
| **037k** | Partition nemesis. Stale-leader reads produce `{:valid? false}` — the first Jepsen-driven failure |
| **037l** | ReadIndex implementation. 037k's test passes — linearizable reads under partitions |

In scope (across the sub-series):
- HTTP GET/PUT endpoints on the server for Jepsen's client to use
- Cross-platform Docker Compose setup (control node + 5 db nodes) — works on Windows, Mac, Linux
- Jepsen test project with workload separation (`basic`, `partition`, future `stale`, `crash`, …)
- Linearizability checking with Knossos
- Result visualization: interactive timeline HTML, latency/throughput graphs, browsable via `lein run serve`
- Network partition nemesis via iptables
- ReadIndex protocol (leader confirms liveness before serving reads)

Deferred:
- Process kill nemesis — crash recovery under Jepsen (already tested in unit tests via 037g)
- Clock skew nemesis — Docker containers don't have real clocks; needs VMs
- Independent key generators — optimization for checker tractability, add when single-key tests pass

Out of scope:
- Cluster membership changes under Jepsen
- Performance benchmarking through Jepsen (kv-bench already covers this)

## Design

### Why failure-driven

037a planned the reimplementation order as a series of features. That worked for the purge — the order was obvious (remove first, rebuild second). But for correctness features like ReadIndex, the order should come from evidence, not intuition. Jepsen provides that evidence: run the test, see the failure, fix what broke. The failure is the spec.

### Why HTTP

kv-go speaks a custom binary protocol (22-byte request header, frame-multiplexed over TCP). Writing a Clojure client for this protocol is possible but fragile — any wire format change breaks the Jepsen client. HTTP is a two-line bridge: `GET /kv/{key}` reads from the state machine, `PUT /kv/{key}` proposes through Raft. The HTTP layer is disposable test infrastructure, not a production API.

### Why Docker

Jepsen needs a Linux cluster with SSH, iptables, and process control. Docker Compose gives this portably — same `docker compose up` on Windows, Mac, and Linux. The community [jepsen-docker](https://github.com/nurturenature/jepsen-docker) images provide Debian nodes with systemd, SSH, and iptables pre-configured. We extend them with the kv-server binary.

Caveat: Docker containers don't have real clocks, so clock-skew tests are out. Acceptable — the bugs we're hunting (stale reads, lost writes) don't need clock faults.

### Workload separation

Each Jepsen workload is a separate namespace returning `{:generator :checker :nemesis}`. The main namespace (`kvgo.clj`) owns shared infrastructure (db lifecycle, client, configuration) and a workload registry. New failure scenarios are new files, not modifications to existing tests:

```
src/jepsen/kvgo.clj          ← shared infra + CLI dispatcher
src/jepsen/kvgo/basic.clj    ← PUT/GET, no nemesis
src/jepsen/kvgo/partition.clj ← network partitions (037k)
```

### Result visualization

Jepsen produces three kinds of visual output from every test run:

1. **Timeline HTML** (`timeline/html`) — an interactive page showing every operation (invoke → ok/fail/info) across all threads over time. When linearizability fails, this is where you see which concurrent operations collided.
2. **Performance graphs** (`checker/perf`) — latency percentiles and throughput over time rendered via gnuplot. Shows how the system behaves before, during, and after faults.
3. **Result browser** (`lein run serve`) — a web server that indexes all test runs under `store/`. Navigate to any past run, view its timeline, graphs, history, and checker output in a browser.

All three are composed in the checker:

```clj
(checker/compose
  {:linear   (checker/linearizable ...)
   :perf     (checker/perf)
   :timeline (timeline/html)})
```

Results land in `store/<test-name>/<timestamp>/`. The Docker setup exposes port 8080 from the control container so `lein run serve` is browsable from the host machine on Windows or Mac.

### What we expect to find

With no nemesis (basic workload), the test should pass — a single leader handles all reads and writes. With partition nemesis, the test should fail: a partitioned leader continues serving reads from its local state machine while a new leader accepts writes on the majority side. The linearizability checker will flag the stale read. That failure motivates ReadIndex.

## Minimum tests

**Invariant:** Jepsen infrastructure is wired end-to-end — a test can run against a live kv-go cluster from a single command and produce a valid linearizability result.

1. **HTTP GET returns value after PUT** — proves the HTTP bridge reaches the state machine through the same propose-wait-apply path as the binary protocol.
2. **HTTP GET returns 404 for missing key** — proves the not-found path works through HTTP.
3. **HTTP PUT returns 200 after Raft commit** — proves the HTTP handler shares the propose-wait-apply path.
4. **proposePut shared path** — binary `handlePut` and HTTP PUT both call `proposePut`. Same state machine mutation regardless of wire format.
5. **Docker cluster boots and elects** — `docker compose up` starts 5 nodes + 1 control, a leader is elected within the Raft election timeout. Manual verification until 037j automates it.
6. **Basic workload reports `{:valid? true}`** — `lein run test -w basic` completes with no linearizability violations against a healthy cluster.
7. **Partition workload reports `{:valid? false}`** — (037k) network partitions cause stale reads. The checker detects the violation.
8. **Partition workload reports `{:valid? true}` after ReadIndex** — (037l) ReadIndex eliminates stale reads. The same test passes.

## Open threads

1. **Independent keys** — single-key testing with high concurrency makes the linearizability checker exponentially slow. Split across many keys when single-key tests are stable.
2. **Process kill nemesis** — SIGKILL nodes mid-write, verify acknowledged writes survive. Already unit-tested in 037g but not integration-tested under Jepsen.
