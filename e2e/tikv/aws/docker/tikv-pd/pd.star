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
PD_REPLICAS = int(env("MUSTER_PD_REPLICAS", "3"))

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
    go(prune_loop)
    return w
