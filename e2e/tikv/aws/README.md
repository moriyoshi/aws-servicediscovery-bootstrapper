# End-to-end test: TiKV on ECS Fargate

This directory stands up a real TiKV cluster — three PD replicas and three
stores, all Fargate tasks — with **muster** as the container entrypoint, then
asserts that the cluster muster assembled is the one it was supposed to.

It is the test that cannot be written against a mock. The interesting part of
muster is what happens when several replicas boot at once, none of them knowing
who they are: exactly one must bootstrap a new Raft group and the rest must join
it. Getting that wrong produces two clusters that each look perfectly healthy
from the inside, which no unit test and no single API call can see.

> **This creates billable AWS resources.** A run is a VPC with interface
> endpoints, six Fargate tasks, an on-demand DynamoDB table and two ECR
> repositories, for roughly 30–60 minutes. The test tears everything down on the
> way out, including after a failure; if it is killed mid-run, `make destroy`
> finishes the job.

**Nothing in the stack is reachable from outside the VPC.** There is no internet
gateway, no NAT and no load balancer: the task subnet's route table carries only
the VPC-local route and two gateway endpoints. The tasks reach AWS over
interface endpoints, and the test driver reaches PD by running `curl` inside the
tasks with `aws ecs execute-command`.

## What it checks

| Subtest | What it proves |
| --- | --- |
| `ServicesReachSteadyState` | Both ECS services rolled out and are running their full task count. |
| `MusterReportsHealthy` | Every task's ECS health status is `HEALTHY` — the health check *is* `muster -health-probe`, so this is muster's own view of its workloads. |
| `CloudMapRegistrations` | Service Connect registered every task, and the set of addresses CloudMap serves matches the set of running ENIs. This is the input to `instances()`. |
| `PDClusterBootstrapped` | PD reports a non-zero cluster id, all replicas are members, there is a leader, and every member name maps to a running task. |
| `NoSplitBrain` | Every PD replica, asked on its own loopback, reports the same cluster id and the same member list. **The seed election check.** |
| `StoresUp` | PD knows exactly the expected stores, all `Up`, at the addresses the tasks actually have. |
| `RegionsReplicated` | Every region has a full peer set and a leader: the Raft groups really formed across the stores, not just registered. |
| `SeedLease` | The DynamoDB lease is either released or held by a running PD task. |
| `PDReplacementRejoins` | Stops a PD task, waits for ECS to replace it, and requires the **same cluster id**, the stopped member gone, membership back to three, and the stores untouched. The replacement took the "join" branch, not the "bootstrap" one, and `pre_stop` cleaned up after itself. |

`PDReplacementRejoins` is skipped under `go test -short`.

## Running it

Prerequisites: `terraform`, `docker` (with BuildKit), Go, the `aws` CLI and the
[`session-manager-plugin`](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html)
it needs for ECS Exec, and credentials for an account you are happy to create a
VPC in. Without the plugin there is no way into the cluster and the test skips.

```bash
cd e2e/tikv/aws
cp config.mk.example config.mk     # optional; every value has a default
make e2e                           # provision, assert, tear down
```

Or drive the phases yourself, which is what you want while iterating:

```bash
make up          # terraform + build and push the images
make test        # assertions only, against the stack that is already up
make logs-pd     # tail PD's CloudWatch log group
make destroy
```

`make up` runs in three phases because of a chicken-and-egg problem: the ECS
services need images, the images need ECR repositories, and the repositories
come from the same Terraform state. So it applies the two repositories first,
pushes, then applies everything else.

That ordering is also where `DOCKER_REGISTRY` comes from: rather than assembling
a hostname out of `aws sts get-caller-identity` and a region, `make images`
reads the `ecr_registry` output back out of the state written by `make
bootstrap`. The images therefore land in exactly the repositories the task
definitions reference — an account or region resolving differently between the
two cannot produce a push that succeeds and a deployment that then fails to pull.
The ECR login region is likewise taken from the registry hostname. Nothing
account-specific is written down anywhere in the tree.

### Configuration

Set these in `config.mk`, or as environment variables:

| Variable | Default | |
| --- | --- | --- |
| `AWS_REGION` | unset | left alone, `AWS_PROFILE` / `~/.aws/config` decide, as usual |
| `DOCKER_REGISTRY` | the `ecr_registry` Terraform output | see below |
| `NAME_PREFIX` | `muster-e2e-tikv-` | prefixes every resource, so two stacks can share an account |
| `TIKV_VERSION` | `v8.5.1` | upstream `pingcap/pd` and `pingcap/tikv` tag |
| `PD_DESIRED_COUNT` / `TIKV_DESIRED_COUNT` | `3` / `3` | |

And these control the test itself:

| Variable | |
| --- | --- |
| `MUSTER_E2E_TIKV=1` | required; without it the test skips |
| `MUSTER_E2E_TIKV_PROVISION=0` | assert against an existing stack instead of provisioning |
| `MUSTER_E2E_TIKV_KEEP=1` | leave the stack up afterwards, for a post-mortem |

## How it is wired

```
   test driver (your machine)
        │
        │  aws ecs execute-command ──> SSM ──> ssmmessages VPC endpoint
        │                                              │
  ─ ─ ─ ┼ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─│─ ─ ─ ─ ─ ─ ─ ─ ─
        │   internal subnet — no IGW, no NAT, no LB    │
        │                                     ┌────────▼──────────────┐
        └── ECS / CloudMap / DynamoDB APIs ──>│ tikv-pd ×3  (Fargate) │──┐
                    (VPC endpoints)           │ muster → /pd-server   │  │
                                              └───────────────────────┘  │ CloudMap
                                              ┌───────────────────────┐  │ (Service
                                              │ tikv-node ×3          │──┘  Connect)
                                              │ muster → /tikv-server │
                                              └───────────────────────┘
```

The task images are the upstream `pingcap/pd` and `pingcap/tikv` images with the
muster binary — built from *this working tree*, not a release — and a Starlark
script copied in. The ECS task definition makes muster the entrypoint and passes
the real command after `--`.

### Talking to PD without an endpoint

`pdClient` (in `harness_test.go`) runs `curl` inside a chosen task via
`aws ecs execute-command`. That is not only a workaround for having no ingress —
it addresses *one named replica* rather than whichever one a load balancer
happened to pick, which is what makes `NoSplitBrain` exhaustive instead of
statistical.

The awkward part is getting bytes back out. The session returns a transcript,
not a response: SSM's banner, the command's stdout and stderr, and the plugin's
closing line, multiplexed over a pty that rewrites newlines — and
`aws ecs execute-command` exits `0` no matter what the remote command did. So
the remote command fences its output with markers and echoes `$?` in band, and
`parseExecOutput` in `execout.go` pulls the payload back out. That parser lives
outside the build tag and is covered by ordinary `go test ./...`.

### `docker/tikv-pd/pd.star`

The cold-start decision, made fresh on every respawn:

```
(A) our data directory already holds a member  -> restart in place
(B) some peer is already answering             -> --join that peer
(C) nobody is up and we win the DynamoDB lease -> bootstrap the cluster
(D) nobody is up and someone else won          -> wait for them, then join
```

(C) and (D) are the race. `kv_put_if_absent()` is a conditional write, so
exactly one replica can win it; the losers block on `poll()` until the key
appears, wait for the winner to serve, and join. The lease has a TTL, so a
winner that dies before it bootstraps releases it.

`pre_stop` removes this member from the Raft group on the way out. Fargate
storage is ephemeral, so a replaced task comes back at a new address under a new
member name — without the removal the member list would grow a dead entry per
replacement and eventually lose quorum. `PDReplacementRejoins` is what checks
this actually happens.

Two deliberate choices worth knowing about:

- The waiting happens inside `resolve()`, not `pre_start()`. `spawn()` resolves
  argv *before* it calls `pre_start`, and every branch above depends on having
  seen the peers.
- `restart_on_liveness = False`. A PD that stops answering is reported unhealthy
  and left for ECS to replace. Restarting it in place would run `pre_stop` first
  — evicting it from the Raft group it is about to rejoin under the same name.

### `docker/tikv-node/tikv.star`

Simpler: wait until all PD replicas are discoverable and one of them answers,
then start `tikv-server` pointed at the full endpoint list. Stores have no
election to run — whichever gets there first bootstraps the first region, and PD
arbitrates that. A store keeps its data directory and has no membership to be
evicted from, so unlike PD it does restart in place on a liveness loss.

## Reading a failure

The test streams `terraform` and `docker` output into the Go test log, and each
`eventually` logs what it is still waiting for.

**On failure it dumps both task log groups into the test output before tearing
down.** Teardown deletes the log groups along with everything else, so without
that a failed run leaves nothing to read and the only way to find out what the
tasks were doing is to spend another twenty minutes and another cluster
reproducing it. A failing subtest also aborts immediately once ECS reports the
deployment circuit breaker has tripped, rather than waiting out its timeout
against a cluster that is already dead.

To dig further:

```bash
make logs-pd            # or logs-tikv
MUSTER_E2E_TIKV_KEEP=1 make e2e     # keep the stack up to poke at it
aws ecs execute-command --cluster muster-e2e-tikv-main \
    --task <task-id> --container default --interactive --command /bin/sh
```

Failure modes that are the environment rather than the code:

- **`TargetNotConnectedException`.** The SSM agent in a task takes a few seconds
  to connect after the task starts. `pdClient.exec` retries three times for
  exactly this; if it persists, check the `ssmmessages` VPC endpoint and that
  the task role has the four `ssmmessages:*` actions.
- **`SessionManagerPlugin is not found`.** Install the session-manager-plugin;
  the AWS CLI cannot open an exec session without it.
- **`terraform destroy` fails on the CloudMap namespace.** ECS deletes the
  Service Connect registrations asynchronously; run `make destroy` again.
- **Image pull failures.** The tasks reach ECR over VPC endpoints only. If you
  changed the subnet or security-group wiring, check `ecr.api`, `ecr.dkr` and
  the S3 gateway endpoint.
- **`make up` cannot find the images.** `make bootstrap` must have created the
  ECR repositories and `make images` must have pushed before `make apply`;
  `make up` does all three in order.

## What is not covered

No data-plane reads or writes: that would mean pulling a TiKV client into the
module and publishing the store ports, and `RegionsReplicated` already shows the
Raft groups formed. Nor is there a test for stopping a *store* — PD marks a
missing store `Down` and only reschedules its regions after 30 minutes, which is
longer than a test should run.
