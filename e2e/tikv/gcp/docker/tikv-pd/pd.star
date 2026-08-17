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

# The store tier, for the reconciliation loop below.
TIKV_SERVICE = env("MUSTER_TIKV_SERVICE", "tikv-node")
TIKV_PORT = 20160
TIKV_STATUS_PORT = 20180
TIKV_REPLICAS = int(env("MUSTER_TIKV_REPLICAS", "3"))
PRUNE_INTERVAL = env("MUSTER_PRUNE_INTERVAL", "60s")
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


def seed_looks_abandoned(seed):
    """Whether the seed lease names a replica that no longer exists.

    Only ever consulted after the seed has failed to serve for five minutes, so
    this is the second half of the evidence rather than the whole of it.

    It has to be, because getting it wrong is a split brain. The lease exists to
    make exactly one replica bootstrap; releasing one that is merely slow would
    let a second replica bootstrap a second cluster. So: the seed must have been
    silent for the full wait above, *and* be absent from a discovery answer large
    enough to be believed.
    """
    peers = [i.ipv4 for i in instances(PD_SERVICE, health_status = "ALL") if i.ipv4]
    if len(peers) * 2 <= PD_REPLICAS:
        return False
    return seed not in peers


def await_seed():
    """Block until the seed lease is taken, and its holder is serving."""
    if not join(poll(lambda: kv_get(SEED_KEY) != None, "180s", interval = "2s")):
        fail("pd: no seed was elected within 180s")
    seed = kv_get(SEED_KEY)
    if join(poll(http_ok(client_url(seed) + "/pd/api/v1/members", "2s"), "300s", interval = "2s")):
        return seed

    # Five minutes of silence. Either the seed is very slow, or the lease
    # outlived the cluster that wrote it -- which is what a full restart leaves
    # behind, because the lease lives in a store that survives the cluster while
    # PD's own data directory does not. Left alone this still recovers, once the
    # lease TTL expires and some later attempt wins a fresh election; releasing
    # it here turns several minutes of retries into one.
    if seed_looks_abandoned(seed):
        log("pd: the seed lease names a replica that no longer exists; releasing " +
            "it so the next attempt can hold a fresh election", seed = seed)
        # Conditional on the value we judged: if anyone has since taken the
        # lease, this is a no-op rather than a theft.
        kv_delete(SEED_KEY, if_value = seed)
    fail("pd: elected seed %s never started serving" % seed)


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

    # Each fallible step on its own task, awaited with select(), which does not
    # re-raise. http_request raises when a peer is already gone -- and during a
    # teardown that stops several instances at once, it will be -- so a bare
    # call here aborts pre_stop and skips everything after it.
    #
    # What that cost: a failed DELETE meant deregister() never ran, and the
    # instance left its Service Directory endpoint behind. The cluster was fine
    # and discovery was not, which is the worse of the two failures, because
    # every peer that later reads discovery believes in a replica that is gone.
    select(go(release_seed, ip))
    select(go(evict_member, ip))

    # Last, and deliberately bare: by this point nothing depends on it, so a
    # failure should be reported as a failed pre_stop rather than swallowed.
    deregister()


def release_seed(ip):
    kv_delete(SEED_KEY, if_value = ip)


def evict_member(ip):
    name = member_name(ip)
    if not drop_member(live_peers(other_pds(ip)), name):
        log("pd: no peer accepted the member removal", name = name)


# --- reconciling PD's store list with the stores that exist ------------------
#
# A store's identity lives in its data directory. That directory is ephemeral
# here, so a replaced task is not a restarted store: it is a new, empty one at a
# new address, and the old store's record survives in PD along with every region
# that had a replica on it.
#
# PD does recover from this on its own -- after max-store-down-time, thirty
# minutes by default, it declares the store down and rebuilds its replicas
# elsewhere. Thirty minutes is long enough that a rolling deployment replaces
# the next store first, and the one after that, and by the third the regions
# have no live replica left and the cluster is unrecoverable. That is not a
# hypothetical: it is what happened, and every process stayed up and every
# health check stayed green throughout.
#
# So this closes the window. It is the same move drop_member() makes for PD's
# own Raft membership, one tier down.


def local_json(path):
    """GET a PD API path on this replica and decode it, or None.

    Guarded by a probe rather than trusting http_request not to raise: this runs
    on a loop for the life of the process, including while PD is restarting.
    """
    url = "http://127.0.0.1:%d%s" % (CLIENT_PORT, path)
    if not http_ok(url, "2s")():
        return None
    r = http_request("GET", url, timeout = "5s")
    if r.status != 200:
        return None
    return json.decode(r.body)


def is_pd_leader():
    """Only the leader prunes, so three replicas do not race to issue the same
    deletion and fill the log with each other's rejections."""
    m = local_json("/pd/api/v1/members")
    if m == None:
        return False
    leader = m.get("leader")
    if leader == None:
        return False
    return leader.get("name") == member_name(me())


def live_store_addrs():
    return ["%s:%d" % (i.ipv4, TIKV_PORT) for i in instances(TIKV_SERVICE, health_status = "ALL") if i.ipv4]


def stale_store(stores, live):
    """The first store PD believes in that no longer exists, or None.

    Two conditions, and both matter:

      the address is absent from discovery -- a task that restarts in place
        keeps its registration, so absence means the *task* is gone, which on
        ephemeral storage means the data is gone with it. This is the load-
        bearing signal; PD's own view cannot distinguish a store that is
        restarting from one that will never come back.

      PD is not hearing from it -- Disconnected or Down. Redundant with the
        above in every case we know of, and cheap insurance against acting on a
        discovery answer that is merely stale.

    One at a time. Returning the whole list and deleting it would let a single
    bad reading take out the cluster's metadata in one pass; returning one means
    the next pass re-derives the evidence from scratch.
    """
    for s in stores:
        st = s["store"]
        if st["state_name"] == "Tombstone" or st["state_name"] == "Offline":
            continue  # PD has already been told about this one
        if st["address"] in live:
            continue
        if st["state_name"] == "Up":
            continue  # heartbeating, so something is answering for it
        return st
    return None


def discovery_is_trustworthy(n):
    """Whether a discovery answer listing n stores is worth acting on.

    The guard that makes everything below it safe. An empty or short answer is
    far more likely to be a throttled API call than a tier that vanished, and
    acting on one would delete the metadata of a perfectly healthy cluster.

    A strict majority of the expected tier, not the whole of it. Requiring every
    store to be visible sounds safer and is useless: during a rolling
    replacement exactly one store is legitimately missing, which is precisely
    when there is a dead record to release. A majority tolerates that and still
    refuses to act on an answer that has collapsed.
    """
    return n * 2 > TIKV_REPLICAS


def status_url(st):
    """Where a store answers about itself.

    PD reports status_address exactly as the store advertised it, which is
    0.0.0.0 when a deployment forgot --advertise-status-addr; fall back to the
    service address's host rather than probing a meaningless target.
    """
    sa = st.get("status_address", "")
    if sa and not sa.startswith("0.0.0.0"):
        return "http://%s/status" % sa
    return "http://%s:%d/status" % (st["address"].split(":")[0], TIKV_STATUS_PORT)


def store_is_occupied(st):
    """Whether a live process is answering at the store's own address.

    The signal discovery cannot deliver in time, and the reason this check
    exists at all.

    PD identifies a store by its address, so when a replacement lands on an
    address some earlier store used -- which happens routinely in a small subnet
    -- PD hands the new process that old record, still marked Down because it
    has not heartbeated yet. For a few seconds the record is indistinguishable
    from an abandoned one: not Up, and not yet in discovery, because
    registration lags process start.

    Releasing it in that window is fatal and permanent. PD tombstones the
    record, and the live process holding it is rejected from then on with
    StoreTombstone -- a healthy store destroyed by the thing meant to protect
    the cluster. Not hypothetical: this did exactly that, eight seconds after a
    replacement started.

    A probe of the store's own status port cannot lag. Either something is
    listening at that address or nothing is.
    """
    return http_ok(status_url(st), "2s")()


def reap_tombstones(stores):
    """Clear tombstoned records so their addresses can be used again.

    PD refuses to register a store at an address a tombstone still holds, so on
    a platform that recycles addresses a leftover tombstone eventually rejects a
    perfectly good replacement. One call clears them all.
    """
    for s in stores:
        if s["store"]["state_name"] != "Tombstone":
            continue
        r = http_request("DELETE", "http://127.0.0.1:%d/pd/api/v1/stores/remove-tombstone" % CLIENT_PORT,
                         timeout = "10s")
        if r.status >= 300:
            log("pd: could not clear tombstoned store records",
                status = str(r.status), body = r.body)
        else:
            log("pd: cleared tombstoned store records so their addresses can be reused")
        return


def cluster_lost_its_data(stores):
    """Whether every region's replicas lived on stores that no longer exist.

    Up stores holding no regions at all, beside records PD is no longer hearing
    from, means the data is entirely on stores that are gone. There is nothing to
    rebuild from: `store delete` never completes because no surviving replica
    exists to copy, and the cluster cannot serve a single read.

    This is the one case deliberately left to a human. Recovering it means
    `pd-ctl unsafe remove-failed-stores`, which discards whatever those regions
    held -- a policy decision, not a repair. Automating it would be a script
    throwing data away on its own judgement, and that judgement is precisely the
    one this loop has already been wrong about once.
    """
    live_regions, dead = 0, 0
    for s in stores:
        if s["store"]["state_name"] == "Up":
            live_regions += s["status"].get("region_count", 0)
        elif s["store"]["state_name"] != "Tombstone":
            dead += 1
    return dead > 0 and live_regions == 0


def prune_pass():
    """One reconciliation. Nothing here may raise; see local_json()."""
    if not is_pd_leader():
        return

    stores = local_json("/pd/api/v1/stores")
    if stores == None:
        return

    live = live_store_addrs()

    if not discovery_is_trustworthy(len(live)):
        return

    if cluster_lost_its_data(stores["stores"]):
        log("pd: every store PD can reach holds no regions, while records it " +
            "cannot reach remain. The data lived entirely on stores that no " +
            "longer exist, so this cluster can neither serve nor rebuild itself. " +
            "It needs `pd-ctl unsafe remove-failed-stores`, which discards those " +
            "regions, or recreating from scratch. Nothing here will do either.")

    reap_tombstones(stores["stores"])

    st = stale_store(stores["stores"], live)
    if st == None:
        return

    # The last and most important gate. See store_is_occupied().
    if store_is_occupied(st):
        log("pd: PD calls this store dead but something is answering at its " +
            "address; leaving the record alone until discovery catches up",
            store = str(st["id"]), address = st["address"], state = st["state_name"])
        return

    log("pd: PD still lists a store that no longer exists; taking it offline so " +
        "its replicas can be rebuilt rather than waiting out max-store-down-time",
        store = str(st["id"]), address = st["address"], state = st["state_name"])

    # `store delete` takes it Offline; it does not throw data away. PD removes
    # each of its peers only after building a replacement from a surviving
    # replica, and a region with no surviving replica keeps the store Offline
    # forever -- which is the honest outcome, and the signal that this needs
    # `pd-ctl unsafe remove-failed-stores` and a human.
    r = http_request("DELETE", "http://127.0.0.1:%d/pd/api/v1/store/%d" % (CLIENT_PORT, st["id"]), timeout = "10s")
    if r.status < 300:
        return

    # PD has its own guard: it will not release a record if doing so would leave
    # fewer up stores than max-replicas. On a cluster running the minimum three
    # stores for three replicas that refusal is routine rather than exceptional
    # -- the count only reaches three again once the replacement has registered,
    # which is seconds after it boots. Saying so plainly keeps a normal
    # replacement from reading like a fault.
    if "ErrStoresNotEnough" in r.body:
        log("pd: not releasing the dead store record yet; the cluster is at its " +
            "minimum size, so PD will not drop below max-replicas until the " +
            "replacement store has registered",
            store = str(st["id"]), address = st["address"])
        return

    log("pd: PD refused the store removal", store = str(st["id"]), status = str(r.status), body = r.body)


def prune_loop():
    """Runs for the life of the process; see main()."""
    for _ in range(100000):
        sleep(PRUNE_INTERVAL)
        # Contained: a raise here would kill the loop on its first bad minute,
        # and Starlark has no way to catch one.
        select(go(prune_pass))


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
    go(prune_loop)
    return w
