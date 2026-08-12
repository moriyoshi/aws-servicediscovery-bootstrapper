# Journal

## 2026-08-12 — Multi-cloud providers (AWS + Google Cloud)

Branch `feat/multicloud-providers`, 15 commits off `4274e43`.
Every commit leaves `go test ./...` green.

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
| KV store | DynamoDB conditional writes | Cloud Storage generation preconditions |
| Identity | ECS task metadata endpoint | Cloud Run env + the metadata server |
| Replica status | ECS `RunningCount == DesiredCount` | *unsupported* — Cloud Run does not expose it |

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
carries one address, not a v4/v6 pair. `SELF.created_at` is empty: the metadata
server does not carry it, and the only source would drag in `computepb` — a
single generated package covering the entire Compute API — for one field
nothing reads.

### Verified

- `gofmt` clean; build + vet under every tag (default, `e2e`, `e2e_tikv`, `gcp`,
  `gcp,gcp_live`).
- Full suite under the default and `gcp` builds, `-race -shuffle=on`.
- Dependency isolation asserted in both directions (see above).
- KV conformance (13 subtests) green against `memKV` and `gcsKV`.
- Every startup error path smoke-tested against the real binaries: absent
  provider, nothing to autodetect, removed flag, cross-cloud autodetect,
  unknown `-provider-opt`.
- Starlark capability messages unchanged where they should be
  ("kv store not configured", "ECS client not configured" → now
  "replica status not configured").

### Not verified — needs infrastructure

- **`-tags=e2e`** (Winterbäume emulator): compiles and vets, not run. Now runs
  the shared conformance suite against the real DynamoDB store.
- **`-tags=e2e_tikv`** (Terraform + Fargate TiKV): compiles and vets, not run.
  **This is the real regression gate for the refactor** — `NoSplitBrain` and
  `SeedLease` are what prove seed election survived. Worth running before merge.
- **`-tags=e2e_tikv_gcp`** (Terraform + Cloud Run worker pools): compiles and
  vets, `terraform validate` passes, never applied. Needs a project.
- **`-tags=gcp,gcp_live`**: skips without `MUSTER_GCP_KV_BUCKET`. The fake is
  single-process and cannot reproduce contention, retries, or the per-object
  write rate.

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

### Deferred

- Stale Service Directory endpoint reaping (an instance killed without teardown
  leaves its entry). Scripts already probe peers before trusting them.
- Azure. The seam accommodates it; nothing else was done.
- Renaming the working directory to `~/Source/muster`. Zero diff — the module
  path and GitHub remote are already `github.com/moriyoshi/muster` — but it
  disrupts open shells and editors, so it was left to be done deliberately.

### If you pick this up next

Run `e2e/tikv/aws` first. It is the regression gate for the whole refactor —
`NoSplitBrain` and `SeedLease` are what prove seed election survived — and a
failure there means the provider work broke something rather than that new
infrastructure code is wrong.

Then `e2e/tikv/gcp`. Expect the ten-second shutdown budget to be the thing that
bites: if `pre_stop` cannot evict a member inside it, the Raft group collects a
dead member per replacement and the symptom is a slow loss of quorum rather than
an obvious failure. `make logs-pd` and the self-reports are where that shows.
