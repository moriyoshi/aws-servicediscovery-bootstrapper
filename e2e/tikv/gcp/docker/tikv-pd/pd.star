# muster script for the PD (Placement Driver) tier of a TiKV cluster running on
# a Cloud Run worker pool.
#
# The decision tree is the ECS script's, and for the same reason: a PD replica
# starts up not knowing whether it is the first member of a brand new cluster, a
# replacement for one that died, or a member coming back after a restart.
#
#   (A) our data directory already holds a member  -> restart in place
#   (B) some peer is already answering             -> --join that peer
#   (C) nobody is up and we win the kv lease       -> bootstrap the cluster
#   (D) nobody is up and someone else won          -> wait for them, then join
#
# (C)/(D) are the cold-start race: all replicas come up at once and exactly one
# must bootstrap, or the cluster splits in two. kv_put_if_absent() is the
# arbiter -- a conditional write, so exactly one caller can win.
#
# A Cloud Run worker pool is very close to Fargate here, which is why this is a
# near-copy of the ECS script rather than a rethink:
#
#   * instances are addressable, but only on a worker pool -- services and jobs
#     get Direct VPC egress without ingress. SELF.ipv4 is populated on the one
#     and empty on the others, so an empty value means this is deployed wrong.
#   * the disk is ephemeral and the address is not preserved, so there is no
#     identity that survives replacement and the member name has to come from
#     the address, exactly as on Fargate.
#   * nothing registers an instance, unlike ECS Service Connect, so each one
#     announces itself. That part has no ECS counterpart.
#
# The one hard difference is the shutdown budget. Fargate allows 30s (up to
# 120s); Cloud Run sends SIGTERM and SIGKILLs 10 seconds later, and that is not
# tunable. pre_stop has to evict this member inside it -- see pd_pre_stop.

PD_SERVICE = env("MUSTER_PD_SERVICE", "tikv-pd")
PD_REPLICAS = int(env("MUSTER_PD_REPLICAS", "3"))

DATA_DIR = env("MUSTER_DATA_DIR", "/pd")
SEED_KEY = "tikv-pd/seed"
CLIENT_PORT = 2379
PEER_PORT = 2380

# The lease is only held across a cold start. It is released in pre_stop, and
# expires on its own if the winner dies before it can bootstrap.
SEED_TTL = "180s"

LOCAL_MEMBERS = "http://127.0.0.1:%d/pd/api/v1/members" % CLIENT_PORT


def me():
    """This instance's address on the VPC.

    Populated only on a worker pool: a Cloud Run service or job gets Direct VPC
    egress but not ingress, so the address it sends from is not one a peer could
    ever connect to, and muster leaves SELF.ipv4 empty there. A PD that cannot
    be reached is not a cluster member, so fail rather than proceed.
    """
    if not SELF.ipv4:
        fail("pd: SELF.ipv4 is empty; PD needs an address peers can reach, which " +
             "means a Cloud Run worker pool -- a service or a job cannot host this")
    return SELF.ipv4


def client_url(ip):
    return "http://%s:%d" % (ip, CLIENT_PORT)


def peer_url(ip):
    return "http://%s:%d" % (ip, PEER_PORT)


def member_name(ip):
    # etcd member names must be stable for the lifetime of a member, and a
    # Cloud Run instance has no identity that outlives it -- SELF.name is empty
    # here, as on Fargate. The address is the only thing PD can also see.
    return "pd-" + ip.replace(".", "-")


def other_pds(ip):
    # health_status="ALL" because Service Directory endpoints carry no health
    # status, and muster raises rather than quietly widening a HEALTHY request.
    # live_peers() probes them itself, which is the portable habit anyway.
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


def lineup():
    """Best-effort wait for the whole pool to have registered.

    The ECS script asks the scheduler with all_replicas_running(). Cloud Run
    does not expose per-instance counts -- the autoscaler owns that, and the
    only source is a Cloud Monitoring metric delayed by minutes -- so muster
    reports the capability unsupported and the builtin raises. Counting what
    discovery can see is the portable substitute, and it is arguably the better
    signal here: it says the peers are registered, not merely scheduled.

    Purely advisory. Nothing in here may raise: Starlark cannot catch an error,
    so a raise escapes resolve(), aborts the attempt, and leaves the workload
    permanently unstarted -- far worse than not waiting.
    """
    def enough():
        return len(instances(PD_SERVICE, health_status = "ALL")) >= PD_REPLICAS

    select(poll(enough, "120s", interval = "5s"))


def drop_member(peers, name):
    """Remove `name` from the cluster via the first peer that accepts it."""
    for p in peers:
        r = http_request("DELETE", client_url(p) + "/pd/api/v1/members/name/" + name, timeout = "3s")
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
    # Line the replicas up before the election, so a cold start really is a race
    # between all of them rather than a walkover for whichever instance Cloud
    # Run started first. Advisory: one slow replica must not block the rest.
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

    # (A) a respawn after a crash. The ephemeral disk outlives the process but
    # not the instance, so this is reached when muster restarts the workload in
    # place -- never after Cloud Run replaces the instance.
    if path_exists(DATA_DIR + "/member"):
        log("pd: restarting an existing member", name = name)
        return argv

    # (B) the cluster already exists.
    live = live_peers(other_pds(ip))
    if live:
        # PD registers a joining member with the cluster *before* it writes its
        # data directory. An attempt that died in between leaves a member
        # carrying our name with nothing behind it, and every later join then
        # fails permanently -- PD refuses to join a name the cluster already
        # knows when the local data dir is empty. Our name comes from this
        # instance's address, so a member holding it while we have no data dir
        # is exactly that leftover.
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


def pd_registered():
    """Publish this instance so the peers and the stores can find it.

    Nothing on Cloud Run does this. ECS Service Connect registers tasks into
    CloudMap, which is why the ECS script has no equivalent of this function;
    Service Directory auto-registration covers GKE Services only.
    """
    register(PD_SERVICE, port = CLIENT_PORT)


REPORTS = [
    ("CLUSTER", "/pd/api/v1/cluster"),
    ("MEMBERS", "/pd/api/v1/members"),
    ("STORES", "/pd/api/v1/stores"),
]


def snapshot(label, path):
    """One sample, on its own task so that a failure cannot escape into report().

    http_request raises when the connection is refused, and report() starts
    before PD is listening -- main() launches it alongside spawn(), and resolve
    has a peer election to run first. A raise here would kill the whole loop on
    its first iteration.
    """
    r = http_request("GET", "http://127.0.0.1:%d%s" % (CLIENT_PORT, path), timeout = "5s")
    if r.status == 200:
        log("pd: " + label, who = SELF.ipv4, body = r.body)


def report():
    """Publish this replica's own view of the cluster, on a loop.

    Nothing outside the VPC can query PD: a worker pool instance is reachable
    from the network and nowhere else, and there is no bastion in this stack.
    So each replica reports about *itself* and the test reads the logs -- which
    is exactly as exhaustive as the ECS suite shelling into each task, and is
    what makes the split-brain check possible at all. Asking one replica through
    a load balancer could never see two clusters.

    Nothing here may raise: it runs on a background task for the life of the
    workload, and a failed probe is a thing to skip, not to die of. Starlark has
    no way to catch a raise, so containment is structural -- each sample runs on
    a task of its own and select() awaits it without re-raising. join() would
    raise, which is the whole difference.
    """
    for _ in range(720):
        for label, path in REPORTS:
            select(go(snapshot, label, path))
        sleep("15s")


def pd_readiness():
    """Serving locally, and discoverable by the rest of the cluster.

    Discovery belongs in readiness because a PD nobody can find is not usable:
    the stores reach PD only through instances(), so an unregistered replica is
    invisible to the tier that depends on it.

    It also makes a registration failure loud. register() runs in post_start,
    which is best-effort by design -- a failure there is logged and the workload
    carries on. That is right in general and was wrong here: registration once
    failed on every replica and the run looked healthy, three PDs ready and
    serving, while the store tier waited forever for peers that discovery would
    never return. Gating readiness on it restarts the attempt instead, which
    also retries the registration.
    """
    def ready():
        if not http_ok(LOCAL_MEMBERS, "3s")():
            return False
        return SELF.ipv4 in [i.ipv4 for i in instances(PD_SERVICE, health_status = "ALL")]

    # Shorter than the ECS script's 30m: nothing here consumes readiness as a
    # container health check, so an unready attempt is simply restarted, and
    # waiting half an hour to retry a failed registration helps nobody.
    return poll(ready, "10m", interval = "5s")


def pd_down():
    """Truthy only once PD has been unreachable for a solid minute."""
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
    """Teardown, on a ten-second budget.

    Cloud Run sends SIGTERM and SIGKILLs 10 seconds later, and unlike ECS's
    stopTimeout that is not tunable. Both calls below are single round trips, so
    they fit -- but the order matters and the timeouts are deliberately short.

    The lease goes first: it is what another replica may be blocked on, and the
    member removal can fail when no peer is reachable.

    The member removal is not optional. The disk and the address are both
    ephemeral, so a replacement comes back with a different address and
    therefore a different member name; without this the Raft group accumulates a
    dead member per replacement and eventually loses quorum. This is the same
    reason the ECS script does it, and the opposite of what a platform with
    stable identities would want.
    """
    ip = me()
    kv_delete(SEED_KEY, if_value = ip)

    name = member_name(ip)
    if not drop_member(live_peers(other_pds(ip)), name):
        log("pd: no peer accepted the member removal", name = name)

    deregister()


def main():
    w = spawn(
        name = "pd",
        resolve = resolve_pd,
        post_start = pd_registered,
        pre_stop = pd_pre_stop,
        readiness = pd_readiness,
        liveness = pd_liveness,
        respawn = True,
        max_retries = 5,
        # A PD that goes unresponsive is reported unhealthy and left alone
        # rather than restarted here: restarting in place would first run
        # pre_stop, which evicts us from our own Raft group.
        restart_on_liveness = False,
        # Inside Cloud Run's fixed ten seconds, with room for the SIGTERM the
        # workload itself needs to handle.
        pre_stop_timeout = "5s",
        shutdown_grace = "8s",
    )
    go(report)
    return w
