package provider

import "os"

// Identity is what the platform knows about the instance muster runs on. It is
// read once at startup, best-effort: outside a recognised platform it is simply
// nil, and the SELF global is None.
//
// Every field is optional. A platform that cannot answer leaves it empty rather
// than inventing a value, because scripts branch on emptiness -- see Name.
type Identity struct {
	// Provider is the registered provider name; always set when Identity is
	// non-nil.
	Provider string

	// ID uniquely identifies this instance and is stable for its lifetime. It
	// is also the kv lease owner. ECS: the task ARN. GCE: the instance name
	// joined with the numeric instance id, because an autohealed replacement
	// reuses the name and must not be able to renew its predecessor's lease.
	ID string

	// Name is a short identity that survives replacement and is therefore safe
	// to persist as a cluster member name. Empty where the platform has no such
	// thing -- notably ECS/Fargate, whose tasks get a fresh id and address every
	// time. Scripts must treat "" as "derive a name some other way", not as a
	// name; that emptiness is the portability signal.
	Name string

	// Group is the scheduling group this instance belongs to (ECS cluster, MIG
	// location) and Service the replica set within it. Together they default
	// the target of all_replicas_running().
	Group   string
	Service string

	Zone    string // failure domain
	Region  string
	Network string // VPC / VPC network
	IPv4    string
	IPv6    string

	// CreatedAt is the platform's own timestamp, verbatim and unparsed: scripts
	// compare it lexicographically as a deterministic tie-break, and reformatting
	// it would break that against peers running a different muster.
	CreatedAt string

	// Extra carries provider-specific fields with no neutral equivalent. It is
	// exposed to scripts as SELF.<provider>.<key>, so reading one on the wrong
	// build fails loudly instead of returning something plausible.
	Extra map[string]string
}

// OwnerID is the kv lease owner for self: its ID, falling back to the hostname
// when the platform gave us no identity at all. The owner only has to be
// unique -- Renew compares it for equality -- so a hostname is a sound last
// resort outside a recognised platform.
func OwnerID(self *Identity) string {
	if self != nil && self.ID != "" {
		return self.ID
	}
	if hn, err := os.Hostname(); err == nil {
		return hn
	}
	return ""
}
