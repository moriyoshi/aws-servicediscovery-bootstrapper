# muster script for the TiKV store tier, on a Cloud Run worker pool.
#
# Stores have no election to run: whichever one gets there first bootstraps the
# first region, and PD arbitrates that. What they do need is to not start until
# PD is discoverable and answering, to be handed the full PD endpoint list, and
# -- unlike on ECS -- to publish themselves, because nothing on Cloud Run
# registers an instance.

PD_SERVICE = env("MUSTER_PD_SERVICE", "tikv-pd")
TIKV_SERVICE = env("MUSTER_TIKV_SERVICE", "tikv-node")
PD_REPLICAS = int(env("MUSTER_PD_REPLICAS", "3"))

DATA_DIR = env("MUSTER_DATA_DIR", "/db")
PORT = 20160
STATUS_PORT = 20180
LOCAL_STATUS = "http://127.0.0.1:%d/status" % STATUS_PORT


def me():
    # Only a worker pool instance has an address a peer -- or PD -- can reach.
    # See pd.star for why an empty value means this is deployed wrong.
    if not SELF.ipv4:
        fail("tikv: SELF.ipv4 is empty; a store has to be reachable by PD, which " +
             "means a Cloud Run worker pool -- a service or a job cannot host this")
    return SELF.ipv4


def pd_addrs():
    # Sorted so a respawn produces the same argv when nothing has changed.
    return sorted(["%s:%d" % (i.ipv4, i.port) for i in instances(PD_SERVICE, health_status = "ALL") if i.ipv4])


def pd_up():
    addrs = pd_addrs()
    if len(addrs) < PD_REPLICAS:
        return False

    # Short-circuits on the first PD that answers, cancelling the other probes.
    return any_true(*[go(http_ok("http://%s/pd/api/v1/members" % a, "3s")) for a in addrs])


def resolve_tikv():
    # Wait for PD here rather than in pre_start(): spawn() resolves argv first,
    # and --pd-endpoints below is built from the discovered PD addresses. On
    # timeout this raises, which under resolve_failure="retry" (the default)
    # backs off and tries again instead of starting a store that is blind.
    if not join(poll(pd_up, "300s", interval = "5s")):
        fail("tikv: PD was not reachable within 300s")

    ip = me()

    # Auto-heal the unrecoverable case. A tombstoned record cannot be revived,
    # and the data directory behind it can never register again -- so this task
    # is finished whatever it does next. Failing here rather than starting a
    # store that PD will reject on every heartbeat exhausts max_retries in
    # seconds and exits, which is the signal the orchestrator needs to replace
    # the task with a clean volume at (usually) a different address. pd.star
    # clears the tombstone, so the replacement is not refused in turn.
    #
    # Only ever true on a respawn: the first attempt has not registered yet, so
    # PD has no record of us to have tombstoned.
    stores = pd_stores()
    if stores != None and store_state(stores, "%s:%d" % (ip, PORT)) == "Tombstone":
        fail("tikv: PD has tombstoned the store at %s:%d. That record cannot be " % (ip, PORT) +
             "revived and its data directory cannot be reused, so this task has to " +
             "be replaced rather than restarted -- exiting to let that happen.")

    return COMMAND + [
        "--addr",
        "0.0.0.0:%d" % PORT,
        "--advertise-addr",
        "%s:%d" % (ip, PORT),
        "--status-addr",
        "0.0.0.0:%d" % STATUS_PORT,
        "--advertise-status-addr",
        "%s:%d" % (ip, STATUS_PORT),
        "--data-dir",
        DATA_DIR,
        "--pd-endpoints",
        ",".join(pd_addrs()),
    ]


def tikv_registered():
    register(TIKV_SERVICE, port = PORT)


def pd_stores():
    """PD's whole view of the store tier, or None if no PD answered."""
    for a in pd_addrs():
        r = http_request("GET", "http://%s/pd/api/v1/stores" % a, timeout = "5s")
        if r.status == 200:
            return json.decode(r.body)["stores"]
    return None


def store_state(stores, addr):
    """This store's state as PD sees it: Up, Offline, Down, Tombstone, or None
    when PD has never heard of it."""
    for s in stores:
        if s["store"]["address"] == addr:
            return s["store"]["state_name"]
    return None


def tikv_readiness():
    """Serving locally, discoverable, and accepted into the cluster by PD.

    Discovery is part of readiness for the reason pd.star gives: nothing on Cloud
    Run registers an instance, so an unregistered store is invisible to
    everything that depends on it. PD registration is the newer half -- serving
    locally answers "my process is up", which is true of a store PD has never
    heard of and of one whose record PD has thrown away.

    A hand-rolled loop rather than poll(), because there are three outcomes and
    poll() only distinguishes two: ready, not ready yet, and never going to be.
    """
    addr = "%s:%d" % (SELF.ipv4, PORT)

    def watch():
        for _ in range(120):  # ten minutes at five seconds
            if http_ok(LOCAL_STATUS, "3s")() and \
               SELF.ipv4 in [i.ipv4 for i in instances(TIKV_SERVICE, health_status = "ALL")]:
                stores = pd_stores()
                if stores != None:
                    state = store_state(stores, addr)
                    if state == "Up":
                        return True
                    if state == "Tombstone":
                        # Terminal. A tombstoned record cannot be revived and the
                        # data directory behind it can never be registered again,
                        # so waiting out the rest of the window buys nothing and
                        # costs the orchestrator's whole grace period on a store
                        # that is never coming back. Returning False restarts the
                        # workload, and resolve_tikv() then exits the task so a
                        # clean one takes its place.
                        log("tikv: PD has tombstoned this store's record; it can " +
                            "never register again and needs a fresh task rather " +
                            "than a restart", me = addr)
                        return False
            sleep("5s")
        return False

    return go(watch)


def tikv_down():
    """Truthy only when the status server fails twice in a row."""
    probe = http_ok(LOCAL_STATUS, "3s")

    def check():
        if probe():
            return False
        sleep("3s")
        return not probe()

    return check


def tikv_liveness():
    return poll(tikv_down(), "24h", interval = "10s")


def tikv_pre_stop():
    # A store has no membership to be evicted from -- PD notices it stop and
    # re-replicates its regions -- so there is only the registry entry to
    # withdraw, which fits Cloud Run's ten-second budget with room to spare.
    deregister()


def main():
    return spawn(
        name = "tikv",
        resolve = resolve_tikv,
        post_start = tikv_registered,
        pre_stop = tikv_pre_stop,
        readiness = tikv_readiness,
        liveness = tikv_liveness,
        respawn = True,
        max_retries = 5,
        # Unlike PD, a store has no membership to be evicted from and keeps its
        # data directory across an in-place restart, so restarting on a liveness
        # loss (the spawn() default) is the cheapest way back.
        pre_stop_timeout = "5s",
        shutdown_grace = "8s",
    )
