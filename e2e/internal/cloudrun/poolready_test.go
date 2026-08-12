package cloudrun

import (
	"strings"
	"testing"
)

// readyPool is real output, captured from
// `gcloud beta run worker-pools describe --format=json` against a pool that had
// been serving for some minutes. Trimmed to the fields PoolReady reads.
const readyPool = `{
  "apiVersion": "run.googleapis.com/v1",
  "kind": "WorkerPool",
  "metadata": {"name": "muster-e2e-tikv-tikv-pd", "generation": 1},
  "status": {
    "observedGeneration": 1,
    "latestCreatedRevisionName": "muster-e2e-tikv-tikv-pd-00001-wrr",
    "latestReadyRevisionName": "muster-e2e-tikv-tikv-pd-00001-wrr",
    "conditions": [
      {"lastTransitionTime": "2026-08-12T13:38:59.326356Z", "status": "True", "type": "Ready"}
    ]
  }
}`

// v2Pool is the same pool as the Cloud Run v2 API describes it -- the shape the
// Terraform provider stores, and the one this check was originally written
// against. Everything it looks for is absent.
const v2Pool = `{
  "name": "projects/p/locations/l/workerPools/muster-e2e-tikv-tikv-pd",
  "generation": "1",
  "observedGeneration": "1",
  "reconciling": false,
  "terminalCondition": {"type": "Ready", "state": "CONDITION_SUCCEEDED"}
}`

func TestPoolReady(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string // substring of the expected error; empty means ready
	}{
		{name: "ready", raw: readyPool},
		{
			// The regression. Against this input the old check reported
			// "condition  is :" -- two empty fields where the answer belonged --
			// and waited out its full fifteen-minute budget on a pool that was
			// ready the whole time. Silence about a shape mismatch is the bug;
			// the wrong field names were only how it happened.
			name: "v2 shape is called out, not silently unready",
			raw:  v2Pool,
			want: "no Ready condition at all",
		},
		{
			name: "not ready reports the platform's own reason",
			raw: `{"metadata":{"generation":1},"status":{"observedGeneration":1,
			       "latestCreatedRevisionName":"r1","latestReadyRevisionName":"r1",
			       "conditions":[{"type":"Ready","status":"False","reason":"RevisionFailed",
			                      "message":"Image pull failed"}]}}`,
			want: "Image pull failed",
		},
		{
			name: "still reconciling",
			raw:  `{"metadata":{"generation":2},"status":{"observedGeneration":1}}`,
			want: "still reconciling",
		},
		{
			// A rolled revision that never came up: the pool keeps reporting
			// Ready=True for the revision it is still running, so the condition
			// alone would call this ready. PDReplacementRejoins depends on
			// noticing the difference.
			name: "created a revision that is not the ready one",
			raw: `{"metadata":{"generation":2},"status":{"observedGeneration":2,
			       "latestCreatedRevisionName":"r2","latestReadyRevisionName":"r1",
			       "conditions":[{"type":"Ready","status":"True"}]}}`,
			want: "ready revision is",
		},
		{name: "unparseable", raw: `{`, want: "parsing describe output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := PoolReady("muster-e2e-tikv-tikv-pd", []byte(tc.raw))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("PoolReady() = %v, want ready", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("PoolReady() reported ready, want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("PoolReady() = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
