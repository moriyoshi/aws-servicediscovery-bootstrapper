# muster

> *muster* — to assemble scattered members into formation before they act.

**muster** is a container entrypoint for clustered stateful workloads. Replicas start up not knowing who they are: muster discovers its peers through its cloud's service registry, coordinates with them to settle roles, computes the workload's command line, then starts and supervises the process. Discovery, coordination, argv, and the process lifecycle are all driven by a small **[Starlark](https://github.com/google/starlark-go)** script, so it can express the imperative decisions that clustered stateful systems (e.g. TiKV/PD) need at startup — *am I bootstrapping a new cluster, joining an existing one, or restarting an existing member?* — rather than just string interpolation.

The script is **imperative and async**. It defines one required `main()` function that drives everything and **returns a promise** representing the workload; the harness awaits that promise and, on `SIGTERM`/`SIGINT`, delivers a graceful-stop `signal()` to it. `main()` calls `spawn()`, which supervises the workload — it resolves the argv, runs it, respawns it, and drives script-supplied `readiness`/`liveness` promise factories — passing every callback by reference (`resolve`, `pre_start`, `post_start`, `pre_stop`, `post_stop`, `readiness`, `liveness`). Async primitives — `go()`, `poll()`, `promise()`, `join()`, `select()` — let `main()` coordinate (seed election, ordering, multiple workloads) and react to health imperatively. Builtins also expose service-registry discovery, the host's interface addresses, filesystem checks, HTTP/TCP/gRPC probes, replica-set preconditions, and a conditional-write key/value store. Those four are the cloud-facing ones, and each is provided by a [provider](#providers) chosen at build time: AWS by default, Google Cloud with `-tags gcp`.

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
go install github.com/moriyoshi/muster@latest          # AWS (the default)
go install -tags gcp github.com/moriyoshi/muster@latest # Google Cloud
```

## Providers

The provider is chosen at **build time**, not at runtime: one binary talks to one
cloud. That is what keeps the default image carrying the AWS SDK and nothing
else — the Google Cloud build is three times the size, and neither should pay
for the other. CI asserts both directions.

Which provider a binary has is in its startup log, in the `PROVIDER` global, and
in `muster -provider-help`. Selecting one that was not compiled in says how to
rebuild rather than reporting an unknown name.

| | AWS (default) | Google Cloud (`-tags gcp`) |
| --- | --- | --- |
| Build | `go build ./...` | `go build -tags gcp ./...` |
| Target runtime | ECS (Fargate or EC2) | Cloud Run (services, jobs, worker pools) |
| `instances()` | CloudMap `DiscoverInstances` | Service Directory `ResolveService` |
| `register()` / `deregister()` | *unsupported* — ECS Service Connect registers tasks for you | Service Directory endpoints; **worker pools only** |
| `all_replicas_running()` | ECS `RunningCount == DesiredCount` | *unsupported* — see below |
| `kv_*` store | a DynamoDB table | a Cloud Storage bucket, or a Firestore collection |
| `SELF` | ECS task metadata | Cloud Run's environment + the metadata server |
| `SELF.name` | *empty* — a task has no name that survives replacement | *empty* — nor does a Cloud Run instance |
| `SELF.ipv4` | the task's own address | the instance's VPC address on a **worker pool**; empty on a service or job |
| Credentials | the AWS SDK default chain | Application Default Credentials |

**Whether peers can address each other depends on the Cloud Run runtime**, and
it is the single thing that decides what a script can do:

- **Worker pools** support [Direct VPC *ingress*](https://docs.cloud.google.com/run/docs/configuring/vpc-direct-vpc):
  each instance gets a private address on your VPC and other resources — other
  instances included — can connect to it there. `SELF.ipv4` is that address,
  `register()` publishes it, and a peer cluster is possible.
- **Services and jobs** support Direct VPC egress only. The address such an
  instance sends *from* is not one anything can send *to*: inbound requests go
  through the Cloud Run frontend and are load-balanced. `SELF.ipv4` is empty
  there rather than advertising an address nothing can connect to, and
  `register()` refuses for the same reason.

So on a service or a job, `instances()` is for resolving your **dependencies** —
a database, a broker set, a cluster running elsewhere — not for assembling
peers. On a worker pool it is for either.

One caveat remains even on a worker pool, and it is the same one Fargate has:
nothing survives replacement. `SELF.name` is empty on all three runtimes, the
address is not preserved, and the ephemeral disk is *"permanently deleted when
the instance shuts down"* — so a cluster member name has to be derived from the
address and `pre_stop` has to evict the member, exactly as on ECS. Cloud Run's
shutdown budget for that is a fixed ten seconds, where ECS allows thirty.

That is workable — [`e2e/tikv/gcp`](e2e/tikv/gcp) runs a real TiKV cluster this
way — but a workload wanting an identity or a disk that *outlives* an instance
wants neither of these platforms.

`all_replicas_running()` raises on Google Cloud: Cloud Run deliberately does not
expose per-instance counts, and the only source is a Cloud Monitoring metric
delayed by minutes. The portable substitute is to count what you can see —
`len(instances(svc, health_status="ALL")) >= n`.

Selection is `-provider`, then `$MUSTER_PROVIDER`, then autodetection.
**Autodetection covers AWS only**: it keys off the ECS task metadata variables,
and a managed instance group is an ordinary Compute Engine VM with nothing in
its environment to key off. Detection deliberately never dials the metadata
server, because that would cost a timeout on every container start on every
other platform. A GCP deployment therefore has to say so — set
`MUSTER_PROVIDER=gcp` in the instance template alongside the rest of its
metadata.

A third provider, `mem`, is always compiled in and has to be asked for by name.
It backs `kv_*` with an in-process store and supports nothing else, so
`muster -provider mem -kv-store x -script s.star -- cmd` runs a script with no
cloud account at all — enough for seed election to behave while you iterate.

### AWS

Region, credentials and endpoint come from the AWS SDK's own default chain:

- `AWS_REGION`: The AWS region to use for service discovery. If not set, the default region from the AWS CLI configuration will be used.

- `AWS_PROFILE`: The AWS profile to use for service discovery. If not set, the default profile from the AWS CLI configuration will be used.

- `AWS_ACCESS_KEY_ID`: The AWS access key ID to use for service discovery. If not set, the default credentials from the AWS CLI configuration will be used.

- `AWS_SECRET_ACCESS_KEY`: The AWS secret access key to use for service discovery. If not set, the default credentials from the AWS CLI configuration will be used.

- `AWS_SESSION_TOKEN`: The AWS session token to use for service discovery. If not set, the default credentials from the AWS CLI configuration will be used.

- `AWS_ENDPOINT_URL`: The AWS endpoint URL to use. If not set, the default endpoint URL from the AWS CLI configuration will be used. Specifying this will effectively disable the endpoint prefixing behavior. (Thus the actual endpoint will end up being the same as the endpoint URL, in contrast to `data-servicediscovery.<region>.amazonaws.com` where the endpoint is `servicediscovery.<region>.amazonaws.com`.) The override applies to the ServiceDiscovery (CloudMap), ECS, and DynamoDB clients alike, so all three can be pointed at a single mock endpoint for testing.

**IAM.** The task role needs `servicediscovery:DiscoverInstances` on the namespace and `ecs:DescribeServices` on the service, plus the kv grants [below](#backing-store-aws-dynamodb).

### Google Cloud

Credentials are Application Default Credentials, which on Cloud Run means the
revision's service account with nothing to configure. Everything else is a
`-provider-opt`:

- `project=<id>` — defaults to `$GOOGLE_CLOUD_PROJECT` (then `GCLOUD_PROJECT`, `GCP_PROJECT`), then the metadata server.
- `location=<region>` — the Service Directory location. Defaults to this instance's own region.
- `kv.backend=gcs|firestore` — which store backs `kv_*`. Default `gcs`. See [below](#backing-store-google-cloud).
- `kv.database=<id>` — the Firestore database. Default `(default)`; ignored by the `gcs` backend.
- `endpoint.storage=`, `endpoint.servicedirectory=`, `endpoint.compute=` — endpoint overrides for a fake server. Setting one also disables authentication for that client, since a fake has no credentials to detect and looking for them would hang.

**IAM**, granted on the resource rather than the project:

| Capability | Role |
| --- | --- |
| `instances()` | `roles/servicedirectory.viewer` on the namespace |
| `register()` / `deregister()` | `roles/servicedirectory.editor` on the namespace |
| `kv_*` | `roles/storage.objectUser` on the bucket |
| `-kv-create` | `storage.buckets.create`; see [below](#backing-store-google-cloud) |

**Nothing registers a Cloud Run instance for you.** Service Directory
auto-registration covers GKE Services, and Cloud Run has no equivalent of ECS
Service Connect, so a clustered workload on a worker pool has to announce
itself: `register(service, port=…)` from `post_start`, `deregister()` from
`pre_stop` while peers are still reachable. Registration is keyed on the
instance id, which does *not* survive replacement — so unlike a platform with
stable names, a restarted instance does not reclaim its own entry, and one
killed without teardown leaves a stale endpoint behind. Probe discovered peers
before trusting them, which is the portable habit anyway.

Autodetection covers Cloud Run: it sets `K_SERVICE` on a service,
`CLOUD_RUN_JOB` on a job and `CLOUD_RUN_WORKER_POOL` on a worker pool, so
`-provider` and `MUSTER_PROVIDER` are optional there.

## Usage

```bash
muster \
  -namespace <namespace> \
  -script <path> \
  [-provider <name>] [-provider-opt k=v ...] \
  [-kv-store <name> [-kv-key-prefix <prefix>] [-kv-create]] \
  [-allow-run] \
  [-control-socket <path>] \
  -- [command...]
```

The trailing `command...` after `--` is optional; it is passed to the script as the `COMMAND` global so a script can transform a base command rather than build argv from scratch.

**Every flag can also be given as an environment variable**, `MUSTER_<FLAG>` with the dashes turned into underscores — `-namespace` is `MUSTER_NAMESPACE`, `-kv-store` is `MUSTER_KV_STORE`. An explicitly passed flag always wins. muster is a container entrypoint, and a container's entrypoint is baked into its image while its environment is per deployment; an image whose entrypoint ends in `-- <workload>` cannot take extra flags by appending arguments at all, so on a platform that only lets you set environment and arguments there would otherwise be no way to pass one. The two mode switches (`-health-probe`, `-provider-help`) are deliberately excluded, so a stray variable cannot turn an ordinary run into a probe.

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
<dt><code>-provider</code></dt>
<dd>

Which cloud provider to talk to. Empty (the default) reads `$MUSTER_PROVIDER`, then autodetects from the environment. Only providers compiled into the binary can be selected; naming one that was not built in says how to rebuild. `-provider-help` lists what this binary has.
</dd>
<dt><code>-provider-opt</code></dt>
<dd>

Provider-specific setting as `k=v`, repeatable. Keys the selected provider does not declare are rejected at startup, so a typo fails rather than being silently ignored. `-provider-help` lists the keys each compiled-in provider accepts.
</dd>
<dt><code>-kv-store</code></dt>
<dd>

Name of the provider-side store backing the `kv_*` builtins — a DynamoDB table on AWS. Empty (the default) disables the key/value store; calling a `kv_*` builtin then raises an error. See [Key/value store](#keyvalue-store).
</dd>
<dt><code>-kv-key-prefix</code></dt>
<dd>

Prefix prepended to every `kv_*` key, so multiple clusters can share one store without colliding. Default is empty.
</dd>
<dt><code>-kv-create</code></dt>
<dd>

Create the kv store with provider defaults if it does not already exist (on AWS: a DynamoDB table with on-demand billing and TTL enabled on `expires_at`). Default is off; normally the store is provisioned out of band (Terraform/CDK), and a provider that cannot create its own store says so rather than starting without one.
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
    kv_delete("my-cluster/seed", if_value=SELF.ipv4)

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

> **Under an orchestrator, prefer a bounded `max_retries`.** `max_retries=0` is right for a bare process supervisor, but on ECS or Kubernetes the scheduler is itself the outer retry loop, and retrying forever keeps that loop from ever running. A `resolve()` that cannot succeed — a denied API call, peers that never appear — then leaves the container up as PID 1 having never started its workload, which reads as a hang rather than a failure: the task stays `RUNNING`, and nothing acts on it until a health check's grace period expires. Exhausting the retries instead exits non-zero, so the scheduler replaces the task and a persistent fault surfaces promptly. `reset_after` clears the counter once an attempt actually stays up, so a bound only fires on consecutive failures.
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

A task that **rejects with nothing to join it** is logged: `join()`, `select()`
and `any_true()` all count as observing the outcome, and a failure none of them
ever collects would otherwise be discarded in silence. This is what makes a
background `go()` loop safe to start — a loop that dies on its first iteration
says so, instead of looking exactly like a loop with nothing to report. Since
Starlark cannot catch a raise, containing a failure means running the fallible
part on its own task and awaiting it with `select()`, which does not re-raise.

Every promise has `p.done()`. **Cancellable** promises (`go`/`poll`) add `p.cancel()` (abort and discard → resolves `None`). **Signallable** promises (`spawn()`, bare `promise()`) add `p.signal(value)` / `p.reject(err)`. `go`/`poll` accept `signallable=True`; `promise()` accepts `cancellable=`/`signallable=`.

### Globals

- `COMMAND`: the trailing `--` command as a list of strings.
- `PROVIDER`: the compiled-in provider's name (`"aws"` / `"gcp"` / `"mem"`). Always set, including when `SELF` is `None` — which is exactly when a script needs it.
- `SELF`: this instance's identity — `.id` (unique, stable for its lifetime; also the kv lease owner), `.name` (stable across replacement, **empty on ECS**, which has no such thing — derive a member name from `.ipv4` there instead), `.group`, `.service`, `.zone`, `.region`, `.network`, `.ipv4`, `.ipv6`, `.created_at`, plus a sub-struct named for the provider — `SELF.aws` carrying `.task_arn`, `.cluster`, `.family`, `.revision`, `.vpc_id`; `SELF.gcp` carrying `.project`, `.instance_id`, `.mig`, `.mig_location`. Reading the wrong one raises, so anything under that name is a visible declaration that the script is not portable. `None` when the platform gave muster nothing, so `if SELF:` is a real guard. Read once at startup. Individual fields can be empty even on a supported platform — notably `.network` on Fargate, which AWS only populates for tasks on EC2 container instances, and `.created_at` on GCE, which the metadata server does not carry.

### Builtins

**Discovery / networking**
- `instances(service, health_status=None, namespace=None)`: one service-registry lookup → list of structs with `.ipv4`, `.ipv6`, `.port`. No internal retry; an empty result is an empty list — wrap in `poll` (join it) for a quorum. `namespace` overrides `-namespace`; `health_status` overrides the default `HEALTHY` (`HEALTHY`/`UNHEALTHY`/`ALL`/`HEALTHY_OR_ALL`); a provider that cannot honour a filter raises rather than widening it.
- `ifaddr(cidr)`: the host's own address within `cidr`. `SELF.ipv4` usually says the same thing without needing the CIDR.
- `register(service, port=None, address=None, namespace=None)` / `deregister()`: publish this instance into the registry, for platforms that do not do it themselves. Both **raise** where the platform already registers for you (AWS), or where the instance has no address anything can reach (a Cloud Run service or job), so a script is told rather than silently doing nothing. Address defaults to `SELF.ipv4`. See [Providers](#providers).

**Replica-set preconditions** — group/service default to `SELF.group`/`SELF.service`:
- `all_replicas_running(group=None, service=None)` → bool: every desired replica is running (on AWS, ECS `RunningCount == DesiredCount`). Gate on it in `pre_start`: `if not join(poll(all_replicas_running, "60s")): fail(...)`. It is the scheduler's view, not "the workload inside is up" — treat it as advisory and probe peers yourself.

**Filesystem** (read-only): `path_exists(path)`, `read_file(path)` (≤1 MiB).

**Health / HTTP**
- `http_ok(url, timeout=2s)`, `tcp_ok(hostport, timeout=2s)`, `grpc_ok(hostport, service="", timeout=2s)` → a **check factory**: each captures its target and returns a zero-arg callable that runs the probe and returns a bool. Pass it straight to `poll`/`go` (`poll(http_ok(url), "60s")`) or call it for the bool inline (`if http_ok(url)(): ...`).
- `un(fn, *args)` → a check factory computing `not fn(*args)`. Negates a probe without a lambda: `poll(un(http_ok(url, "2s")), "24h")` resolves when the target stops being live.
- `http_request(method, url, body=None, headers={}, timeout=30s)` → `{status, body}` (e.g. a PD member delete via HTTP `DELETE`).

**Key/value store** — see [below](#keyvalue-store).

**Misc**: `env(name, default=None)` → str (reads the harness's own environment), `log(msg, **kwargs)`, `sleep(seconds)` (cancellable), `rand()` → float `[0.0, 1.0)`, `randint(a, b)` (inclusive, like `random.randint`), and `run([...])` → `{code, stdout, stderr}` (only with `-allow-run`).

Durations accept a number of seconds or a string like `"5s"` / `"500ms"`. Blocking builtins are cancelled when the harness shuts down.

### Key/value store

When `-kv-store` is set, the `kv_*` builtins provide a conditional-write key/value store backed by the provider. Atomic put-if-absent and compare-and-swap with TTL leases make it a distributed lock, which is what lets exactly one node win seed election during a cold start (avoiding split-brain).

- `kv_put_if_absent(key, val, ttl=None)` → bool. Writes only if the key is absent or its lease has expired. **The seed-election primitive.**
- `kv_compare_and_swap(key, old, new, ttl=None)` → bool.
- `kv_get(key)` → str or `None`. To wait for a key, compose with `poll`: `join(poll(lambda: kv_get(k) != None, "120s"))`, then `kv_get(k)`.
- `kv_delete(key, if_value=None)` → bool.
- `kv_renew(key, ttl)` → bool. Extends a lease you own; `False` means the lease was lost.

A `ttl` of `None`/`0` is a permanent key; a positive `ttl` is a lease that expires (and frees the lock) if the holder dies.

Expiry is filtered when a key is read, on every backend. Both DynamoDB TTL and
Cloud Storage lifecycle rules delete lazily — hours or days late — so neither
can be trusted to have reaped a lapsed lease. Use `-kv-key-prefix` to share one
store across clusters.

Every backend runs the same conformance suite, so they agree on the semantics
that matter, down to the corners: a permanent key is never claimable and never
renewable, and a conditional delete matches on value alone so a script can
release its own lease even if it lapsed while the process was shutting down.

#### Backing store: AWS (DynamoDB)

**Table schema.** Partition key `pk` (String), value `val` (String), and `expires_at` (Number, Unix epoch seconds) configured as the table's **TTL attribute**. Provision it out of band, or pass `-kv-create` to create it (on-demand billing, TTL enabled) on first run.

**IAM.** The task role needs `dynamodb:GetItem`, `PutItem`, `UpdateItem`, and `DeleteItem` on the table. With `-kv-create`, also `CreateTable`, `DescribeTable`, and `UpdateTimeToLive`.

#### Backing store: Google Cloud

Two backends, chosen with `-provider-opt kv.backend=`. They satisfy the same
conformance suite, so the choice is about what you are willing to provision, not
about semantics.

**Cloud Storage (`gcs`, the default).** One object per key under `leases/`, the
value in the body and the lease in custom metadata (`owner`, `ttl_ms`).
Atomicity comes from generation preconditions, which makes compare-and-swap
immune to a value being changed and changed back between a script's read and its
write — something the DynamoDB backend's value comparison cannot see.

`-kv-store` is the bucket. Provision it out of band, or pass `-kv-create` (which
additionally needs `storage.buckets.create`, and a `project`/`location` it can
resolve). A created bucket gets uniform access, **soft delete switched off** — it
defaults to seven billed days, and a lease object is rewritten on every renew —
and a one-day lifecycle rule scoped to `leases/` as a janitor.

**IAM.** `roles/storage.objectUser` on the bucket.

**Firestore (`firestore`).** One document per key, and each operation is one
transaction — so put-if-absent and compare-and-swap read as the semantics are
stated rather than as an encoding of them, and cost one round trip instead of
two. Keys contain `/`, which a document id may not, so they are percent-escaped.

`-kv-store` is the collection and `kv.database` the database. There is no
`-kv-create` for it: a database is a long-running operation and a project-level
decision, not something a container entrypoint should make on your behalf.

**IAM.** `roles/datastore.user`. Firestore has no per-database IAM, so this is
necessarily project-scoped — which is a point in Cloud Storage's favour if least
privilege matters to you.

Expiry is filtered on read by every backend. Firestore's native TTL deletes
lazily — typically within 24 hours — as does DynamoDB's, so none of them can be
trusted to have reaped a lapsed lease.

### Example: PD restart / join / bootstrap

```python
# pd.star  —  run with: -kv-store <table> -allow-run -- /pd-server
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
    run(["/pd-ctl", "-u", PD(me), "member", "delete", "name", SELF.id])
    kv_delete("tikv-pd/seed", if_value = me)

def main():
    return spawn(resolve=resolver, liveness=liveness, pre_stop=on_stop,
                 respawn=True, max_retries=5)   # bounded: give up → ECS replaces the task
```

A worked version of this — three PD replicas and three TiKV stores, with the
seed election exercised against a real cold start — is in
[`e2e/tikv/aws`](e2e/tikv/aws).

### Migrating

#### From the AWS-only releases

muster now runs on more than one cloud, and the script surface no longer names AWS services. The old names are **gone**, not deprecated: Starlark resolves free names at compile time, so a script using them fails to load — naming the missing symbol — before the workload starts.

| Old | New | |
| --- | --- | --- |
| `TASK` | `SELF` | |
| `TASK.task_arn` | `SELF.id` | the unique, lifetime-stable id of this instance; also the kv lease owner |
| — | `SELF.name` | an id that survives replacement, safe to persist as a cluster member name. **Empty on ECS**, which has none |
| — | `SELF.ipv4` / `SELF.ipv6` | this instance's own addresses, so `ifaddr(cidr)` is no longer needed to find them |
| `TASK.cluster` | `SELF.group` | "cluster" now means only the workload's own cluster |
| `TASK.service_name` | `SELF.service` | matches the `service=` kwarg |
| `TASK.availability_zone` | `SELF.zone` | `SELF.region` is new |
| `TASK.vpc_id` | `SELF.network` | |
| `TASK.created_at` | `SELF.created_at` | unchanged |
| `TASK.family`, `.revision` | `SELF.aws.family`, `.revision` | AWS-only; absent on other providers |
| — | `PROVIDER` | `"aws"`; set even when `SELF` is `None` |
| `all_ecs_tasks_running(cluster=…, service=…)` | `all_replicas_running(group=…, service=…)` | argument **order** is unchanged |
| `health_status="HEALTHY_OR_ELSE_ALL"` | `health_status="HEALTHY_OR_ALL"` | `HEALTHY`/`UNHEALTHY`/`ALL` are unchanged |
| `-kv-table <name>` | `-kv-store <name>` | the store is a table only on AWS |
| `-kv-create-table` | `-kv-create` | |

`-namespace`, `-kv-key-prefix`, `instances()`, and everything about `spawn()` and the promise primitives are unchanged.

- **`SELF.id` is byte-identical to `TASK.task_arn` on AWS**, so migrating to it changes nothing there. A script that *parses* it (`arn:aws:…`) is provider-locked and should say so by reading `SELF.aws.task_arn` instead.
- **Do not switch a persisted member name to `SELF.name` on a live cluster.** etcd, PD and Consul remember member names; changing the derivation orphans the existing member. It is empty on ECS anyway — keep deriving from `SELF.ipv4` there.
- **`SELF` is `None` when the platform gave muster nothing**, exactly as `TASK` was, so an `if SELF:` guard still works. `PROVIDER` is separate precisely so a script can still branch on the platform in that case.
- **The script and the flags fail differently, and the flags are the risk.** A renamed builtin is a load-time error, so muster exits before spawning anything. A renamed flag is caught by the flag parser, which exits non-zero as PID 1 — but the script ships inside your image while `-kv-table` lives in a task definition, so the flags are the half that can lag the binary. **Update the task definition and the image in the same deploy.** Passing a removed flag names its replacement rather than reporting an undefined flag.

#### From the template-based releases

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

## Testing

### Unit tests

```bash
go test ./...              # the default (AWS) build
go test -tags gcp ./...    # the Google Cloud build
```

These cover seed election and the lifecycle hooks against the in-memory kv
store, and run the **kv conformance suite** — the same set of assertions every
backend has to satisfy, so a second implementation cannot quietly disagree about
when a lease has expired or who is allowed to renew it. The Google Cloud store
runs it against an in-process fake Cloud Storage that implements the generation
preconditions its correctness rests on; it caught two real bugs on the way in.

The Firestore store runs the same suite against Google's emulator, which is a
Java process — so it is skipped unless `FIRESTORE_EMULATOR_HOST` is set. Docker
supplies the runtime if you would rather not install one:

```bash
docker run -d --name muster-fs-emu -p 8484:8484 \
  gcr.io/google.com/cloudsdktool/google-cloud-cli:emulators \
  gcloud emulators firestore start --host-port=0.0.0.0:8484
FIRESTORE_EMULATOR_HOST=127.0.0.1:8484 go test -tags=gcp -race ./internal/provider/gcp/
```

There is no Service Directory emulator, so that path is covered by fakes at the
interface only.

The Cloud Storage store can also be run against a **real bucket**, which is
where contention and retries actually happen. It costs a few cents and is the
one part of the Google Cloud provider subtle enough to be worth it:

```bash
MUSTER_GCP_KV_BUCKET=my-bucket \
  go test -tags=gcp,gcp_live ./internal/provider/gcp/
```

### Emulator suite (AWS)

An opt-in end-to-end test exercises the DynamoDB-backed kv store and seed election against a real, stateful AWS emulator — [Winterbäume](https://github.com/moriyoshi/winterbaume) in its standalone `winterbaume-server` mode, which speaks the DynamoDB, ECS, and CloudMap APIs over one HTTP endpoint. It is build-tag gated and skipped unless an endpoint is provided:

```bash
winterbaume-server &   # or any DynamoDB-compatible endpoint
AWS_ENDPOINT_URL=http://127.0.0.1:8080 AWS_REGION=us-east-1 \
  go test -tags=e2e -run E2E ./...
```

It runs the same conformance suite as the unit tests, against the real store.

### TiKV on a real cluster

A second, heavier pair of end-to-end suites runs a real TiKV
cluster — three PD replicas and three stores, with muster as the entrypoint —
on throwaway infrastructure each provisions with Terraform.

[`e2e/tikv/aws`](e2e/tikv/aws) runs it as Fargate tasks on ECS. It is
the only test that can see a split brain: it asks every PD replica, on its own
loopback via ECS Exec, and requires them to agree on one cluster id, then stops
a PD task and checks that its replacement *joins* rather than bootstrapping a
second cluster. Nothing in the stack is reachable from outside the VPC. It
creates billable AWS resources and is gated behind both a build tag and an
environment variable:

```bash
cd e2e/tikv/aws && make e2e     # provision, assert, tear down
```

[`e2e/tikv/gcp`](e2e/tikv/gcp) is the Google Cloud counterpart: the same cluster
on Cloud Run **worker pools** — the one runtime whose instances are addressable
at their VPC address, and so the only one that can host a Raft group. Nothing in
that stack is reachable from outside the VPC and there is no bastion, so each PD
replica reports its own view on a loop and the test reads those back: asking
*every* replica about *itself* is what makes the split-brain check exhaustive,
the same property the ECS suite gets from shelling into each task.

```bash
cd e2e/tikv/gcp && make e2e
```

Both suites' Starlark scripts are loaded and checked by the ordinary `go test
./...` run, so a syntax error surfaces without a cloud account.
