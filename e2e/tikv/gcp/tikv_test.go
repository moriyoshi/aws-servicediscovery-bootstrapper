//go:build e2e_tikv_gcp

package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/moriyoshi/muster/e2e/internal/harness"
)

// TestTiKVOnCloudRun brings a real TiKV cluster up on Cloud Run worker pools,
// with muster as each container's entrypoint, and asserts that what muster
// decided during the cold start was right.
//
// One test with ordered subtests, not several: the stack takes many minutes to
// provision and every assertion is about the same cluster, so they share it and
// run in the order a cluster actually comes together.
func TestTiKVOnCloudRun(t *testing.T) {
	s := setupStack(t)
	ctx := context.Background()

	t.Run("PoolsReachReady", func(t *testing.T) {
		for _, pool := range []string{s.pdPool, s.tikvPool} {
			harness.Eventually(t, pool+" ready", 15*time.Minute, 20*time.Second,
				func(ctx context.Context) error { return s.poolReady(ctx, pool) })
		}
	})

	t.Run("ServiceDirectoryRegistrations", func(t *testing.T) {
		// Every one of these endpoints was written by muster's register().
		// Nothing on Cloud Run registers an instance -- ECS Service Connect has
		// no equivalent here -- so unlike the CloudMap check on the AWS side,
		// this is testing muster's own code rather than the platform's.
		for _, svc := range []struct {
			name string
			want int
		}{
			{s.pdDiscovery, s.pdWant},
			{s.tikvService, s.tikvWant},
		} {
			harness.Eventually(t, svc.name+" registered", 15*time.Minute, 20*time.Second,
				func(ctx context.Context) error {
					eps, err := s.endpoints(ctx, svc.name)
					if err != nil {
						return err
					}
					if len(eps) != svc.want {
						return fmt.Errorf("%s has %d endpoints, want %d", svc.name, len(eps), svc.want)
					}
					return nil
				})
		}
	})

	t.Run("PDClusterBootstrapped", func(t *testing.T) {
		harness.Eventually(t, "PD cluster bootstrapped", 20*time.Minute, 20*time.Second,
			func(ctx context.Context) error {
				reports, err := s.latestReports(ctx, s.pdPool, "pd: CLUSTER")
				if err != nil {
					return err
				}
				if len(reports) == 0 {
					return fmt.Errorf("no replica has reported a cluster yet")
				}
				for who, r := range reports {
					var info pdClusterInfo
					if err := json.Unmarshal([]byte(r.Body), &info); err != nil {
						return fmt.Errorf("%s: decode %q: %w", who, r.Body, err)
					}
					if info.ID == 0 {
						return fmt.Errorf("%s reports cluster id zero", who)
					}
				}
				return nil
			})
	})

	t.Run("NoSplitBrain", func(t *testing.T) {
		// The reason this suite exists. A cold start races every replica for
		// one seed lease; if the lease is not a real conditional write, two
		// replicas each bootstrap a cluster and each is perfectly healthy on
		// its own.
		//
		// Every replica reports the cluster id it sees on its *own* loopback,
		// so this compares all of them rather than whichever one a load
		// balancer picked -- which is the only way the failure is visible, and
		// the same property the ECS suite gets from shelling into each task.
		harness.Eventually(t, "every PD replica reported", 20*time.Minute, 20*time.Second,
			func(ctx context.Context) error {
				reports, err := s.latestReports(ctx, s.pdPool, "pd: CLUSTER")
				if err != nil {
					return err
				}
				if len(reports) < s.pdWant {
					return fmt.Errorf("%d of %d replicas have reported", len(reports), s.pdWant)
				}
				return nil
			})

		reports, err := s.latestReports(ctx, s.pdPool, "pd: CLUSTER")
		if err != nil {
			t.Fatalf("read reports: %v", err)
		}
		ids := map[int64][]string{}
		for who, r := range reports {
			var info pdClusterInfo
			if err := json.Unmarshal([]byte(r.Body), &info); err != nil {
				t.Fatalf("%s: decode %q: %v", who, r.Body, err)
			}
			ids[info.ID] = append(ids[info.ID], who)
		}
		if len(ids) != 1 {
			t.Fatalf("split brain: %d distinct cluster ids across %d replicas: %v",
				len(ids), len(reports), ids)
		}
		for id, who := range ids {
			t.Logf("all %d PD replicas agree on cluster id %d (%s)", len(who), id, strings.Join(who, ", "))
		}
	})

	t.Run("QuorumComplete", func(t *testing.T) {
		harness.Eventually(t, "PD quorum complete", 15*time.Minute, 20*time.Second,
			func(ctx context.Context) error {
				reports, err := s.latestReports(ctx, s.pdPool, "pd: MEMBERS")
				if err != nil {
					return err
				}
				if len(reports) < s.pdWant {
					return fmt.Errorf("%d of %d replicas have reported members", len(reports), s.pdWant)
				}
				// Every replica has to see the full membership, not just one of
				// them: a member that formed its own view is the same failure
				// as a split brain, one level down.
				for who, r := range reports {
					var m pdMembers
					if err := json.Unmarshal([]byte(r.Body), &m); err != nil {
						return fmt.Errorf("%s: decode: %w", who, err)
					}
					if len(m.Members) != s.pdWant {
						return fmt.Errorf("%s sees %d members, want %d: %v",
							who, len(m.Members), s.pdWant, m.names())
					}
				}
				return nil
			})
	})

	t.Run("StoresUp", func(t *testing.T) {
		harness.Eventually(t, "stores up", 20*time.Minute, 20*time.Second,
			func(ctx context.Context) error {
				reports, err := s.latestReports(ctx, s.pdPool, "pd: STORES")
				if err != nil {
					return err
				}
				if len(reports) == 0 {
					return fmt.Errorf("no store report yet")
				}
				for who, r := range reports {
					var stores pdStores
					if err := json.Unmarshal([]byte(r.Body), &stores); err != nil {
						return fmt.Errorf("%s: decode: %w", who, err)
					}
					if stores.Count != s.tikvWant {
						return fmt.Errorf("%s sees %d stores, want %d", who, stores.Count, s.tikvWant)
					}
					for _, entry := range stores.Stores {
						if entry.Store.StateName != "Up" {
							return fmt.Errorf("store %d (%s) is %s",
								entry.Store.ID, entry.Store.Address, entry.Store.StateName)
						}
					}
				}
				return nil
			})
	})

	t.Run("SeedLease", func(t *testing.T) {
		// The lease is released in pre_stop and expires on its own, so a live
		// cluster should not still be holding one. Its absence is what says the
		// election ran and finished rather than never having happened.
		lease, err := s.seedLease(ctx)
		if err != nil {
			t.Fatalf("read seed lease: %v", err)
		}
		if lease == "" {
			t.Log("seed lease is gone, as it should be once the cluster is up")
			return
		}
		// Still held is not a failure on its own -- the winner keeps it until
		// it stops -- but it has to name a registered replica, not a stale
		// address.
		eps, err := s.endpoints(ctx, s.pdDiscovery)
		if err != nil {
			t.Fatalf("list endpoints: %v", err)
		}
		for _, e := range eps {
			if e.Address == lease {
				t.Logf("seed lease still held by %s", lease)
				return
			}
		}
		t.Errorf("seed lease %q belongs to no registered PD replica", lease)
	})

	t.Run("PDReplacementRejoins", func(t *testing.T) {
		if testing.Short() {
			t.Skip("-short: skipping the replacement cycle")
		}
		// A Cloud Run instance's disk and address are both ephemeral, so a
		// replacement is necessarily a *new* member -- the same deal Fargate
		// offers. What must not happen is the replacement deciding it is alone
		// and bootstrapping a second cluster.
		//
		// Rolling the revision replaces every instance, which is the bluntest
		// version of the test: the whole PD tier turns over and the cluster id
		// has to survive it, carried by the seed lease and the peers rather
		// than by anything on disk.
		before, err := s.latestReports(ctx, s.pdPool, "pd: CLUSTER")
		if err != nil || len(before) == 0 {
			t.Fatalf("read cluster id before: %v", err)
		}
		var wantID int64
		for _, r := range before {
			var info pdClusterInfo
			if err := json.Unmarshal([]byte(r.Body), &info); err != nil {
				t.Fatalf("decode: %v", err)
			}
			wantID = info.ID
			break
		}
		t.Logf("cluster id before the roll: %d", wantID)

		rolled := time.Now()
		if err := harness.Run(t, "gcloud", "beta", "run", "worker-pools", "update", s.pdPool,
			"--project="+s.project, "--region="+s.region,
			"--update-env-vars=MUSTER_ROLL="+fmt.Sprint(rolled.Unix()), "--quiet"); err != nil {
			t.Fatalf("roll the PD pool: %v", err)
		}

		harness.Eventually(t, "PD pool ready again", 20*time.Minute, 20*time.Second,
			func(ctx context.Context) error { return s.poolReady(ctx, s.pdPool) })

		harness.Eventually(t, "the same cluster came back", 20*time.Minute, 20*time.Second,
			func(ctx context.Context) error {
				lines, err := s.logLines(ctx, s.pdPool, time.Since(rolled)+2*time.Minute, 4000)
				if err != nil {
					return err
				}
				seen := map[string]int64{}
				for _, line := range lines {
					var r report
					if json.Unmarshal([]byte(line), &r) != nil || r.Msg != "pd: CLUSTER" {
						continue
					}
					var info pdClusterInfo
					if json.Unmarshal([]byte(r.Body), &info) != nil {
						continue
					}
					seen[r.Who] = info.ID
				}
				if len(seen) < s.pdWant {
					return fmt.Errorf("%d of %d replacements have reported", len(seen), s.pdWant)
				}
				for who, id := range seen {
					if id != wantID {
						// Terminal: a replacement that bootstrapped its own
						// cluster will never converge, and waiting out the
						// timeout only delays the report.
						return fmt.Errorf("%w: %s reports cluster id %d, want %d — "+
							"the replacement bootstrapped a new cluster",
							harness.ErrTerminal, who, id, wantID)
					}
				}
				return nil
			})
		t.Logf("the PD tier rolled and stayed on cluster %d", wantID)
	})
}
