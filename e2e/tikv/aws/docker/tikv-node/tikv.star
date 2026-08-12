# muster script for the TiKV store tier.
#
# Stores have no election to run: whichever one gets there first bootstraps the
# first region, and PD arbitrates that. What they do need is to not start until
# PD is discoverable and answering, and to be handed the full PD endpoint list.

# The fallback for me(), and optional now that SELF.ipv4 is the primary source.
CIDR = env("MUSTER_SUBNET_CIDR")
PD_SERVICE = env("MUSTER_PD_SERVICE", "tikv-pd")
PD_REPLICAS = int(env("MUSTER_PD_REPLICAS", "3"))

DATA_DIR = env("MUSTER_DATA_DIR", "/db")
PD_PORT = 2379
PORT = 20160
STATUS_PORT = 20180
LOCAL_STATUS = "http://127.0.0.1:%d/status" % STATUS_PORT


def me():
    """This task's own address on the VPC. See pd.star's me() for why ifaddr()
    is a fallback rather than the source."""
    if SELF and SELF.ipv4:
        return SELF.ipv4
    if CIDR:
        return ifaddr(CIDR)
    fail("tikv: this task's own address is unknown -- the task metadata endpoint " +
         "told muster nothing and MUSTER_SUBNET_CIDR is unset, so there is no " +
         "address to advertise to PD")


def pd_addrs():
    # Sorted so a respawn produces the same argv when nothing has changed.
    return sorted(["%s:%d" % (i.ipv4, PD_PORT) for i in instances(PD_SERVICE, health_status = "ALL") if i.ipv4])


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
    #
    # Bounded well inside the service's health-check grace period: the stack
    # only starts the stores once PD is already steady, so if PD is not there
    # within four minutes something is actually wrong, and a logged error and a
    # retry say so more clearly than blocking until the scheduler intervenes.
    if not join(poll(pd_up, "240s", interval = "5s")):
        fail("tikv: PD was not reachable within 240s")

    ip = me()
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


def tikv_readiness():
    return poll(http_ok(LOCAL_STATUS, "3s"), "30m", interval = "5s")


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


def main():
    return spawn(
        name = "tikv",
        resolve = resolve_tikv,
        readiness = tikv_readiness,
        liveness = tikv_liveness,
        respawn = True,
        # See pd.star: bounded so a resolve() that can never succeed exits and
        # lets ECS replace the task, rather than hanging as a healthy-looking
        # container forever.
        max_retries = 5,
        # Unlike PD, a store has no membership to be evicted from and keeps its
        # data directory across a restart, so restarting in place on a liveness
        # loss (the spawn() default) is the cheapest way back.
        shutdown_grace = "30s",
    )
