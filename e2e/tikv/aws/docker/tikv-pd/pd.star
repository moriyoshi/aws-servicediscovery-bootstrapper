# muster script for the PD (Placement Driver) tier of a TiKV cluster running on
# ECS Fargate.
#
# A PD replica starts up not knowing whether it is the first member of a brand
# new cluster, a replacement for one that died, or a member coming back after a
# restart. It decides between those three every time it (re)spawns:
#
#   (A) our data directory already holds a member  -> restart in place
#   (B) some peer is already answering             -> --join that peer
#   (C) nobody is up and we win the kv lease        -> bootstrap the cluster
#   (D) nobody is up and someone else won          -> wait for them, then join
#
# (C)/(D) are the cold-start race: all replicas come up at once and exactly one
# must bootstrap, or the cluster splits in two. kv_put_if_absent() is the
# arbiter — it is a conditional write, so exactly one caller can win.

# The fallback for me(), and optional now that SELF.ipv4 is the primary source.
CIDR = env("MUSTER_SUBNET_CIDR")
PD_SERVICE = env("MUSTER_PD_SERVICE", "tikv-pd")

# Our own replica-set coordinates, for all_replicas_running(). Left unset these
# are None and the builtin falls back to SELF, which muster reads once at startup
# from the platform's metadata, best-effort, with no retry. When that comes up
# empty the builtin can only raise — and the raise escapes resolve(), so the
# workload never starts at all. The deployment knows both names for certain, so
# it says so.
SELF_GROUP = env("MUSTER_SELF_GROUP")
SELF_SERVICE = env("MUSTER_SELF_SERVICE")

DATA_DIR = env("MUSTER_DATA_DIR", "/pd")
SEED_KEY = "tikv-pd/seed"
CLIENT_PORT = 2379
PEER_PORT = 2380

# The lease is only held across a cold start. It is released in pre_stop, and
# expires on its own if the winner dies before it can bootstrap.
SEED_TTL = "180s"

# Readiness and liveness probe the member list, not /health. It is served out of
# etcd the moment this PD joins the quorum, and does not depend on the TiKV
# cluster having been bootstrapped — which cannot happen until a store registers,
# and no store exists until this service is already up. The cold-start sequence
# below already relies on that being true: live_peers() and await_seed() use the
# same endpoint to decide a peer is serving.
LOCAL_MEMBERS = "http://127.0.0.1:%d/pd/api/v1/members" % CLIENT_PORT


def me():
    """This task's own address on the VPC.

    SELF.ipv4 is what ECS put in the task metadata, and is the same source the
    Cloud Run script uses -- so the two agree on the common path.

    ifaddr() stands behind it rather than replacing it because muster reads that
    metadata once at startup, best-effort and without retry, and a task that
    cannot name its own address cannot be a PD member at all. The Cloud Run
    script has no such fallback and fails outright, because an empty SELF.ipv4
    means something different there: the workload was deployed on a service or a
    job instead of a worker pool, which no fallback should paper over.
    """
    if SELF and SELF.ipv4:
        return SELF.ipv4
    if CIDR:
        return ifaddr(CIDR)
    fail("pd: this task's own address is unknown -- the task metadata endpoint " +
         "told muster nothing and MUSTER_SUBNET_CIDR is unset, so there is no " +
         "address to advertise to peers")


def client_url(ip):
    return "http://%s:%d" % (ip, CLIENT_PORT)


def peer_url(ip):
    return "http://%s:%d" % (ip, PEER_PORT)


def member_name(ip):
    # etcd member names must be stable for the lifetime of a member; the task's
    # address is the only identity a Fargate task has that PD can also see.
    return "pd-" + ip.replace(".", "-")


def other_pds(ip):
    # health_status="ALL": a peer that is up but has not yet passed its ECS
    # health check is still worth joining, and we probe it ourselves below.
    return [i.ipv4 for i in instances(PD_SERVICE, health_status = "ALL") if i.ipv4 and i.ipv4 != ip]


def live_peers(peers):
    """The subset of peers whose PD API answers, probed concurrently."""
    probes = [go(http_ok(client_url(p) + "/pd/api/v1/members", "2s")) for p in peers]

    # All the probes are already in flight, so joining inside the filter awaits
    # them in order rather than serializing the work. Pairing each peer with its
    # promise, rather than with join(*probes), keeps the two cases that actually
    # matter here working: no peers at all (a cold start, where join() would
    # raise) and exactly one (join returns a bare bool, not a list).
    return [p for p, probe in zip(peers, probes) if join(probe)]


def all_pd_replicas_running():
    return all_replicas_running(group = SELF_GROUP, service = SELF_SERVICE)


def lineup():
    """Best-effort wait for every task in our service to be running.

    Purely advisory: it shaves churn off a cold start by letting the replicas
    reach the seed election together. Nothing in here may raise. Starlark has no
    way to catch an error, so a raise escapes resolve(), aborts the attempt, and
    leaves the workload permanently unstarted — a far worse outcome than simply
    not waiting.
    """
    if not SELF_GROUP or not SELF_SERVICE:
        log("pd: own group/service unknown; not waiting for the line-up")
        return

    # select() waits for the poll to settle and hands back the promise itself,
    # so unlike join() it does not re-raise a rejection. all_replicas_running()
    # calls the provider's describe-service API, which can fail for reasons that
    # have nothing to do with whether the replicas are up: throttling, a missing
    # permission, an IAM condition key that does not match. None of those should
    # stop PD starting.
    select(poll(all_pd_replicas_running, "120s", interval = "5s"))


def drop_member(peers, name):
    """Remove `name` from the cluster via the first peer that accepts it."""
    for p in peers:
        r = http_request("DELETE", client_url(p) + "/pd/api/v1/members/name/" + name, timeout = "5s")
        if r.status < 300:
            log("pd: dropped member", name = name, via = p)
            return True
        if r.status == 404:
            return True  # not in the cluster, which is the state we wanted
        log("pd: peer rejected the member removal", name = name, via = p, status = str(r.status))
    return False


def await_seed():
    """Block until the seed lease is taken, and its holder is serving."""
    if not join(poll(lambda: kv_get(SEED_KEY) != None, "180s", interval = "2s")):
        fail("pd: no seed was elected within 180s")
    seed = kv_get(SEED_KEY)
    if not join(poll(http_ok(client_url(seed) + "/pd/api/v1/members", "2s"), "300s", interval = "2s")):
        fail("pd: elected seed %s never started serving" % seed)
    return seed


def resolve_pd():
    # Line the replicas up before the election, so a cold start really is a
    # race between all of them rather than a walkover for whichever task ECS
    # placed first. Advisory: one slow replica must not block the rest forever.
    #
    # This lives in resolve() rather than pre_start() because spawn() resolves
    # argv first, and the decisions below depend on seeing the peers.
    lineup()

    ip = me()
    name = member_name(ip)
    argv = COMMAND + [
        "--name",
        name,
        "--data-dir",
        DATA_DIR,
        "--client-urls",
        "http://0.0.0.0:%d" % CLIENT_PORT,
        "--advertise-client-urls",
        client_url(ip),
        "--peer-urls",
        "http://0.0.0.0:%d" % PEER_PORT,
        "--advertise-peer-urls",
        peer_url(ip),
    ]

    # (A) a respawn after a crash: PD picks its role up from the data directory.
    if path_exists(DATA_DIR + "/member"):
        log("pd: restarting an existing member", name = name)
        return argv

    # (B) the cluster already exists.
    live = live_peers(other_pds(ip))
    if live:
        # PD registers a joining member with the cluster *before* it writes its
        # data directory. An attempt that died in between leaves a member
        # carrying our name with nothing behind it, and every later join then
        # fails permanently — PD refuses to join a name the cluster already
        # knows when the local data dir is empty, and no amount of respawning
        # gets past it. Our name comes from this task's address, so a member
        # holding it while we have no data dir is exactly that leftover.
        drop_member(live, name)

        log("pd: joining the running cluster", name = name, via = ",".join(live))
        return argv + ["--join", ",".join([client_url(p) for p in live])]

    # (C) cold start, and we are the one that gets to bootstrap.
    if kv_put_if_absent(SEED_KEY, ip, SEED_TTL):
        log("pd: won the seed lease, bootstrapping a new cluster", name = name)
        return argv

    # (D) cold start, someone else won: wait for them and join.
    seed = await_seed()
    if seed == ip:
        # A previous attempt of ours won the lease and then failed; the lease is
        # still ours, so bootstrapping is still the right move.
        log("pd: reclaimed our own seed lease, bootstrapping", name = name)
        return argv
    log("pd: following the elected seed", name = name, seed = seed)
    return argv + ["--join", client_url(seed)]


def pd_readiness():
    # Generous on purpose. Readiness feeds the container health probe, and it is
    # ECS — not spawn() — that should replace a PD which never comes up: a local
    # restart would run pre_stop, and pre_stop removes this member from the Raft
    # group, which an in-place restart could not recover from.
    return poll(http_ok(LOCAL_MEMBERS, "3s"), "30m", interval = "3s")


def pd_down():
    """Truthy only once PD has been unreachable for a solid minute.

    Settling here condemns the task. With restart_on_liveness = False the
    workload is marked unhealthy and nothing ever marks it back, so ECS
    replacing the task is the only way out — far too heavy a response to one
    slow reply during a leader election or a burst of store registrations.
    """
    probe = http_ok(LOCAL_MEMBERS, "5s")

    def check():
        for _ in range(6):
            if probe():
                return False
            sleep("10s")
        return True

    return check


def pd_liveness():
    return poll(pd_down(), "24h", interval = "10s")


def pd_pre_stop():
    """Teardown. Runs while the process is still alive, so peers are reachable."""
    ip = me()

    # Release the lease first: it must not outlive us, and the member removal
    # below can fail when no peer is reachable.
    kv_delete(SEED_KEY, if_value = ip)

    # Fargate storage is ephemeral, so our replacement comes back with a new
    # address and therefore a new member name. Drop this member while peers are
    # still reachable, or the Raft group accumulates a dead member per
    # replacement and eventually loses quorum.
    name = member_name(ip)
    if not drop_member(live_peers(other_pds(ip)), name):
        log("pd: no peer accepted the member removal", name = name)


def main():
    return spawn(
        name = "pd",
        resolve = resolve_pd,
        pre_stop = pd_pre_stop,
        readiness = pd_readiness,
        liveness = pd_liveness,
        respawn = True,
        # Bounded, not unlimited. Retrying forever turns a resolve() that can
        # never succeed — a denied API call, a peer set that never appears —
        # into a container that sits up as PID 1 doing nothing, which reads as a
        # hang: ECS only notices via the health check once the grace period
        # expires, and the deployment stalls without ever saying why. Giving up
        # exits non-zero instead, so the task stops, ECS replaces it, and a
        # persistent fault trips the deployment circuit breaker promptly.
        # reset_after (30s) clears the counter once an attempt actually stays
        # up, so this only fires on genuinely consecutive failures.
        max_retries = 5,
        # A PD that goes unresponsive is reported unhealthy and left for ECS to
        # replace, rather than restarted here. On Fargate the orchestrator is
        # the thing that can actually give it a clean slate, and restarting in
        # place would first run pre_stop and evict us from our own Raft group.
        restart_on_liveness = False,
        pre_stop_timeout = "20s",
        shutdown_grace = "30s",
    )
