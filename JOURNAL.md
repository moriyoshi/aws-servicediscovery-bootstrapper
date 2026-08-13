# Journal

## 2026-08-12 — Multi-cloud providers (AWS + Google Cloud)

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

### The suite runs, and the last subtest asserted the impossible

Seven of eight subtests pass against a real project: `PoolsReachReady`,
`ServiceDirectoryRegistrations`, `PDClusterBootstrapped`, `NoSplitBrain`,
`QuorumComplete`, `StoresUp`, `SeedLease`. Everything the provider exists to do,
end to end, on Cloud Run worker pools.

`PDReplacementRejoins` failed, and it was right to. It rolled the PD revision
and required the cluster id to survive — but **a Cloud Run revision update
replaces every instance of a worker pool**, and with three ephemeral disks
discarded together there is nothing left to carry the cluster. The seed lease
carries an address, not cluster state, and PD cannot be told to adopt an id. So
the new tier bootstrapped a new cluster, correctly: id ...4512 before, ...0939
after. The test asserted a property the platform cannot provide, and the README
called it "the strongest statement this platform allows" while it was in fact a
statement about a different platform — Fargate, where the ECS suite stops *one*
task.

The fix is to make the same claim the ECS suite makes: `--no-promote` to deploy
the revision with no instances, then `update-instance-split --to-revisions=
<new>=34` to move one instance of three onto it. Two members survive, quorum
holds, and the replacement has a cluster to join. It also now asserts membership
returns to three, because agreeing about which cluster you are in is not the
same as being in it.

**How it failed is the more useful half.** The mismatched cluster id was visible
in the very first poll, and the check said `2 of 3 replacements have reported`
for twenty minutes instead, because it counted replicas before comparing what
they said. A test whose single purpose is catching a split brain reported a
split brain as a timeout. The comparison now runs first and is terminal.

And the failure dump could not have explained any of it: 300 lines of a pool
where PD writes thousands an hour is about ninety seconds of raft chatter, with
none of muster's decisions in it. Dumps now print every line muster wrote —
which branch each replica took, what it registered, what it respawned — and then
a bounded tail of the workload's own output. Everything muster says across an
hour fits in a fraction of what the workload says in a minute.

### Verified

- `gofmt` clean; build + vet under every tag (default, `e2e`, `e2e_tikv`,
  `e2e_tikv_gcp`, `gcp`, `gcp,gcp_live`).
- Full suite under the default and `gcp` builds, `-race -shuffle=on`.
- Dependency isolation asserted in both directions (see above).
- KV conformance (13 subtests) green against `memKV`, `gcsKV` and — via the
  emulator in Docker — `firestoreKV`.
- Every startup error path smoke-tested against the real binaries: absent
  provider, nothing to autodetect, removed flag, cross-cloud autodetect,
  unknown `-provider-opt`.
- Starlark capability messages unchanged where they should be
  ("kv store not configured", "ECS client not configured" → now
  "replica status not configured").

### What the real Cloud Run runs proved, and what they did not

The Google Cloud stack **has been applied**, repeatedly, against a real project.
That settles the most important claim in this document, and most of the others.

**A complete TiKV cluster now runs on Cloud Run worker pools, assembled by
muster.** Read off the live stack rather than inferred:

| Assertion | Live state |
| --- | --- |
| `PoolsReachReady` | both pools `Ready=True`, reconciled onto their created revision |
| `ServiceDirectoryRegistrations` | 6 endpoints, 3 per tier — **every one written by `register()`**, since nothing on Cloud Run registers an instance |
| `PDClusterBootstrapped` | all three replicas report a non-zero cluster id |
| `NoSplitBrain` | all three report the *same* id, `7673139720976975013` |
| `QuorumComplete` | each sees 3 members, not just itself |
| `StoresUp` | 3 stores, `['Up', 'Up', 'Up']`, from each replica independently |
| `SeedLease` | held by `10.128.253.18`, which is a registered replica |
| `PDReplacementRejoins` | **failed** — see below; it asserted something Cloud Run cannot do |

Seed election is the part the whole provider exists for, and on Cloud Run it is
no longer theoretical: from a cold start one replica took the lease and
bootstrapped, two followed it, and every replica agrees on one cluster.

Two of this session's fixes are confirmed in production by that table rather
than by argument — the store tier is up, so the file-descriptor config works;
and the numbers above were *read from the self-reports*, so the reporting loop
survives its own failing first samples.

**Seven of the eight subtests now pass in one run.** The eighth, and the six
failures that preceded it, were never the election — registry authentication, a
configuration variable nothing read, the reporting loop's silent death, TiKV's
file-descriptor demand, a readiness check reading the wrong API's field names,
three stacked defects in the log reads, and finally an assertion that the
platform cannot satisfy.

Still not verified, and needing infrastructure:

- **`-tags=e2e_tikv`** (Terraform + Fargate TiKV): compiles and vets, not run.
  **This is the real regression gate for the refactor** — `NoSplitBrain` and
  `SeedLease` are what prove seed election survived. Worth running before merge,
  and now doubly so, since the AWS scripts changed after it last passed.
- **`-tags=e2e`** (Winterbäume emulator): compiles and vets, not run. Now runs
  the shared conformance suite against the real DynamoDB store.
- **`-tags=gcp,gcp_live`**: skips without `MUSTER_GCP_KV_BUCKET`. The fake is
  single-process and cannot reproduce contention, retries, or the per-object
  write rate. The same gap applies to Firestore, where the emulator stands in
  for the server whose transaction semantics are the thing under test.
- **`PDReplacementRejoins`** and the **Firestore backend against a real
  database** (`KV_BACKEND=firestore`) are the two paths on Google Cloud that no
  run has touched.

### Deferred

- Stale Service Directory endpoint reaping (an instance killed without teardown
  leaves its entry). Scripts already probe peers before trusting them.
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

Run `e2e/tikv/aws` first. It is the regression gate for the whole refactor —
`NoSplitBrain` and `SeedLease` are what prove seed election survived — and a
failure there means the provider work broke something rather than that new
infrastructure code is wrong. It also now carries a change of its own: `me()`
reads `SELF.ipv4` rather than `ifaddr()`, which has not been near Fargate.

Then `e2e/tikv/gcp`, which now needs one clean pass rather than another repair:
the cluster assembles, and every assertion but `PDReplacementRejoins` has been
confirmed against a live stack by hand. That one is the next real unknown, and
the ten-second shutdown budget is what should bite — if `pre_stop` cannot
release the lease and evict the member inside it, the Raft group collects a dead
member per replacement and the symptom is a slow loss of quorum rather than an
obvious failure. `make logs-pd` and `make logs-tikv` are where it shows.

The pattern across every failure so far is worth carrying forward: not one was
in the election logic. All five were in the space between muster and the
platform — a registry credential, a variable name, a background task nobody
joined, a resource limit, and a field name. Four of the five were *silent*: they
produced an empty value, an unread variable, a discarded error, a log with
nothing in it. That space, and that failure mode, is where the remaining risk
is.
