# End-to-end test: TiKV on Cloud Run worker pools

This directory stands up a real TiKV cluster — three PD replicas and three
stores, all Cloud Run worker pool instances — with **muster** as the container
entrypoint, then asserts that the cluster muster assembled is the one it was
supposed to.

It is the Google Cloud counterpart of [`../aws`](../aws), and exists for the
same reason: the interesting part of muster is what happens when several
replicas boot at once, none of them knowing who they are, and getting that wrong
produces two clusters that each look perfectly healthy from the inside.

> **This creates billable Google Cloud resources.** A run is a VPC, two worker
> pools totalling six instances with 10 GiB ephemeral disks, a Cloud Storage
> bucket, an Artifact Registry repository and a Service Directory namespace, for
> roughly 30–60 minutes. Worker pool instances bill while they run, so the stack
> is meant to be torn down; the test does that on the way out, including after a
> failure, and `make destroy` finishes the job if it is killed mid-run.

## Why worker pools, and what that constrains

A **worker pool** is the one Cloud Run runtime that supports
[Direct VPC *ingress*](https://docs.cloud.google.com/run/docs/configuring/vpc-direct-vpc):
each instance gets a private address on your VPC, and other resources — other
instances included — can connect to it there. Services and jobs get Direct VPC
egress only, so their instances cannot be dialled and cannot form a Raft group.
`SELF.ipv4` is populated on the one and empty on the others, and both scripts
fail loudly on an empty value rather than starting a member nothing can reach.

Beyond addressing, a worker pool is very close to Fargate, which is why these
scripts are near-copies of the ECS ones rather than a rethink:

| | ECS Fargate | Cloud Run worker pool |
| --- | --- | --- |
| storage | ephemeral, lost with the task | ephemeral disk, *"permanently deleted when the instance shuts down"* |
| identity across replacement | none | none — `SELF.name` is empty on both |
| address across replacement | not preserved | not preserved |
| PD member name | derived from the address | derived from the address |
| `pre_stop` must evict the member | yes | yes |
| registration | ECS Service Connect does it | **nothing does it** — `register()` from `post_start` |
| shutdown budget | 30s, up to 120s | **10s, fixed** |
| file descriptors | `ulimit nofile = 123880` in the task definition | **capped at 25000**, and it is the hard limit |

The **ephemeral disk needs `medium = "DISK"`**. Cloud Run's default is `MEMORY`,
which is charged against the instance's memory limit — PD's data directory would
eat the RAM it needs to run, and 10 GiB of it would not fit at all.

The **ten-second shutdown budget** is the one genuinely hard difference. ECS's
`stopTimeout` is configurable; Cloud Run sends `SIGTERM` and `SIGKILL`s ten
seconds later, full stop. `pd.star`'s `pre_stop` has to release the seed lease
*and* evict this member from the Raft group inside it — both single round trips,
so it fits, but `pre_stop_timeout` is 5s and `shutdown_grace` 8s rather than the
ECS script's 20s and 30s. Miss that window and the Raft group accumulates a dead
member per replacement until it loses quorum.

The **file-descriptor cap is why the stores carry a config file.** TiKV refuses
to start unless `RLIMIT_NOFILE` is at least `1000 + rocksdb.max-open-files +
raftdb.max-open-files`, counting the rocksdb term twice while Titan is on --
1000 + 3 × 40960 = 123880 at the defaults. The ECS stack simply asks Fargate for
that many. Cloud Run's 25000 is a *hard* limit, so `setrlimit(2)` from inside the
container cannot raise it (that needs `CAP_SYS_RESOURCE`) and no worker pool
setting exposes it. `docker/tikv-node/tikv.toml` brings the demand down to
13288 instead; `max-open-files` caps RocksDB's table cache, so the cost of the
lower value is latency on a working set this test will never have.

## How the cluster is observed

Nothing in this stack is reachable from outside the VPC, and unlike the ECS
suite there is no `ecs execute-command` to borrow — nor a bastion, which would
mean a Compute Engine VM this project deliberately does not use.

So each PD replica **reports its own view on a loop** — cluster id, member list,
store list — and the test reads those back out of Cloud Logging. That is not a
workaround for a missing route: asking one replica through a load balancer could
never see a split brain, and asking *every* replica about *itself* is exactly
what the ECS suite achieves by shelling into each task.

## What it checks

| Subtest | What it proves |
| --- | --- |
| `PoolsReachReady` | Both pools reconciled onto a ready revision. A worker pool serves no requests, so this is the only signal the instances started. |
| `ServiceDirectoryRegistrations` | Every instance registered itself. **Every one of these endpoints came from muster's `register()`** — nothing on Cloud Run does it — so unlike the CloudMap check on the AWS side, this tests muster's code rather than the platform's. |
| `PDClusterBootstrapped` | Every replica that has reported sees a non-zero cluster id. |
| `NoSplitBrain` | Every PD replica, reporting from its own loopback, names the same cluster id. **The seed election check.** |
| `QuorumComplete` | Every replica sees the full membership — not just one of them. A member that formed its own view is a split brain one level down. |
| `StoresUp` | PD knows exactly the expected stores, all `Up`. |
| `SeedLease` | The lease is released, or held by a registered replica. |
| `PDReplacementRejoins` | Replaces **one** PD instance and requires the cluster id — and the membership count — to survive. Skipped under `go test -short`. |

`PDReplacementRejoins` replaces one instance of three, which is the same claim
the ECS version makes by stopping one task — and it took a failed run to arrive
at. Rolling the revision outright, which is what it did first, asserts something
Cloud Run cannot do: a revision update replaces *every* instance of a worker
pool, and with three ephemeral disks discarded together nothing is left to carry
the cluster. The seed lease carries an address, not cluster state, and PD cannot
be told to adopt an id, so a fresh tier correctly bootstraps a fresh cluster.

Hence `--no-promote` followed by `update-instance-split --to-revisions=<new>=34`:
the new revision takes one instance of three, two members survive, quorum holds,
and the replacement has something to join. A mismatched cluster id fails
immediately rather than waiting out the timeout — that is the one failure this
subtest exists to catch, and the first version reported it as "2 of 3 replicas
have reported".

## Running it

Prerequisites: `terraform`, `docker` (with BuildKit), Go, and `gcloud`
authenticated against a project you are happy to create a VPC in. The project
needs the `run`, `compute`, `storage`, `servicedirectory` and `artifactregistry`
APIs enabled. Worker pools are a beta surface, hence `gcloud beta` and
`launch_stage = "BETA"`.

```bash
cd e2e/tikv/gcp
cp config.mk.example config.mk     # set GCP_PROJECT, or rely on gcloud's current project
make e2e                           # provision, assert, tear down
```

Or drive the phases yourself, which is what you want while iterating:

```bash
make up          # terraform + build and push the images
make test        # assertions only, against the stack that is already up
make logs-pd     # PD's muster output, including its self-reports
make destroy
```

### Choosing the kv backend

`KV_BACKEND=firestore make up` runs the same cluster with the seed election on
Firestore instead of Cloud Storage. It additionally creates a named Firestore
database, which is slower to stand up and slower to tear down than a bucket --
which is why `gcs` is the default and why the database is created only when
selected.

It is the only way to exercise the Firestore backend against Google's own
servers. Short of that, the conformance suite runs against the emulator, which
needs no JRE on the host if you let Docker supply one:

```bash
docker run -d --name muster-fs-emu -p 8484:8484 \
  gcr.io/google.com/cloudsdktool/google-cloud-cli:emulators \
  gcloud emulators firestore start --host-port=0.0.0.0:8484
FIRESTORE_EMULATOR_HOST=127.0.0.1:8484 go test -tags=gcp -race ./internal/provider/gcp/
```

`make up` runs in three phases on purpose: the pools name images that do not
exist yet, and the images cannot be pushed until the Artifact Registry
repository does. `bootstrap` creates just the repository, `images` builds and
pushes, `apply` does the rest.

## Debugging

`MUSTER_E2E_TIKV_GCP_KEEP=1` leaves the stack up after a failure, and the
harness dumps both pools' logs before teardown removes them — **muster's own
decisions first**, in full, then a bounded tail of the workload's output. A flat
tail is useless here: PD writes thousands of lines an hour, so the last few
hundred cover about a minute of raft chatter and reach nothing muster did.

The self-reports are the first place to look. Each is one line:

```
{"level":"INFO","msg":"pd: CLUSTER","who":"10.128.253.4","body":"{\"id\":7449...}"}
{"level":"INFO","msg":"pd: MEMBERS","who":"10.128.253.4","body":"{\"members\":[...]}"}
```

If replicas report *different* cluster ids, that is a split brain and the seed
election is broken. If they report nothing, PD never came up — read the lines
above them.

If discovery finds no peers, check the firewall before anything else: instances
get addresses they cannot use on each other without the intra-subnet rule, and
the symptom reads like a discovery failure rather than a network one.
