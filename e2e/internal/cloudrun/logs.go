package cloudrun

import (
	"encoding/json"
	"fmt"
	"strings"
)

// WorkerPoolResourceType is what Cloud Logging calls a worker pool instance.
//
// Not `cloud_run_worker`, which is what this filtered on at first and which
// matches nothing at all — a filter naming a resource type that does not exist
// is not an error, it is an empty result set, so the assertions above it
// reported "no replica has reported a cluster yet" about a cluster that had
// been reporting for twenty minutes.
//
// TestEntryShape pins this against a captured entry, because the failure mode
// is silence and the only defence is comparing it to real data.
const WorkerPoolResourceType = "cloud_run_worker_pool"

// Report is one self-report line, as muster's logger wrote it.
type Report struct {
	Msg  string `json:"msg"`
	Who  string `json:"who"`
	Body string `json:"body"`
}

// LogEntry is one Cloud Logging entry from `gcloud logging read --format=json`.
//
// Both payload fields matter and only one is ever set. Anything muster logs is
// structured JSON on stdout, which the logging agent parses into jsonPayload;
// anything the workload writes itself — PD's and TiKV's own output — arrives as
// textPayload. Reading only textPayload, as this did, is blind to muster
// exactly where the suite depends on it, and to nothing else.
type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Text      string `json:"textPayload"`
	JSON      struct {
		Report
		Level string `json:"level"`
		Name  string `json:"name"`
		Err   string `json:"err"`
	} `json:"jsonPayload"`
	Resource struct {
		Type   string            `json:"type"`
		Labels map[string]string `json:"labels"`
	} `json:"resource"`
}

// String renders an entry for a human reading a failure dump, whichever payload
// it carries.
func (e LogEntry) String() string {
	ts := e.Timestamp
	if len(ts) > 23 {
		ts = ts[11:23]
	}
	if e.Text != "" {
		return ts + " " + strings.TrimRight(e.Text, "\n")
	}
	j := e.JSON
	line := ts + " " + j.Level + " " + j.Msg
	for _, kv := range []struct{ k, v string }{{"who", j.Who}, {"name", j.Name}, {"err", j.Err}, {"body", j.Body}} {
		if kv.v != "" {
			line += fmt.Sprintf(" %s=%q", kv.k, kv.v)
		}
	}
	return line
}

// ParseEntries decodes `gcloud logging read --format=json` output. An empty
// result is not an error: it means nothing has been logged yet, which callers
// distinguish from a broken filter by what they were expecting.
func ParseEntries(raw []byte) ([]LogEntry, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	var entries []LogEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parsing log entries: %w", err)
	}
	return entries, nil
}

// RevisionLabel is where an entry records the revision whose instance wrote it.
const RevisionLabel = "revision_name"

// StructuredFilter selects the entries muster wrote: it logs JSON on stdout,
// which the agent parses into jsonPayload, while the workload's own output
// arrives as textPayload.
const StructuredFilter = `jsonPayload.msg!=""`

// MsgFilter selects one kind of line by its exact message. Cloud Logging's
// --limit bounds what the query returns, not what matches afterwards, so a
// caller after a handful of self-reports has to say so in the filter or spend
// the whole limit on whatever the workload happened to be printing.
func MsgFilter(msg string) string {
	return fmt.Sprintf("jsonPayload.msg=%q", msg)
}

// IsReport reports whether an entry is one of the periodic self-reports. They
// are the assertions' input and a failure dump's noise: each carries a whole PD
// API response, so a handful of them crowds out everything else.
func (e LogEntry) IsReport() bool {
	return strings.HasPrefix(e.JSON.Msg, "pd: CLUSTER") ||
		strings.HasPrefix(e.JSON.Msg, "pd: MEMBERS") ||
		strings.HasPrefix(e.JSON.Msg, "pd: STORES")
}

// Decisions returns the lines muster itself wrote, minus the periodic reports:
// which branch resolve() took, what it registered, what it respawned and why.
//
// These are what a failure dump is for, and they are also the rarest lines in
// it. PD emits thousands of lines an hour, so a dump of the last few hundred
// covers a minute or two of raft chatter and reaches nothing muster decided --
// which is exactly what happened the first time this suite failed for a reason
// worth reading.
func Decisions(entries []LogEntry) []LogEntry {
	var out []LogEntry
	for _, e := range entries {
		if e.JSON.Msg != "" && !e.IsReport() {
			out = append(out, e)
		}
	}
	return out
}

// Workload returns the lines the supervised process wrote itself.
func Workload(entries []LogEntry) []LogEntry {
	var out []LogEntry
	for _, e := range entries {
		if e.Text != "" {
			out = append(out, e)
		}
	}
	return out
}

// LatestReports returns the most recent report of the given kind per replica,
// keyed on the address the replica reported about itself.
//
// Latest, not first: a replica's early reports describe a cluster still forming,
// and asserting on those would assert on a moment rather than a state. Entries
// may arrive in either order, so this compares timestamps rather than trusting
// the caller to have sorted them — they are RFC 3339 with a fixed offset, so
// lexicographic order is chronological.
func LatestReports(entries []LogEntry, msg string) map[string]Report {
	out := map[string]Report{}
	when := map[string]string{}
	for _, e := range entries {
		if e.JSON.Msg != msg || e.JSON.Who == "" {
			continue
		}
		if prev, seen := when[e.JSON.Who]; seen && e.Timestamp < prev {
			continue
		}
		out[e.JSON.Who] = e.JSON.Report
		when[e.JSON.Who] = e.Timestamp
	}
	return out
}
