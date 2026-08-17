# Journal

## 2026-08-12 – 13 — Multi-cloud providers (AWS + Google Cloud)

Branch `feat/multicloud-providers` (PR #8), squashed to one commit off
`0cac981`, plus the follow-ups merged since. Every commit leaves
`go test ./...` green.

muster was AWS-only in every cloud-facing part: CloudMap for discovery, DynamoDB
for the kv store, the ECS task metadata endpoint for identity, ECS
`DescribeServices` for the "are all replicas up?" precondition. The project had
already been renamed from `aws-servicediscovery-bootstrapper` to the neutral
`muster`; this made the code match the name and added a Google Cloud provider.

### What was built

Four capabilities behind interfaces in `internal/provider`, none of which may
import a cloud SDK:

| Capability | AWS | Google Cloud |
| --- | --- | --- |
| Discovery | CloudMap `DiscoverInstances` | Service Directory `ResolveService` |
| KV store | DynamoDB conditional writes | Cloud Storage generation preconditions, or Firestore transactions |
| Identity | ECS task metadata endpoint | Cloud Run env + the metadata server |
| Replica status | ECS `RunningCount == DesiredCount` | *unsupported* — Cloud Run does not expose it |
| Registration | *unsupported* — Service Connect does it | Service Directory `CreateEndpoint` |

GCP targets **Cloud Run**. See the rescope below — the first cut targeted
stateful managed instance groups, and GCE turned out to be out of scope.

Script-facing surface renamed in a clean break, no aliases: `TASK`→`SELF` with
neutral fields, `all_ecs_tasks_running()`→`all_replicas_running(group=,
service=)`, `HEALTHY_OR_ELSE_ALL`→`HEALTHY_OR_ALL`, `-kv-table`→`-kv-store`,
`-kv-create-table`→`-kv-create`. New: `PROVIDER`, `register()`/`deregister()`,
`-provider`, `-provider-opt k=v`, `-provider-help`.

### Findings

**The conformance suite found three real bugs.** It was the highest-leverage
thing in the plan and it earned that on the first run, which is worth recording
because none of the three would have survived to anyone's attention otherwise —
each returns a plausible boolean and only shows up as a lock that never releases.

1. **`memKV.Renew` accepted a permanent key** where `dynamoKV` refuses it (its
   condition is `expires_at > :now`, which an absent attribute never satisfies).
   DynamoDB is right: renewing a key with no lease quietly converts it into one
   that can then expire, so a deliberately permanent key becomes reclaimable.
   The old `memKV` tests never covered it.

2. **`gcsKV` stored the lease length in whole seconds**, copied from DynamoDB —
   where it has to be, because `expires_at` *is* the service's TTL attribute.
   Here the metadata is ours, and a sub-second lease truncated to `0` read back
   as "no lease at all": a permanent key nobody could ever reclaim. Now
   milliseconds.

3. **`storage.Reader.Metadata()` returns HTTP-canonicalised keys.** Custom
   metadata comes back as `x-goog-meta-*` headers and Go canonicalises the
   names, so `ttl_ms` reads back as `Ttl_ms` and parses as absent — the same
   silent permanent-key failure as (2), by a different route. `Reader`'s
   `LastModified` also carries only whole seconds. `read()` now takes
   `ObjectAttrs` (correct names, full-precision `Updated`) and pins the body
   read to the generation it returned, which costs a second call and gets both
   right.

(2) and (3) were found by running the suite against `fake-gcs-server`, which
implements the generation preconditions the design rests on — a hand-written
fake would simply have agreed with whatever the store did.

**A silent test-degradation hazard, now guarded.** `e2e_tikv_scripts_test.go`
sets `MUSTER_*` env vars that `pd.star` reads. If the names drift apart, `env()`
returns `None`, the script takes its "target unknown" branch, and
`TestE2ETiKVLineupNeverRaises` keeps passing while no longer exercising the
API-failure path it was written for. The test now asserts the loaded globals
hold what was set. Verified by desyncing a name on purpose: the guard fails, and
without it the suite stayed green.

**An ordering trap in the provider interface.** `DynamoKV` takes its lease owner
at construction and `Renew` conditions on it. A provider that resolved identity
lazily *after* building the store would hand out leases nobody can renew — a bug
that only manifests in production, on a long-lived seed lease. Providers
therefore memoize `Self` and their `KV()` calls it; this is documented on the
`Provider` interface rather than left to each implementation to rediscover.

**GCE needs a compound lease owner.** After autohealing, a replacement VM has
the *same instance name* and a new numeric id. A name-only owner would let it
renew a lease its predecessor took and it never held, defeating the entire owner
check. `Identity.ID` is `name + "/" + id` on GCP for that reason.

**Build tags shrink the binary, not the module graph.** `go mod tidy` is
tag-agnostic, so `cloud.google.com/*` lands in `go.mod` either way and
`go mod download` fetches it for both builds. What the tags buy is linkage, and
that is what matters: default build 63 `aws-sdk-go-v2` packages and **zero**
Google ones, `-tags gcp` the exact reverse. Binaries 15.9 MB and 48.1 MB — a
single fat binary would have cost every AWS user the larger figure. CI asserts
both directions, because nothing else notices an import that escaped its
constraint until someone measures the artifact.

**Two Go build-tag details worth knowing.** A package whose files are *all*
excluded fails `go build ./...` with "build constraints exclude all Go files",
so `internal/provider/gcp/doc.go` carries no constraint. And a `_gcp.go` filename
suffix is *not* a build constraint (unlike `_linux.go`) — every tagged file has
an explicit `//go:build` line.

**Autodetection is env-only, and that constrained the runtime choice.** Probing
`169.254.169.254` to identify a platform would cost a timeout on *every*
container start on every other platform, so a runtime with nothing
distinguishing in its environment simply is not autodetectable. That was the
first cut's problem — a MIG instance is an ordinary VM — and it is why Cloud Run
is not: `K_SERVICE` and `CLOUD_RUN_JOB` are reliable.

The detection table lives in the neutral package rather than on `Factory`
specifically so it can answer for providers the binary *lacks*: an AWS-only
muster started on Cloud Run says "rebuild with `-tags gcp`" rather than failing
on a credentials error twenty seconds later.

**`-provider-opt` over per-cloud flags.** `flag.Parse` runs before the provider
is selected, so `-gcp-project` on a binary built for another cloud yields "flag
provided but not defined" — exactly the un-actionable error the registry exists
to remove — while pre-registering every cloud's flags in every binary defeats
the point of building them separately. `ValidateOptions` rejects undeclared keys
because an option silently ignored looks applied; `-provider-help` restores the
discoverability this costs.

**`SELF.name` is empty on both providers, and that is the design.** It means
"an identity that survives replacement", which neither a Fargate task nor a
Cloud Run instance has. Filling it in with a task-id or instance-id suffix would
hand scripts a member name that silently changes on every replacement and
orphans the etcd member behind it. Scripts read the emptiness as "derive a name
some other way" — which is why the field exists at all rather than being
dropped: a runtime that *does* have one (a stateful MIG, a StatefulSet pod) can
populate it without any script changing.

**Where GCP is worse, and fails loudly rather than quietly.** Service Directory
endpoints carry no health status, so anything but `health_status="ALL"` raises
instead of widening to "return everything" — handing a script dead peers it
explicitly asked not to see would make it join a cluster that is not there.
`ResolveService` truncates at 100 endpoints (logged when hit). An endpoint
carries one address, not a v4/v6 pair.

**Service Directory endpoint ids are 63 characters**, `[a-z0-9-]`, starting with
a letter — and a Cloud Run instance id is around 200 hex characters, so the
obvious key does not fit. Endpoints are keyed on the *address* instead
(`ip-10-128-253-19`), which is shorter than the limit, unique among live
instances, and has the useful property that a replacement at the same address
inherits the entry rather than adding a second one. The cost is that an instance
does not reclaim its own entry across replacement, which was already true for a
different reason. `SELF.created_at` is empty: the metadata
server does not carry it, and the only source would drag in `computepb` — a
single generated package covering the entire Compute API — for one field
nothing reads.

### Configuration travels as environment variables

Every flag is also readable as `MUSTER_<FLAG>` — `-kv-store` from
`MUSTER_KV_STORE`, and so on — with an explicitly passed flag always winning.

This started as a special case for `MUSTER_PROVIDER` and should not have stayed
one. muster is a container entrypoint: the entrypoint is baked into the image
while the configuration is per deployment, and an image whose entrypoint ends in
`-- <workload>` cannot take extra flags by appending arguments. A platform that
lets you set only environment and arguments then leaves no way to pass a
setting at all.

Two flags are exempt (`-health-probe`, `-provider-help`): a stray variable in
someone's shell turning an ordinary run into a health probe or a help dump is a
failure mode worth foreclosing. `-provider-opt` is repeatable, so its variable
holds a comma-separated list.

**The near-miss this came from, and the guard against the next one.** The Cloud
Run stack passed `MUSTER_KV_BUCKET`, which is not what the flag is called, so
the configuration was simply absent and PD refused to start — a failure that
cost a full provision to discover and would have been a one-line diff to
prevent. Now that every flag has a variable, plausible-but-wrong *names* are the
remaining hazard, and `MUSTER_KV_BUCKET` looks exactly as reasonable as
`MUSTER_KV_STORE`. `TestE2EStackEnvIsConsumed` walks both stacks' Terraform and
fails on any `MUSTER_*` variable that neither muster nor a script that stack
deploys reads. It deliberately does not check the converse: `env()` takes a
default, and both PD scripts rely on that.

### The AWS scripts had drifted, and it was the AWS ones that were stale

`SELF.ipv4` was added for every provider, and the Cloud Run scripts use it. The
AWS scripts were never converted — they still picked their address off the
interface with `ifaddr(MUSTER_SUBNET_CIDR)`, a **required** variable carrying a
value muster already had in the task metadata. So the two stacks disagreed about
the most basic question either script asks, and a deployment that forgot the
variable failed at load time for nothing.

`SELF.ipv4` is now the source on both. `ifaddr()` stayed as a *fallback* rather
than being deleted, which is a deviation from the original plan and deliberate:
muster reads the task metadata once at startup, best-effort and without retry,
so a transient failure leaves `SELF` empty for the life of the process, and a
task that cannot name its own address cannot be a PD member at all. The Cloud
Run script keeps failing outright in that case, because an empty `SELF.ipv4`
means something different there — the workload was deployed on a service or a
job rather than a worker pool, a deployment mistake no fallback should paper
over.

The general shape is worth keeping: two scripts differing in *what an absent
answer means* is a platform difference; two scripts differing in where they get
the answer is drift.

### The rescope, and what it cost

The GCP provider was built twice. The first cut targeted stateful regional
managed instance groups, including a full six-VM TiKV e2e suite; then "GKE and
GCEs are out — the only option is Cloud Run" made all of it unbuildable.

**The finding that matters, and the mistake I made getting to it.** I asserted
that Cloud Run instances cannot address each other, and rescoped the provider on
that basis. It is true of *services* and *jobs* — Direct VPC egress assigns an
address to send *from*, and inbound goes through the Cloud Run frontend — but
**worker pools support Direct VPC ingress**: each instance gets a private
address on the VPC and other resources, including other instances, can connect
to it there. A peer cluster on Cloud Run is possible.

The correction cost four defects that shipped for about an hour: `SELF.ipv4`
hardcoded empty, worker pools missing from autodetection entirely,
`CLOUD_RUN_WORKER_POOL` not read as the service name, and `register()`/
`deregister()` removed on the grounds that there was never an address to
publish. All four are fixed, and the per-runtime difference is now pinned by
tests rather than by a claim in a comment.

The lesson is narrow and worth keeping: I reasoned from "Cloud Run" as though it
were one runtime. It is three, and they differ in exactly the property the
provider is built around. The user pushing back is what surfaced it; a web
search settled it in two minutes, against a claim I had already spent 3,000
lines of deletion on.

What the corrected picture decides about the provider:

- `SELF.ipv4` is the instance's VPC address on a **worker pool**, and empty on a
  service or job — where publishing the egress address would read as "connect to
  me here", which is false. `register()` refuses there for the same reason.
- `instances()` resolves peers on a worker pool, and **dependencies** — a
  database, a broker set, a cluster elsewhere — on a service or job.
- `SELF.name` is empty, as on Fargate. An instance gets a fresh id per scale
  event; `SELF.id` is that id, which is all a lease owner needs to be — unique —
  and correctly says a new instance never held the previous one's lease.
- `all_replicas_running()` raises. Cloud Run deliberately does not expose
  per-instance counts, and the only source is a Cloud Monitoring metric delayed
  by minutes. Returning a stale answer to a *precondition* is worse than refusing.
- Cloud Run's zone is the region with `-1` appended. It is not a zone and is not
  reported as one.

`register()`/`deregister()` survive, keyed on the instance id. That id does *not*
survive replacement, so unlike a platform with stable names a restarted instance
does not reclaim its own entry and one killed without teardown leaves a stale
endpoint. Documented rather than solved; scripts should probe discovered peers,
which is the portable habit regardless.

One thing got *better*: autodetection now covers Google Cloud. A MIG instance
was an ordinary VM with nothing in its environment to key off, which is why the
first cut required `MUSTER_PROVIDER=gcp`. Cloud Run sets `K_SERVICE`,
`CLOUD_RUN_JOB` or `CLOUD_RUN_WORKER_POOL`.

**What survived intact: the kv store.** GCS conditional writes need no peer
addressing, so the highest-risk code — and both bugs the conformance suite
caught — was untouched by the rescope. Roughly 3,000 lines of MIG Terraform,
startup script, harness and scripts were deleted.

### The end-to-end suites

`e2e/tikv` became `e2e/tikv/{aws,gcp}`. The GCP half was written three times:
for stateful MIGs, then deleted as impossible, then rebuilt on Cloud Run worker
pools. Both deletions came from a claim of mine that was wrong.

The second one: I said TiKV could not run on Cloud Run for want of a persistent
disk. Worker pools have a **10 GiB ephemeral disk** (`medium = "DISK"`, since
the default `MEMORY` is charged against the instance's memory limit). Ephemeral
means lost when the instance shuts down — but that is *exactly* Fargate's deal,
and the AWS suite runs there. I cited a constraint the working suite already
survives, which I should have noticed.

So the GCP scripts are near-copies of the ECS ones, and for the right reason:
same ephemeral storage, same absence of stable identity, same address churn, so
the same member-name-from-address and the same evict-in-pre_stop. The one hard
difference is the shutdown budget — ECS allows 30s and up to 120s, Cloud Run
sends SIGTERM and SIGKILLs 10 seconds later, not tunable. `pre_stop` must
release the lease and evict the member inside it, so the timeouts are 5s/8s
rather than 20s/30s.

Observation had to change shape, though. Nothing in the stack is reachable from
outside the VPC and there is no bastion — that would mean a Compute Engine VM,
which is out of scope. So each PD replica reports its own view on a loop and the
test reads Cloud Logging. That is not a workaround: asking one replica through a
load balancer could never see a split brain, and asking every replica about
itself is what the ECS suite achieves by shelling into each task.

`scaling_mode = "MANUAL"` on both pools: a Raft group needs a known, odd number
of members, and an autoscaler deciding otherwise is a split brain waiting to
happen. `PDReplacementRejoins` rolls the whole PD revision rather than stopping
one instance as the ECS version does — nothing on disk outlives an instance
here, so surviving a total turnover carried by the seed lease and the peers is
the strongest statement the platform allows.

Both stacks' scripts are loaded by the ordinary `go test ./...`, and the GCP
ones are additionally checked for the two things that differ: the member name
derived from the address, and `me()` refusing when `SELF.ipv4` is empty, which
means it has been deployed to a service or a job where no peer could reach it.

Shared underneath both suites: the transcript parser (the AWS one drives a shell
over a session that interleaves its own diagnostics) and the
make/terraform/eventually plumbing. Note the Go `internal` rule forced these up
to `e2e/internal/`: from `e2e/tikv/internal/`, a sibling suite could not import
them.

### Two kv backends on Google Cloud, and why

Cloud Storage came first because provisioning decides it: `-kv-create` is one
idempotent `buckets.insert`, where a Firestore database is a long-running
operation and a project-level decision. Generation preconditions also give a
compare-and-swap that is immune to a value being changed and changed back —
something the DynamoDB backend's value comparison cannot see.

Firestore was added as a choice rather than a replacement, because the two lose
on different axes and neither loss is universal:

- Firestore expresses each operation as one transaction, so put-if-absent and
  compare-and-swap read as the semantics are stated instead of as an encoding of
  them, and cost one round trip rather than two.
- Firestore has **no per-database IAM**, so `roles/datastore.user` is necessarily
  project-scoped. A bucket grant is scoped to the bucket. If least privilege
  matters more than round trips, that alone settles it.
- Neither can delegate expiry. Firestore's native TTL deletes lazily, typically
  within 24 hours; DynamoDB's is lazy too; the storage lifecycle rule is
  day-granular. All three filter expiry on read and treat the platform's reaper
  as a janitor.

Both anchor expiry on the server's write timestamp — `attrs.Updated` and
`snap.UpdateTime` — rather than on a deadline the writer computed, which keeps
clock skew between nodes out of seed election. The conformance suite is what
makes the choice safe to offer: it is the same 13 subtests either way.

**Verified against the emulator, via Docker.** There is no in-process Firestore
fake -- the emulator is a Java process -- and this machine has no JRE, which at
first read like the backend would ship unrun. It does not: the emulator runs in
a container with nothing installed on the host.

```
docker run -d --name muster-fs-emu -p 8484:8484 \
  gcr.io/google.com/cloudsdktool/google-cloud-cli:emulators \
  gcloud emulators firestore start --host-port=0.0.0.0:8484
FIRESTORE_EMULATOR_HOST=127.0.0.1:8484 go test -tags=gcp -race ./internal/provider/gcp/
```

All 13 subtests pass, stable across repeated runs under `-race`, including
ExactlyOneWinsTheSeedRace. What that leaves unproven is contention *in
production*: the seed race serializing here is a statement about the emulator's
implementation of transactions, not Google's.

The detour worth recording is the one not taken. A mocked Firestore client --
wrapper interfaces plus gomock -- was considered and rejected, because
`firestoreKV` is almost nothing *but* assumptions about the server: whether
`tx.Get` reports a missing document as an error or an absent snapshot, whether
`snap.UpdateTime` advances on `tx.Set`, whether `RunTransaction` re-runs the
closure on contention, whether two transactions on one document serialize. A
fake is programmed from the same mental model that wrote the code, so it cannot
falsify any of them; it would have turned an honest skip into a green run that
proved nothing. It would also have inverted the design, since `provider.KVStore`
is already the seam and `internal/provider/gcp` exists to be the concrete side
of it.

### The silent background task

The first Google Cloud run assembled the cluster correctly and the suite still
had nothing to assert on. Seed election was clean -- one replica took the lease
and bootstrapped, two followed it, one cluster id, quorum formed -- and across
eleven minutes the three PD replicas logged not one of the self-reports the
whole observation strategy is built on.

`main()` starts the reporting loop with `go(report)` and never joins it. The
loop's first sample runs immediately, alongside `spawn()` and therefore before
PD is listening -- resolve has a peer election to get through first.
`http_request` raises on a refused connection, Starlark has no way to catch a
raise, so the task rejected on its first iteration. Nothing joined it, so the
error was discarded. Total silence, which is indistinguishable from a loop with
nothing to say.

Two fixes, because the bug had two halves.

The script's half is structural containment: each sample runs on its own task
and `select()` awaits it, which unlike `join()` does not re-raise. That is the
only way to express "skip a failure" in a language without exceptions, and
`lineup()` in the same file already used the idiom.

muster's half is that a rejection nobody collects is now logged. `join()`,
`select()` and `any_true()` mark a promise as observed; a task that rejects
without any of them having done so gets a warning naming the task and the error,
after a short grace period so a joiner racing the rejection is not
double-reported. A supervisor whose job is to make failures visible should not
have a way to lose one silently.

### The file-descriptor cap

TiKV would not start on Cloud Run: `the maximum number of open file descriptors
is too small, got 25000, expect greater or equal to 123880`. The expectation is
`1000 + rocksdb.max-open-files + raftdb.max-open-files`, with the rocksdb term
counted twice while Titan is enabled -- exactly 1000 + 3 × 40960 at the stock
settings, which is where the oddly specific number comes from.

The ECS stack never met this because it asks for the limit it wants: the task
definition carries `ulimit nofile = 123880` and Fargate grants it. Cloud Run
caps a container at 25000 and that cap is the *hard* limit, so raising the soft
limit from inside is impossible -- it needs `CAP_SYS_RESOURCE`, which the
sandbox does not grant -- and no worker pool setting exposes it. Which also
settles the tempting idea of teaching muster to raise `RLIMIT_NOFILE` before
exec: a sound thing for a PID 1 to do in general, and worth nothing here.

So the demand comes down instead of the limit going up, in
`docker/tikv-node/tikv.toml`: `max-open-files = 4096` on both engines, needing
13288. It is a cap on RocksDB's table cache rather than a correctness setting --
past it, SST files are closed and reopened on demand -- so the cost of being
wrong is latency, not failure.

Verified locally rather than argued: a single-node PD plus a store under
`--ulimit nofile=25000:25000` reproduces the production FATAL from the same
source line, and the same pair with the config file mounted bootstraps the
cluster and reaches `Up`.

### The readiness check that read the wrong API

`PoolsReachReady` waited out its full fifteen minutes on two worker pools that
had been ready for most of it, reporting `condition  is :` — two empty fields
where the answer belonged.

`gcloud run` renders Cloud Run resources in the **Knative** shape:
`apiVersion`/`kind`/`metadata`/`spec`/`status`, with `status.conditions[]`
carrying `type` and `status`. The check read the **v2 API's** shape instead —
a flat `terminalCondition` whose `state` is `CONDITION_SUCCEEDED`. That is the
spelling the Terraform provider stores, which is where it came from, and against
gcloud's output those keys do not exist at all. So `encoding/json` filled in
nothing, the state never equalled `CONDITION_SUCCEEDED`, and the poll could
never succeed.

The wrong field names are the shallow half. The real defect is that a check
which cannot find the field it needs reported an empty value rather than a
mismatch — and an empty value reads as a workload problem, so it sends you to
the pods' logs instead of to the checker. It now says so explicitly, and that
branch is the case the test exists for.

The parsing moved to `e2e/internal/cloudrun` for one reason: at its old address,
inside a build-tagged harness that needs a real project, it could not be tested
at all. It now has fixtures — real captured output, the v2 shape it used to
misread, a failed revision, a mid-reconcile pool, and a pool whose newest
revision never became the ready one, which is what `PDReplacementRejoins`
depends on noticing. The fix was also run against the live pools before landing.

### Three ways to read a log and find nothing

`PDClusterBootstrapped` sat for its full twenty minutes reporting that no
replica had reported a cluster, while three replicas were reporting one every
fifteen seconds. Three separate defects, stacked, and every one of them failed
by returning an empty result rather than an error.

1. **The filter named `resource.type=cloud_run_worker`.** The type is
   `cloud_run_worker_pool`. Cloud Logging answers an unknown resource type with
   an empty result set, not a complaint, so the query was simply always empty.
2. **The output was read as `value(textPayload)`.** muster logs structured JSON
   on stdout, which the logging agent parses into `jsonPayload`; `textPayload`
   carries only what the workload itself printed. So even with the right filter,
   every line muster wrote came back blank — and `dumpLogs`, the thing you reach
   for when a run fails, was blind to muster in exactly the same way.
3. **`--order=asc` with `--limit=4000` truncates at the wrong end.** PD emits
   thousands of lines an hour, so a limit is unavoidable and ascending order
   spends it on the oldest entries in the window. The first read against a live
   cluster returned four thousand entries from half past eight that morning,
   containing no self-reports at all. Newest-first, and `LatestReports` picks by
   timestamp rather than trusting the order it was handed.

Fixing those exposed a fourth, which would have failed `NoSplitBrain` on a
healthy cluster. **A pool's logs outlive its instances.** An hour-wide window
holds replicas from revisions that no longer exist, and those reported a
different cluster id — which is exactly what the split-brain check is looking
for. Read unscoped, the live pool offered five replicas and two clusters; scoped
to the revision the pool has settled on, three and one. Assertions now scope;
the failure dump deliberately does not, because there you want everything.

The through-line is the same as the readiness check that preceded it: every
failure here was a *silent empty result*, and the fix is only incidentally the
right field names. What actually changed is that this parsing now lives in
`e2e/internal/cloudrun` with a captured log entry as a fixture, so the resource
type, the revision label and both payload shapes are pinned against real data
instead of against my memory of an API. Verified against the live cluster as
well: three replicas, one cluster id, from the current revision only.

### Replacing one PD instance, four attempts

Seven of the eight subtests passed the first time the suite ran end to end and
have passed every run since. The eighth took four attempts, and what it kept
teaching was not about muster.

**Attempt 1 — roll the revision.** A Cloud Run revision update replaces *every*
instance of a worker pool, and with three ephemeral disks discarded together
there is nothing left to carry the cluster: the seed lease holds an address, not
cluster state, and PD cannot be told to adopt an id. So the new tier bootstrapped
a new cluster, correctly — id ...4512 before, ...0939 after. The test asserted a
property of Fargate, where the ECS suite stops *one* task.

Two things about how it reported that were worse than the failure. The
mismatched id was visible in the first poll, but the check counted replicas
before comparing what they said, so the one failure this subtest exists to catch
appeared as `2 of 3 replicas have reported` followed by a twenty-minute timeout.
And the dump could not have explained it either: three hundred lines of a pool
where PD writes thousands an hour is about ninety seconds of raft chatter, with
none of muster's decisions in it.

**Attempt 2 — deploy unpromoted, move a third of the instances.** muster did
this correctly, and the dump built for attempt 1 is what showed it:

```
04:07:09  pd: joining the running cluster   name="pd-10-128-253-22"
04:07:26  running pre_stop
04:07:27  pd: dropped member                name="pd-10-128-253-16"
04:07:28  workload torn down
```

The replacement joined, the departing instance evicted itself from the Raft
group **inside Cloud Run's fixed ten-second budget** — the platform constraint
most likely to be fatal here — and the cluster id never moved.

It failed on `10.128.253.16 sees 4 members ... want 3`, for twenty minutes.
`.16` is the replica that was replaced: its last report was filed mid-handover,
with the replacement joined and itself not yet evicted, and then it was gone.
`LatestReports` returns the newest report *per replica*, so that one stands
forever. **A self-report outlives the replica that wrote it** — the third time
that cost a run, after two that revision scoping had answered. This one needed
liveness instead, because during a replacement the survivors and the replacement
are legitimately on different revisions, and muster already publishes liveness:
Service Directory, withdrawn by the same `pre_stop` that evicts the member.
Scoping to registered replicas also let the assertion compare member *names*
rather than count them, so a group that swapped an evicted member for a stale one
now fails instead of reaching three.

**Attempt 3 — the same split, and the platform disagreed again.** Four replicas
registered themselves and stayed. Under `MANUAL` scaling the instance count is
honoured **per revision**, so the pool ran the old revision's three alongside the
new one's one. Attempt 1 asserted something the platform cannot do; attempt 3
assumed something it does differently. Both were wrong about the same thing —
what a worker pool does when you ask it to change.

Two genuine bugs surfaced there, neither one the test was looking for.

`pd_pre_stop` **skipped `deregister()` when a peer was already gone.**
`http_request` raises on a transport error, Starlark cannot catch it, so a failed
member DELETE aborted the rest of `pre_stop`. The instance left its Service
Directory endpoint behind: the cluster was fine and *discovery* was not, which is
the worse of the two, because every peer that later reads discovery believes in a
replica that is gone. Contained the same way as the reporting loop — each
fallible step on its own task, awaited with `select()`. That is now the third
place in one script where the fix is "a raise must not escape", which is worth
stating as a rule: **in Starlark, containment is structural or it does not
exist.**

And muster **reported a lost task failure on every clean shutdown**. The
unobserved-rejection warning fired for background loops cancelled during
teardown, which had not failed — they were stopped. It stays quiet now when the
parent context is already done. A diagnostic that cries wolf at every shutdown is
unlearned within a week, which would have cost more than it ever saved.

The dump was empty again too, for a new reason: `--limit` bounds what a Cloud
Logging query *returns*, not what matches it, and four thousand entries from the
PD pool cover about fifteen seconds. Selecting muster's lines from that in Go
means they were never fetched. The filter is server-side now.

**Attempt 4 — the instance count.** It lives on the pool rather than the revision
template, so changing it scales in place and rolls nothing: **3 → 2 → 3**. Going
down, an instance stops and `pre_stop` must evict its member inside the ten
seconds. Coming back up, a new instance appears at a new address with an empty
disk and must join rather than bootstrap. That is the ECS claim, both halves are
asserted, and this is the approach that passes.

### Verified

- `gofmt` clean; build + vet under every tag (default, `e2e`, `e2e_tikv`,
  `e2e_tikv_gcp`, `gcp`, `gcp,gcp_live`).
- Full suite under the default and `gcp` builds, `-race -shuffle=on`.
- Dependency isolation asserted in both directions (see above).
- KV conformance (13 subtests) green against `memKV`, `gcsKV` and — via the
  emulator in Docker — `firestoreKV`.
- **Both end-to-end suites, against real infrastructure.** `e2e/tikv/aws` on
  ECS Fargate and `e2e/tikv/gcp` on Cloud Run worker pools, every subtest.
- `pre_stop` evicting a PD member from the Raft group inside Cloud Run's fixed
  ten-second budget, observed in production. This was the platform difference
  most likely to make the whole exercise impossible.
- Every startup error path smoke-tested against the real binaries: absent
  provider, nothing to autodetect, removed flag, cross-cloud autodetect,
  unknown `-provider-opt`.
- Starlark capability messages unchanged where they should be
  ("kv store not configured", "ECS client not configured" → now
  "replica status not configured").

### Both suites pass

**`e2e/tikv/gcp` is green, all eight subtests**, and **`e2e/tikv/aws` is green**
— which between them settle every claim this document was carrying on trust.

The AWS run is the regression gate: it is what proves the provider refactor did
not break seed election on the platform that already worked, and it also carries
a change of its own, `me()` reading `SELF.ipv4` instead of `ifaddr()`, which had
never been near Fargate until now.

The Google Cloud run is the new claim. A complete TiKV cluster — three PD
replicas and three stores — assembled by muster on Cloud Run worker pools:

| Assertion | Live state |
| --- | --- |
| `PoolsReachReady` | both pools `Ready=True`, reconciled onto their created revision |
| `ServiceDirectoryRegistrations` | 6 endpoints, 3 per tier — **every one written by `register()`**, since nothing on Cloud Run registers an instance |
| `PDClusterBootstrapped` | all three replicas report a non-zero cluster id |
| `NoSplitBrain` | all three report the *same* id, `7673139720976975013` |
| `QuorumComplete` | each sees 3 members, not just itself |
| `StoresUp` | 3 stores, `['Up', 'Up', 'Up']`, from each replica independently |
| `SeedLease` | held by `10.128.253.18`, which is a registered replica |
| `PDReplacementRejoins` | **passes** on the fourth approach — scale to 2 and back to 3; see below |

Seed election is the part the whole provider exists for, and on Cloud Run it is
no longer theoretical: from a cold start one replica took the lease and
bootstrapped, two followed it, and every replica agrees on one cluster. A
replacement then joined that same cluster rather than starting its own, with the
departing member evicted inside a ten-second budget.

Several fixes are confirmed in production by that table rather than by argument:
the store tier is up, so the file-descriptor config works; the numbers were
*read from the self-reports*, so the reporting loop survives its own failing
first samples; and the last run's dump shows a replaced replica evicting itself
from the Raft group inside Cloud Run's ten-second budget, which was the platform
constraint most likely to be fatal.

**Ten distinct failures have cost a provision, and not one was the election.**
In order: registry authentication; a configuration variable nothing read; the
reporting loop's silent death; TiKV's file-descriptor demand; a readiness check
reading the wrong API's field names; three stacked defects in the log reads; an
assertion the platform cannot satisfy; a repository check that swallowed an
expired credential and blamed a missing repository; a stale self-report from a
replica that had been replaced; and an instance split that added a fourth
replica instead of replacing one.

Seven of the ten were **silent** — an empty result, an unread variable, a
discarded error, a log with nothing in it, a message with only one thing it
could say. That is the finding this suite produced, more than any individual
bug: on this platform the expensive failures are not the ones that shout.

Three were in the *observation* of the cluster rather than the cluster, and all
three were the same idea in different clothes — **a self-report outlives the
replica that wrote it.** Revision scoping answered the first two; the third
needed liveness, because during a replacement the survivors and the replacement
are legitimately on different revisions.

Three more were **wrong about the platform rather than about muster**: what a
revision update does to a worker pool, what an instance split does under manual
scaling, and what `--limit` bounds in a Cloud Logging query. Each was a
confident assumption that read plausibly and cost a provision to disprove, and
each is now pinned by a fixture or written down here.

Still not verified, and needing infrastructure:

- **`-tags=e2e`** (Winterbäume emulator): compiles and vets, not run. Now runs
  the shared conformance suite against the real DynamoDB store. The least
  pressing of these, since both real clusters pass.
- **`-tags=gcp,gcp_live`**: skips without `MUSTER_GCP_KV_BUCKET`. The fake is
  single-process and cannot reproduce contention, retries, or the per-object
  write rate. The same gap applies to Firestore, where the emulator stands in
  for the server whose transaction semantics are the thing under test.
- The **Firestore backend against a real database** (`KV_BACKEND=firestore`) is
  the other Google Cloud path no run has touched.

### Deferred

- Stale Service Directory endpoint reaping (an instance killed *without*
  teardown leaves its entry). One that gets its ten seconds now withdraws its
  own, since `pre_stop` no longer loses the deregistration to a failed member
  eviction. Scripts probe peers before trusting them regardless.
- The Firestore backend against a real database. It satisfies the conformance
  suite on the emulator and has never met the server.
- Azure. The seam accommodates it; nothing else was done.
- Renaming the working directory to `~/Source/muster`. Zero diff — the module
  path and GitHub remote are already `github.com/moriyoshi/muster` — but it
  disrupts open shells and editors, so it was left to be done deliberately.

### Notes on the mechanics

Two things that cost time and are not findings about muster:

- **The branch requires signed commits.** `commit.gpgsign` was not set in this
  repository, `gpg.format=ssh` and `user.signingkey` were, so commits were
  simply unsigned and the PR was unmergeable. It is now set local to the repo.
- **`--location`, not `--region`, for Artifact Registry and Service Directory.**
  `gcloud run` takes `--region`; these take `--location`, and passing the wrong
  one produces "unrecognized arguments". Both are regional; only the spelling
  differs, and a suite that touches all three surfaces mixes them up.
- **A check that can only say one thing will say it when it is wrong.**
  `check-repo` discarded gcloud's stderr and reported "the repository does not
  exist. Run make bootstrap" — one provision after terraform had reported the
  repository present, with an expired credential as the actual cause. It now
  prints gcloud's own error first and blames a missing repository only when
  gcloud said so.
- **`docker login` normalises a path-qualified registry to its host**, which is
  why one variable is enough for both the Artifact Registry login and the image
  tags, and why an earlier two-variable version had one of them empty after a
  targeted `terraform apply` that never evaluated the output.

### If you pick this up next

Both suites pass, so there is nothing outstanding to repair. What is left is
work nobody has started:

- **The Firestore backend against a real database.** `KV_BACKEND=firestore make
  e2e` is wired and has never run; the emulator stands in for the server whose
  transaction semantics are the thing being relied on.
- **Azure.** The seam accommodates it and nothing else was done.
- **`-tags=e2e`**, the Winterbäume emulator suite, which now runs the shared
  conformance suite against the real DynamoDB store.

If a Cloud Run run does fail, the question to ask first is what the worker pool
actually did, not what muster decided. That was the answer to three failures out
of four, and `make logs-pd` now shows muster's decisions in full rather than a
minute of raft chatter.

Two habits are worth carrying into whatever comes next, because they are what
the nine failures actually taught:

**Put the parsing where it can be tested.** Every one of the observation bugs
lived in a build-tagged harness that needs a paid project to run, so nothing
could catch them but a provision. Moving them to `e2e/internal/cloudrun` with
captured API output as fixtures is what turned "read the docs again" into a test
that fails on the desk. That package is where the next one should go too.

**Make a check say what it does not know.** A readiness check that printed two
empty fields, a filter that matched nothing, a repository check with one
hypothesis — each cost a full provision, and each would have cost minutes if it
had been able to say "this is not the shape I expected". Silence is the
expensive failure mode here, which is also why muster now reports a task
rejection nobody joined.

## 2026-08-13 – 15 — Store replacement on ephemeral storage

Not planned work. A throwaway benchmark harness — `bench/`, untracked, outside
everything here — was built to ask how a muster-run TiKV cluster performs next
to the managed key/value service on each cloud. It turned out to be a good
detector for a fault in the deployment muster produces, and this section is
about that fault rather than about the numbers.

Everything below is now in `e2e/tikv/*` and covered by tests. Neither suite has
been run against it yet.

### The fault

A TiKV store's identity lives in its data directory. On Fargate and on Cloud
Run that directory is ephemeral, so **a replaced task is not a restarted store**:
it is a new, empty one, with a new store id at a new address. PD keeps the old
record and every region that had a replica on it.

With three stores and PD's default `max-replicas` of 3 the arithmetic is
unforgiving. Replacing one store leaves each region at 2/3 and survives.
Replacing two loses quorum. Replacing three is permanent loss. A `terraform
apply` that changes the store task definition rolls all three in about two
minutes, and nothing in either platform knows that is destructive.

Two asymmetries turn a degradation into a brick:

- **PD survives what the stores do not.** PD is a Raft group replaced one task
  at a time, so each new member rejoins and syncs from the survivors. Its
  metadata is immortal across rolls while the stores' data survives none of
  them. Restarting the cluster is therefore the operation that *creates* the bad
  state — a PD confidently serving region locations for stores that died two
  generations ago.
- **Every health signal said fine.** All six tasks reported HEALTHY throughout,
  because the probe answered "is my process up and serving" and every process
  was. The cluster could not serve a single region and the deployment completed
  successfully.

### How it was diagnosed, which is the transferable part

The presenting symptom was "the benchmark says TiKV is slow", then "TiKV stopped
working". The client log was thousands of `DeadlineExceeded` health checks and
prewrites retrying twenty-four times.

What settled it, in order:

- **The stores were idle.** CloudWatch put the tier at **2.7% CPU** against 87.6%
  memory. Overload was the obvious hypothesis and it was wrong: the stores were
  not thrashing, they were doing nothing, because they owned nothing.
- **The addresses did not exist.** The client was retrying `.28`, `.81`, `.181`;
  the live tasks were `.6`, `.13`, `.153`, and CloudMap agreed with the live
  ones. The three the client had came from PD.
- **They were two generations old.** `describe-tasks` on the stopped tasks put
  `.28`/`.81`/`.181` at a roll forty minutes earlier — not even the roll that
  had just happened.

The `tikv_memory` change that preceded the failure was a red herring: memory sat
at 87.6% both before and after, because TiKV sizes its caches to the cgroup
limit whatever it is given.

### Findings

**A store record's state is not a stale reading.** PD reports a vanished store
as `Disconnected` within about ninety seconds — `state_name` is a derived
display state covering Disconnected and Down, not only the administrative
Up/Offline/Tombstone. This was expected to need heartbeat-age arithmetic and
does not, which is what makes a cheap check possible.

**PD refuses to release a record below `max-replicas`, and says so.**

    DELETE /pd/api/v1/store/1
      → 400  [PD:core:ErrStoresNotEnough] can not remove store 1 since the
             number of up stores would be 0 while need 3

On a cluster running the minimum three stores for three replicas that refusal is
*routine*: the dead record cannot be released until the replacement registers
and brings the up count back to three. Verified against a real PD rather than
assumed, and it changed the design — the reconciliation loop treats this as the
expected path and says so plainly, or every normal replacement would log like a
fault. It also means the fix narrows the window rather than closing it, and it
is the strongest argument for running more stores than replicas: with four,
releasing a dead record leaves three up and PD allows it immediately.

**Readiness is evaluated once, which decides where a check may live.**
`watchReadiness` awaits its promise a single time: truthy latches healthy, false
restarts the workload, and past the health-check grace period the scheduler
replaces the task — which mints another dead store record. So a store's
readiness must not gate on *other* stores being stale; that turns one stale
store into a loop that manufactures more. The hard gate belongs in a client,
where a false positive costs nothing. This is the shape of the whole mitigation:
detect in the cluster, refuse outside it.

**Requiring the whole tier to be visible is a guard that never fires.** The
first version of the reconciliation guard demanded that discovery account for
every expected store before acting. It reads as the cautious choice and is
useless — during a rolling replacement exactly one store is legitimately
missing, which is precisely when there is a dead record to release. It is a
strict majority now (`n * 2 > expected`), extracted into
`discovery_is_trustworthy()` so both edges are pinned by a test.

**The reconciliation loop destroyed a healthy store, and the fix is a probe
rather than a better guess.** Found on the first real cluster to run it, and the
most important entry here.

PD identifies a store by its *address*. When a replacement task lands on an
address some earlier store used — routine in a small subnet — PD hands the new
process that old record, which is still `Down` because it has not heartbeated
yet. Discovery has not caught up with the new task either, because registration
lags process start by seconds. For that window the record is indistinguishable
from an abandoned one, and the loop released it:

    10:44:53  task starts at 172.31.253.87, empty disk, adopts store 1032
    10:45:01  prune sees 1032 @ .87 Down and .87 absent from CloudMap → deletes
    10:45:13  [ERROR] store heartbeat failed: StoreTombstone("store is tombstone")

Eight seconds. The store ran for the next twenty minutes, healthy by every ECS
signal, rejected by PD on every heartbeat, owning nothing. The majority guard
passed exactly as designed — two of three visible tolerates one missing during a
replacement — because the missing one had not gone anywhere, it had just
started.

This is worse than the problem it was written for. A stale record degrades
gracefully and PD recovers in thirty minutes; the loop *permanently* killed a
running store in eight seconds.

The fix is not a longer timer or a stricter majority; both are still guesses
about a race. It is a signal that cannot lag: **probe the store's own status
port before releasing its record.** Either something is listening at that
address or nothing is, and that answer is available immediately. Discovery
absence and PD's own state remain, but the probe is the last gate.

The second half is `reap_tombstones`. PD refuses to register a store at an
address a tombstone still holds, so on a platform that recycles addresses a
leftover tombstone eventually rejects a perfectly good replacement — which is
also why the store above could not simply be restarted into place. Clearing
them is a single call and was originally dismissed as cosmetic; it is not.

The general lesson is the one this project keeps paying for: a check that acts
on the *absence* of evidence needs a positive signal before it acts. Discovery
saying nothing and PD saying nothing are both absences, and two absences do not
add up to a fact.

**What can and cannot be healed without a human.** Three cases, and the third
is the interesting one.

*A store whose record was thrown away* now heals itself. Readiness distinguishes
three outcomes rather than two — ready, not yet, and never going to be — and
returns false immediately on `Tombstone` instead of polling out a
thirty-minute window. `resolve_tikv()` then refuses to start a store PD would
reject on every heartbeat, which exhausts `max_retries` in seconds and exits, and
the orchestrator replaces the task with a clean volume. `reap_tombstones` clears
the record so the replacement is not refused at the same address in turn. What
used to be a store sitting healthy-looking and useless until the grace period
expired is now a task replacement inside a minute.

*A stale record* heals itself, with the occupancy probe as the gate.

*A cluster whose data is entirely on stores that no longer exist* does not, and
`cluster_lost_its_data()` reports it rather than acting. Recovery means
`pd-ctl unsafe remove-failed-stores`, which discards whatever those regions
held; that is a policy decision, not a repair. Automating it would be a script
throwing data away on its own judgement — and that judgement is exactly the one
this loop has already been wrong about once. Detection is cheap and the log line
names the remedy, which is the right amount of help.

**The seed lease outlives the cluster.** It lives in DynamoDB or GCS, neither
ephemeral. After a full restart a new PD reads a lease naming a replica that no
longer exists and waits five minutes on it before failing. `await_seed()` now
releases such a lease — but only after that full five minutes of silence, only
when the seed is absent from a discovery answer large enough to be believed, and
then conditionally on the exact value it judged. Getting this wrong is a split
brain: releasing a lease held by a replica that is merely slow lets a second
replica bootstrap a second cluster. The delete is the cheap part; the evidence
is the point.

**PD's timestamp oracle is the highest-leverage knob in the deployment.** A
separate finding from the same benchmark, and measured rather than argued: PD at
half a vCPU produced 2989 `get timestamp too slow` warnings (mean 38.9 ms, max
81 ms) and a read p99 of 28.8 ms on a workload that touches no disk. At one vCPU
the warnings went to **zero** and the p99 to 1.9 ms — a fifteenfold improvement
in the tail from half a vCPU. Every TiKV transaction, read or write, fetches a
timestamp from the PD leader first, and that path is CPU-bound. What made it
findable was that the tail was *implausible*: 20 MB of data, entirely in block
cache, cannot produce a 29 ms read p99, so storage did not explain it and
something else had to. The stores never complained once in either run — reading
the client's own warnings rather than only the benchmark's summaries is what
separated PD from TiKV.

### What changed

In muster:

- **`json.decode` / `json.encode` / `json.indent`**, from starlark-go's own
  module. Scripts could previously only substring-match an HTTP body, which
  answers "did this reply" and nothing more. Every mitigation here depends on
  deciding from *what* an API said. `'"state_name":"Up"' in body` is true of a
  healthy cluster and of one where a single store is up and the rest are down —
  and that exact mistake was then made in the benchmark's shell preflight, which
  matched `"state_name":"..."` against a body PD pretty-prints with a space and
  cheerfully reported no stores down.

In `e2e/tikv/*`, both clouds:

- **Readiness means PD has accepted this store**, not that a local port answers.
  Staleness is reported loudly and deliberately not gated on; see above.
- **`pd.star` reconciles PD's store list with the stores that exist** — the same
  move `drop_member()` already makes one tier up for PD's own membership. On the
  leader, once a minute, it takes offline the first store PD believes in whose
  address is absent from discovery and which PD is no longer hearing from. The
  signal is discovery, not PD's own view: a task that restarts *in place* keeps
  its registration, so absence means the task is gone and the data with it —
  *and* nothing may be answering at the store's own status port, which is the
  gate that stops a just-started replacement from being mistaken for a corpse.
  One store per pass, so a single bad reading costs one record rather than all
  of them. Tombstones are cleared as they appear, or a recycled address stays
  poisoned. `store delete`, which marks a store Offline and removes each peer only
  after building a replacement — a region with no surviving replica keeps the
  store Offline forever, which is the honest outcome and the signal that
  `pd-ctl unsafe remove-failed-stores` and a human are required.
- **One task at a time.** ECS defaults to `minimumHealthyPercent 100` /
  `maximumPercent 200`, which for a three-task service permits three
  replacements at once. Both services now cap the ceiling at desired + 1,
  computed rather than hard-coded (`ceil((n+1)/n * 100)`: 134% at three tasks,
  125% at four). Cloud Run worker pools have no equivalent — checked against the
  provider schema, which offers `scaling` and `instance_splits` and nothing
  resembling `max_surge`. Documented rather than solved.
- **`StoreReplacementIsReaped`** on both suites, the tier below
  `PDReplacementRejoins`. AWS stops a store task; GCP scales the pool down and
  back up, for the reason that test documents at length. Both then assert PD's
  store list returns to exactly the addresses that exist, all Up, with the
  replaced address gone; AWS also asserts regions are back to full replication.
  The convergence window is fifteen minutes, deliberately shorter than
  `max-store-down-time`'s thirty — without the reconciliation loop the record
  simply sits there and the test times out, so it cannot pass for the wrong
  reason.

### Verified

- `go vet ./...`, `go test ./...`, `go vet` under both e2e build tags,
  `terraform validate` and `fmt` on both roots.
- All four scripts compile against the real muster binary.
- The decisions behind the reconciliation loop — `stale_store`,
  `discovery_is_trustworthy`, `seed_looks_abandoned` — are unit-tested in
  `e2e_scripts_test.go`, and most cases assert that the answer is "leave it
  alone". `seed_looks_abandoned` is asserted to *raise* without a discovery
  provider, because a decision of that weight must not be reachable without
  consulting discovery.
- `TestE2EScriptsLoad` gained every new function name, which is what catches a
  rename before a provision does.
- PD's API shapes — `/pd/api/v1/stores`, the `state_name` transition, the
  `store delete` refusal, the absence of a `no-leader` region filter — checked
  against a real PD on the laptop, not against documentation.

### Not verified

**Neither suite has been run since any of this landed.** These changes make
readiness strictly harder to satisfy and add a loop that deletes cluster
metadata; that is exactly the sort of thing that passes review and fails on real
infrastructure. Expect the first run to be slower, too: one-at-a-time
replacement serialises what previously happened in parallel, and
`wait_for_steady_state` now waits for it.

The reconciliation loop *has* now run against a real cluster, which is how the
tombstone bug above was found — but it has not run since that fix. What the
first run proved is that the loop fires, finds the right records and calls the
right endpoint; what it also proved is that "finds the right records" was wrong
in a way no unit test would have caught, because the failure is a race between
two systems' notions of when a task exists.

### If you pick this up next

Run both suites. That is the whole of it — nothing else here is blocked.

After that, in order of value:

- **Run more stores than replicas.** Four against `max-replicas` 3 removes the
  no-headroom case entirely and lets PD release a dead record immediately. It
  costs one task. The e2e stacks stay at three because that is the minimum that
  exercises replication and both clouds have to stay the same shape, but no
  deployment holding data anyone cares about should be at the minimum.
- **A cold-restart rule, enforced rather than documented.** A restart of this
  cluster has to take *all* PD down at once, or PD returns remembering a cluster
  whose data is gone. Nothing prevents the rolling version today.
- **Stable storage where it actually matters.** None of this makes ephemeral
  storage safe for a store; it makes loss survivable, visible and recoverable,
  and shortens the window in which a second replacement turns a survivable loss
  into an unrecoverable one from thirty minutes to about one. A store that must
  survive replacement needs a runtime that preserves both its identity and its
  disk — ECS on EC2 with EBS, EKS with PVCs, or the stateful MIGs the original
  design identified as the only GCP runtime offering a preserved disk *and* a
  preserved name. That boundary is worth stating plainly, because muster's whole
  proposition invites running stateful workloads on runtimes that do not really
  support them.
