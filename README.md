# muster

> *muster* — to assemble scattered members into formation before they act.

**muster** is a container entrypoint for clustered stateful workloads. Replicas start up not knowing who they are: muster discovers its peers through AWS CloudMap (a.k.a. ServiceDiscovery), coordinates with them to settle roles, computes the workload's command line, then starts and supervises the process. Discovery, coordination, argv, and the process lifecycle are all driven by a small **[Starlark](https://github.com/google/starlark-go)** script, so it can express the imperative decisions that clustered stateful systems (e.g. TiKV/PD) need at startup — *am I bootstrapping a new cluster, joining an existing one, or restarting an existing member?* — rather than just string interpolation.

The script is **imperative and async**. It defines one required `main()` function that drives everything and **returns a promise** representing the workload; the harness awaits that promise and, on `SIGTERM`/`SIGINT`, delivers a graceful-stop `signal()` to it. `main()` calls `spawn()`, which supervises the workload — it resolves the argv, runs it, respawns it, and drives script-supplied `readiness`/`liveness` promise factories — passing every callback by reference (`resolve`, `pre_start`, `post_start`, `pre_stop`, `post_stop`, `readiness`, `liveness`). Async primitives — `go()`, `poll()`, `promise()`, `join()`, `select()` — let `main()` coordinate (seed election, ordering, multiple workloads) and react to health imperatively. Builtins also expose CloudMap discovery, the host's interface addresses, filesystem checks, HTTP/TCP/gRPC probes, ECS preconditions, and a conditional-write key/value store backed by DynamoDB.

For example, this script builds a `--servers` argument from the healthy instances of `my-service` and supervises the workload:

```python
# servers.star
def resolver():
    peers = instances("my-service")
    urls = ["http://%s:%d" % (p.ipv4, p.port) for p in peers]
    return COMMAND + ["--servers=" + ",".join(urls)]

def main():
    return spawn(resolve=resolver, respawn=True)
```

```bash
muster \
  -namespace my-namespace \
  -script servers.star \
  -- my-executable
```

> **Note:** the previous `text/template` interpolation mode and the declarative `respawn()`/`healthcheck()` builtins have been **removed** in favor of this imperative model. See [Migrating](#migrating).

## Installation

You can install muster using `go install`:

```bash
go install github.com/moriyoshi/muster@latest
```

## Usage

```bash
muster \
  -namespace <namespace> \
  -script <path> \
  [-kv-table <name> [-kv-key-prefix <prefix>] [-kv-create-table]] \
  [-allow-run] \
  [-control-socket <path>] \
  -- [command...]
```

The trailing `command...` after `--` is optional; it is passed to the script as the `COMMAND` global so a script can transform a base command rather than build argv from scratch.

<dl>
<dt><code>-namespace</code></dt>
<dd>

**Required.** The default namespace for the `instances()` builtin (overridable per call via its `namespace` argument).
</dd>
> Supervision (respawn) and healthchecking (readiness/liveness) are configured **from the script** as `spawn()` arguments — there are no CLI flags for them. Retrying discovery until instances appear is also scripted: `instances()` is a single lookup that returns an empty list when nothing matches; wrap it in `poll` (join it for a synchronous wait). See [Scripting](#scripting).

<dt><code>-control-socket</code></dt>
<dd>

Path to a unix-domain socket on which the harness serves `GET /health` (JSON). Exposes harness-level state (uptime) and a per-workload array (up/pid, respawn count, current backoff, health) — one entry per `spawn()`. Empty (the default) disables it. See [Control socket and health probe](#control-socket-and-health-probe).
</dd>
<dt><code>-health-probe</code></dt>
<dd>

Run the binary as a **health-probe client** instead of the harness: it connects to `-control-socket`, and exits `0` when healthy or non-zero otherwise. Intended for use as a container `HEALTHCHECK CMD`. See [Control socket and health probe](#control-socket-and-health-probe).
</dd>
<dt><code>-script</code></dt>
<dd>

**Required.** Path to the Starlark script that defines `resolve()` and the optional lifecycle hooks. See [Scripting](#scripting).
</dd>
<dt><code>-kv-table</code></dt>
<dd>

DynamoDB table name backing the `kv_*` builtins. Empty (the default) disables the key/value store; calling a `kv_*` builtin then raises an error. See [Key/value store (DynamoDB)](#keyvalue-store-dynamodb).
</dd>
<dt><code>-kv-key-prefix</code></dt>
<dd>

Prefix prepended to every `kv_*` key, so multiple clusters can share one table without colliding. Default is empty.
</dd>
<dt><code>-kv-create-table</code></dt>
<dd>

Create the kv table (on-demand billing, TTL enabled on `expires_at`) if it does not already exist. Default is off; normally the table is provisioned out of band (Terraform/CDK).
</dd>
<dt><code>-allow-run</code></dt>
<dd>

Expose the `run([...])` builtin, which executes an arbitrary command. Off by default to keep the sandbox tight; prefer `http_request` where possible.
</dd>
<dt><code>[command...]</code></dt>
<dd>

Optional trailing command after `--`, exposed to the script as the `COMMAND` global (a list of strings).
</dl>

## Scripting

The workload lifecycle is defined by an imperative [Starlark](https://github.com/google/starlark-go) script (a small Python dialect). The only required global is **`main()`**, which returns a promise; the harness awaits it and, on `SIGTERM`/`SIGINT`, delivers a graceful-stop `signal()` to it. Every other function is ordinary — wire the ones you need into `spawn()` by reference.

### `main()` and `spawn()`

`main()` does any up-front coordination and returns the workload promise from `spawn()`:

```python
def resolver():                 # per-attempt: return argv (list) or {"argv":[...], "env":{...}}
    return COMMAND

def readiness():                # a Promise that resolves when the workload is ready
    return poll(http_ok("http://127.0.0.1:8080/ready"), "60s")

def on_stop():                  # runs once on teardown
    deregister()

def main():
    # coordinate here (seed election, ordering) …
    return spawn(resolve=resolver, pre_stop=on_stop, readiness=readiness, respawn=True)
```

`spawn(...)` owns the respawn loop and the readiness/liveness probes. Its arguments:

<dl>
<dt><code>resolve</code> (required)</dt>
<dd>

A function returning the workload argv each attempt — a list of strings, or `{"argv":[...], "env":{...}}`. Runs before every (re)spawn, so a clustered workload can re-decide its role (restart / join / bootstrap) after a crash.
</dd>
<dt><code>name</code></dt>
<dd>

Optional label for this workload in the control-socket snapshot (auto-assigned `workload-<n>` by position if omitted). Give each workload a distinct name when `main()` runs more than one.
</dd>
<dt><code>pre_start</code> / <code>post_start</code> / <code>pre_stop</code> / <code>post_stop</code></dt>
<dd>

Optional lifecycle callbacks around each attempt:
- **`pre_start`** — before the process starts. The only hard gate: an error aborts the attempt (respawns if enabled, else fails). Use it to wait on dependencies.
- **`post_start`** — right after the process is up (PID known). Best-effort (a failure is logged, not fatal — gate on being ready with `readiness`). Use it to record/notify a just-launched process.
- **`pre_stop`** — when the workload is torn down (shutdown, liveness-restart, or exit), **while the process is still alive**. Runs under a detached, time-bounded context (`pre_stop_timeout`, else `shutdown_grace`) so it can deregister while peers are still reachable.
- **`post_stop`** — after the process has **fully exited**. Best-effort, detached/time-bounded like `pre_stop`. Use it for cleanup that needs the process gone (pid files, final deregistration).
</dd>
<dt><code>readiness</code> / <code>liveness</code></dt>
<dd>

Optional functions returning a **promise** (see below), called by `spawn()` per attempt. A truthy `readiness` marks the workload ready (control socket); if it never resolves truthy the attempt is restarted. When `liveness` settles truthy (liveness lost) the workload is marked unhealthy and, if `restart_on_liveness=True` (default), restarted. Intervene (register/deregister) as a side effect inside these callbacks.
</dd>
<dt>respawn policy</dt>
<dd>

`respawn=False`, `keep_alive=False`, `max_retries=5` (0 = unlimited), `initial_interval="1s"`, `max_interval="60s"`, `multiplier=2.0`, `reset_after="30s"`, `shutdown_grace="10s"`, `pre_stop_timeout=0`, `resolve_timeout=0`, `resolve_failure="retry"`, `restart_on_liveness=True`. With `respawn=True`, a non-zero exit restarts with jittered exponential backoff up to `max_retries`; `keep_alive=True` also restarts a clean exit; the counter resets after the workload stays up `reset_after`.
</dd>
</dl>

The returned promise is **signallable**: `w.signal()` requests a graceful stop (stop respawning → `pre_stop` → `SIGTERM`→`shutdown_grace`→`SIGKILL`) and resolves with a `{code, respawn_count, reason}` struct. Retries exhausted → the promise **rejects** and the process exits non-zero.

### Promises

Async primitives let `main()` coordinate and react. A **promise** is a settle-once future.

- `go(fn, *args) -> promise`: run `fn(*args)` on a background task; resolves with its return, rejects on error. The general async primitive. (`fn` and its args are frozen — read captured state, don't mutate after launch.)
- `poll(check, timeout, interval=1s) -> promise`: resolves `True` when `check()` becomes truthy, `False` on timeout, rejects on a `check` error. The idiomatic way to build `readiness`/`liveness`. For a **synchronous** wait, join it: `join(poll(check, "60s"))` blocks the current task and returns the bool.
- `promise() -> promise`: a bare deferred you settle yourself via `p.signal(value)` / `p.reject(err)`.
- `join(*p) -> value|list`: await all; returns the value (or a list for many); **raises** on any rejection or cancellation.
- `select(*p) -> promise`: race; returns the first-settled promise (compare by identity: `if select(a, b) == a`).
- `any_true(*p) -> bool`: race for the first promise to resolve **truthy** → `True` (cancelling the rest); `False` once all resolve falsy; **raises** on rejection. Short-circuiting concurrent predicate — e.g. "is any peer up?": `any_true(*[go(http_ok(PD(p))) for p in peers])`.

Every promise has `p.done()`. **Cancellable** promises (`go`/`poll`) add `p.cancel()` (abort and discard → resolves `None`). **Signallable** promises (`spawn()`, bare `promise()`) add `p.signal(value)` / `p.reject(err)`. `go`/`poll` accept `signallable=True`; `promise()` accepts `cancellable=`/`signallable=`.

### Globals

- `COMMAND`: the trailing `--` command as a list of strings.
- `TASK`: ECS task metadata struct with `.cluster`, `.service_name`, `.task_arn`, `.availability_zone`, `.created_at`, `.family`, `.revision`, `.vpc_id` (or `None` outside ECS).

### Builtins

**Discovery / networking**
- `instances(service, health_status=None, namespace=None)`: one CloudMap `DiscoverInstances` lookup → list of structs with `.ipv4`, `.ipv6`, `.port`. No internal retry; an empty result is an empty list — wrap in `poll` (join it) for a quorum. `namespace` overrides `-namespace`; `health_status` overrides the default `HEALTHY` (`HEALTHY`/`UNHEALTHY`/`ALL`/`HEALTHY_OR_ELSE_ALL`).
- `ifaddr(cidr)`: the host's own address within `cidr`.

**ECS (preconditions)** — cluster/service default to the running task's own:
- `all_ecs_tasks_running(cluster=None, service=None)` → bool: `RunningCount == DesiredCount`. Gate on it in `pre_start`: `if not join(poll(all_ecs_tasks_running, "60s")): fail(...)`.

**Filesystem** (read-only): `path_exists(path)`, `read_file(path)` (≤1 MiB).

**Health / HTTP**
- `http_ok(url, timeout=2s)`, `tcp_ok(hostport, timeout=2s)`, `grpc_ok(hostport, service="", timeout=2s)` → a **check factory**: each captures its target and returns a zero-arg callable that runs the probe and returns a bool. Pass it straight to `poll`/`go` (`poll(http_ok(url), "60s")`) or call it for the bool inline (`if http_ok(url)(): ...`).
- `un(fn, *args)` → a check factory computing `not fn(*args)`. Negates a probe without a lambda: `poll(un(http_ok(url, "2s")), "24h")` resolves when the target stops being live.
- `http_request(method, url, body=None, headers={}, timeout=30s)` → `{status, body}` (e.g. a PD member delete via HTTP `DELETE`).

**Key/value store** — see [below](#keyvalue-store-dynamodb).

**Misc**: `env(name, default=None)` → str (reads the harness's own environment), `log(msg, **kwargs)`, `sleep(seconds)` (cancellable), `rand()` → float `[0.0, 1.0)`, `randint(a, b)` (inclusive, like `random.randint`), and `run([...])` → `{code, stdout, stderr}` (only with `-allow-run`).

Durations accept a number of seconds or a string like `"5s"` / `"500ms"`. Blocking builtins are cancelled when the harness shuts down.

### Key/value store (DynamoDB)

When `-kv-table` is set, the `kv_*` builtins provide a conditional-write key/value store backed by DynamoDB. Atomic put-if-absent and compare-and-swap with TTL leases make it a distributed lock, which is what lets exactly one node win seed election during a cold start (avoiding split-brain).

- `kv_put_if_absent(key, val, ttl=None)` → bool. Writes only if the key is absent or its lease has expired. **The seed-election primitive.**
- `kv_compare_and_swap(key, old, new, ttl=None)` → bool.
- `kv_get(key)` → str or `None`. To wait for a key, compose with `poll`: `join(poll(lambda: kv_get(k) != None, "120s"))`, then `kv_get(k)`.
- `kv_delete(key, if_value=None)` → bool.
- `kv_renew(key, ttl)` → bool. Extends a lease you own; `False` means the lease was lost.

A `ttl` of `None`/`0` is a permanent key; a positive `ttl` is a lease that expires (and frees the lock) if the holder dies.

**Table schema.** Partition key `pk` (String), value `val` (String), and `expires_at` (Number, Unix epoch seconds) configured as the table's **TTL attribute**. Provision it out of band, or pass `-kv-create-table` to create it (on-demand billing, TTL enabled) on first run. Use `-kv-key-prefix` to share one table across clusters.

**IAM.** The task role needs `dynamodb:GetItem`, `PutItem`, `UpdateItem`, and `DeleteItem` on the table. With `-kv-create-table`, also `CreateTable`, `DescribeTable`, and `UpdateTimeToLive`.

### Example: PD restart / join / bootstrap

```python
# pd.star  —  run with: -kv-table <table> -allow-run -- /pd-server
PD = lambda ip: "http://%s:2379" % ip

def resolver():
    me = ifaddr("172.31.255.0/24")
    peers = [i.ipv4 for i in instances("tikv-pd") if i.ipv4 != me]

    if path_exists("/pd/member"):                        # (A) restart existing member
        mode = []
    elif peers and any_true(*[go(http_ok(PD(p) + "/pd/api/v1/members")) for p in peers]):
        mode = ["--join", ",".join([PD(p) for p in peers])]   # (B) cluster exists → join (probes race)
    elif kv_put_if_absent("tikv-pd/seed", me, "90s"):    # (C) win the lease → bootstrap
        mode = []
    else:                                                # someone else is seeding → wait, then join
        join(poll(lambda: kv_get("tikv-pd/seed") != None, "120s"))
        seed = kv_get("tikv-pd/seed")
        join(poll(http_ok(PD(seed) + "/pd/api/v1/health"), "120s"))
        mode = ["--join", PD(seed)]

    return COMMAND + ["--advertise-client-urls", PD(me),
                      "--advertise-peer-urls", "http://%s:2380" % me] + mode

def liveness():                                          # settles when PD stops being live
    me = ifaddr("172.31.255.0/24")
    return poll(un(http_ok(PD(me) + "/pd/api/v1/health", "2s")), "24h", interval="10s")

def on_stop():
    me = ifaddr("172.31.255.0/24")
    run(["/pd-ctl", "-u", PD(me), "member", "delete", "name", TASK.task_arn])
    kv_delete("tikv-pd/seed", if_value = me)

def main():
    return spawn(resolve=resolver, liveness=liveness, pre_stop=on_stop,
                 respawn=True, max_retries=0)   # keep restarting; liveness loss → restart
```

### Migrating

The `text/template` interpolation and the declarative `resolve()`/`respawn()`/`healthcheck()` model have been removed. In the current model:

- Define **`main()`** (the only required global); it returns a promise, usually from `spawn()`.
- Pass the argv provider and lifecycle callbacks to `spawn()` by reference (any names): `resolve`, `pre_start`, `post_start`, `pre_stop`, `post_stop`, `readiness`, `liveness`.
- The old `respawn(...)` knobs are now `spawn(...)` arguments.
- The old `healthcheck(probe=...)` becomes a `readiness`/`liveness` function returning a promise (typically `poll(...)`); `spawn()` drives it and restarts on liveness loss (`restart_on_liveness`).
- List comprehensions replace the old `extract`/`mapprintf`/`join`/`exclude` template funcs (`["http://%s:%d" % (p.ipv4, p.port) for p in instances("s")]`); env goes in the resolve dict `{"argv":[...], "env":{...}}`.

## Control socket and health probe

With `-control-socket <path>`, the harness serves `GET /health` (JSON) over a unix-domain socket. Because `main()` can `spawn()` more than one workload, the payload carries harness-level state plus a **`workloads` array** — one entry per `spawn()`, each with its own process, respawn, and health fields:

```json
{
  "harness": { "running": true, "started_at": "…", "uptime_seconds": 123.4 },
  "workloads": [
    { "name": "pd", "up": true, "pid": 4321, "started_at": "…", "uptime_seconds": 100.2,
      "respawn_count": 2, "current_backoff_seconds": 0, "max_retries": 5,
      "last_exit_code": 0, "last_exit_error": "",
      "health": { "state": "healthy", "consecutive_ok": 3, "consecutive_fail": 0,
                  "last_probe_at": "…", "last_probe_error": "" } }
  ]
}
```

Each workload's `name` is its `spawn(name=…)` argument (auto-assigned `workload-<n>` by position if omitted). A workload's `health.state` is `healthy`/`unhealthy`/`unknown` (`unknown` = no `readiness`/`liveness` configured).

The same binary can act as a probe client with `-health-probe`, which queries the socket and maps state to an exit code (`0` = healthy, non-zero = unhealthy or unreachable). The container is reported healthy only when **every** workload is healthy — a workload with no health probe falls back to being up, and any unhealthy or down workload fails the probe. This makes it usable directly as a container healthcheck command:

```dockerfile
ENTRYPOINT ["muster", "-namespace", "my-namespace", \
            "-script", "/workload.star", \
            "-control-socket", "/run/muster.sock", \
            "--", "my-workload"]
HEALTHCHECK CMD ["muster", "-health-probe", "-control-socket", "/run/muster.sock"]
```

The probe client uses a short timeout, so a missing socket or dead harness reports unhealthy promptly rather than hanging.

## Configuration

muster can be configured using environment variables supported by AWS SDK. The following is a non-exhaustive list of such:

- `AWS_REGION`: The AWS region to use for service discovery. If not set, the default region from the AWS CLI configuration will be used.

- `AWS_PROFILE`: The AWS profile to use for service discovery. If not set, the default profile from the AWS CLI configuration will be used.

- `AWS_ACCESS_KEY_ID`: The AWS access key ID to use for service discovery. If not set, the default credentials from the AWS CLI configuration will be used.

- `AWS_SECRET_ACCESS_KEY`: The AWS secret access key to use for service discovery. If not set, the default credentials from the AWS CLI configuration will be used.

- `AWS_SESSION_TOKEN`: The AWS session token to use for service discovery. If not set, the default credentials from the AWS CLI configuration will be used.

- `AWS_ENDPOINT_URL`: The AWS endpoint URL to use. If not set, the default endpoint URL from the AWS CLI configuration will be used. Specifying this will effectively disable the endpoint prefixing behavior. (Thus the actual endpoint will end up being the same as the endpoint URL, in contrast to `data-servicediscovery.<region>.amazonaws.com` where the endpoint is `servicediscovery.<region>.amazonaws.com`.) The override applies to the ServiceDiscovery (CloudMap), ECS, and DynamoDB clients alike, so all three can be pointed at a single mock endpoint for testing.

## Testing

Unit tests (including seed-election and lifecycle-hook coverage using an in-memory kv store) run with:

```bash
go test ./...
```

An opt-in end-to-end test exercises the DynamoDB-backed kv store and seed election against a real, stateful AWS emulator — [Winterbäume](https://github.com/moriyoshi/winterbaume) in its standalone `winterbaume-server` mode, which speaks the DynamoDB, ECS, and CloudMap APIs over one HTTP endpoint. It is build-tag gated and skipped unless an endpoint is provided:

```bash
winterbaume-server &   # or any DynamoDB-compatible endpoint
AWS_ENDPOINT_URL=http://127.0.0.1:8080 AWS_REGION=us-east-1 \
  go test -tags=e2e -run E2E ./...
```
