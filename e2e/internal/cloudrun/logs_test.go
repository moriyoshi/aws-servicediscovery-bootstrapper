package cloudrun

import (
	"strings"
	"testing"
)

// realEntries is captured output from
// `gcloud logging read ... --format=json` against the PD worker pool: one
// structured line muster wrote, one plain line PD wrote. Trimmed, not reshaped.
const realEntries = `[
  {
    "timestamp": "2026-08-12T13:56:35.215436904Z",
    "resource": {
      "type": "cloud_run_worker_pool",
      "labels": {"worker_pool_name": "muster-e2e-tikv-tikv-pd", "location": "asia-northeast1",
                 "revision_name": "muster-e2e-tikv-tikv-pd-00001-wrr"}
    },
    "jsonPayload": {"level": "INFO", "msg": "pd: STORES", "who": "10.128.253.18", "body": "{\"count\": 3}"}
  },
  {
    "timestamp": "2026-08-12T13:56:20.101000000Z",
    "resource": {
      "type": "cloud_run_worker_pool",
      "labels": {"worker_pool_name": "muster-e2e-tikv-tikv-pd"}
    },
    "textPayload": "[2026/08/12 13:56:20.100 +00:00] [INFO] [server.go:1717] [\"campaign PD leader ok\"]"
  }
]`

// The filter names a resource type, and a type that does not exist yields an
// empty result rather than an error -- so the only thing standing between
// `cloud_run_worker` and twenty silent minutes is comparing the constant to
// something real.
func TestEntryShape(t *testing.T) {
	entries, err := ParseEntries([]byte(realEntries))
	if err != nil {
		t.Fatalf("ParseEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	for _, e := range entries {
		if e.Resource.Type != WorkerPoolResourceType {
			t.Errorf("captured entry has resource.type %q, but the filter asks for %q; "+
				"a filter naming a type that does not exist matches nothing, silently",
				e.Resource.Type, WorkerPoolResourceType)
		}
	}

	// Assertions scope their reads to one revision through this label, and a
	// label name that does not exist filters everything out rather than
	// erroring -- the same silent shape as the resource type above.
	if got := entries[0].Resource.Labels[RevisionLabel]; got == "" {
		t.Errorf("captured entry has no %q label; scoping to a revision would match nothing. Labels: %v",
			RevisionLabel, entries[0].Resource.Labels)
	}

	// muster's own line: structured, so textPayload is empty and everything is
	// in jsonPayload. Reading textPayload here was the second half of the bug.
	if got := entries[0].JSON.Msg; got != "pd: STORES" {
		t.Errorf("muster's line decoded msg %q, want %q", got, "pd: STORES")
	}
	if entries[0].Text != "" {
		t.Errorf("muster's line has textPayload %q; it should carry jsonPayload only", entries[0].Text)
	}

	// PD's own line: the other way round.
	if entries[1].JSON.Msg != "" || !strings.Contains(entries[1].Text, "campaign PD leader ok") {
		t.Errorf("PD's line decoded as %+v, want textPayload only", entries[1])
	}
}

func TestLatestReports(t *testing.T) {
	entries, err := ParseEntries([]byte(realEntries))
	if err != nil {
		t.Fatalf("ParseEntries: %v", err)
	}
	got := LatestReports(entries, "pd: STORES")
	if len(got) != 1 || got["10.128.253.18"].Body != `{"count": 3}` {
		t.Fatalf("LatestReports() = %+v, want one report from 10.128.253.18", got)
	}
	if r := LatestReports(entries, "pd: CLUSTER"); len(r) != 0 {
		t.Errorf("LatestReports() matched %d entries of another kind", len(r))
	}
}

// A replica reports on a loop, so the assertions must see its current view. An
// early report describes a cluster still forming -- one member, no stores -- and
// asserting on that would assert on a moment rather than a state.
func TestLatestReportsTakesTheNewestPerReplica(t *testing.T) {
	const out = `[
	  {"timestamp": "2026-08-12T13:00:02Z", "jsonPayload": {"msg": "m", "who": "a", "body": "new-a"}},
	  {"timestamp": "2026-08-12T13:00:01Z", "jsonPayload": {"msg": "m", "who": "a", "body": "old-a"}},
	  {"timestamp": "2026-08-12T13:00:03Z", "jsonPayload": {"msg": "m", "who": "b", "body": "only-b"}},
	  {"timestamp": "2026-08-12T13:00:04Z", "jsonPayload": {"msg": "m", "body": "no-who"}}
	]`
	entries, err := ParseEntries([]byte(out))
	if err != nil {
		t.Fatalf("ParseEntries: %v", err)
	}
	// Descending order on purpose: gcloud can be asked for either, and which one
	// wins must not depend on that.
	got := LatestReports(entries, "m")
	if len(got) != 2 {
		t.Fatalf("got %d replicas, want 2 (an entry with no `who` names no replica)", len(got))
	}
	if got["a"].Body != "new-a" {
		t.Errorf("replica a reported %q, want the newest, %q", got["a"].Body, "new-a")
	}
}

func TestParseEntriesOnNoLogs(t *testing.T) {
	// gcloud prints nothing at all when a filter matches nothing, which is not
	// the same as invalid output and must not read as an error.
	for _, raw := range []string{"", "   \n", "[]"} {
		entries, err := ParseEntries([]byte(raw))
		if err != nil {
			t.Errorf("ParseEntries(%q) = %v, want no error", raw, err)
		}
		if len(entries) != 0 {
			t.Errorf("ParseEntries(%q) returned %d entries", raw, len(entries))
		}
	}
	if _, err := ParseEntries([]byte("{oops")); err == nil {
		t.Error("ParseEntries accepted malformed output")
	}
}

// A failure dump that reaches none of muster's decisions is the reason this
// split exists: PD writes thousands of lines an hour, so a flat tail of a few
// hundred covers a minute of raft chatter and nothing else. Separating the two
// lets the dump keep every decision and still bound the workload's output.
func TestDecisionsAndWorkloadSplitTheLog(t *testing.T) {
	const out = `[
	  {"timestamp": "2026-08-12T13:00:01Z", "jsonPayload": {"msg": "pd: won the seed lease, bootstrapping a new cluster", "name": "pd-a"}},
	  {"timestamp": "2026-08-12T13:00:02Z", "jsonPayload": {"msg": "pd: STORES", "who": "a", "body": "{}"}},
	  {"timestamp": "2026-08-12T13:00:03Z", "textPayload": "[INFO] raft chatter"},
	  {"timestamp": "2026-08-12T13:00:04Z", "jsonPayload": {"msg": "registered endpoint", "level": "INFO"}},
	  {"timestamp": "2026-08-12T13:00:05Z", "jsonPayload": {"msg": "pd: CLUSTER", "who": "a", "body": "{}"}}
	]`
	entries, err := ParseEntries([]byte(out))
	if err != nil {
		t.Fatalf("ParseEntries: %v", err)
	}

	got := Decisions(entries)
	if len(got) != 2 {
		t.Fatalf("Decisions() returned %d lines, want the 2 muster wrote that are not reports", len(got))
	}
	for _, e := range got {
		if e.IsReport() {
			t.Errorf("Decisions() kept a periodic report: %s", e.JSON.Msg)
		}
	}

	// The workload's own output is the other half, and none of muster's.
	w := Workload(entries)
	if len(w) != 1 || w[0].Text != "[INFO] raft chatter" {
		t.Fatalf("Workload() = %+v, want the one textPayload line", w)
	}
}
