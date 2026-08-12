//go:build e2e_tikv

package tikv

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
)

// seedKey is the DynamoDB key pd.star races for on a cold start. It must match
// SEED_KEY in docker/tikv-pd/pd.star (the stack sets no -kv-key-prefix).
const seedKey = "tikv-pd/seed"

// memberName mirrors member_name() in pd.star.
func memberName(ip string) string { return "pd-" + strings.ReplaceAll(ip, ".", "-") }

// TestTiKVOnFargate brings a TiKV cluster up on ECS Fargate with muster as the
// container entrypoint and checks that the cluster muster assembled is the one
// it was supposed to: one Raft group, one bootstrap, every store joined.
//
// The subtests share the stack and run in order; each one assumes the previous
// ones passed.
func TestTiKVOnFargate(t *testing.T) {
	s := setupStack(t)

	// Recorded by the bootstrap subtest and re-checked after the chaos one: a
	// second bootstrap would mean the seed election let two PDs through.
	var clusterID uint64

	t.Run("ServicesReachSteadyState", func(t *testing.T) {
		for _, svc := range []struct {
			name string
			want int
		}{
			{s.pdService, s.pdCount},
			{s.tikvService, s.tikvCount},
		} {
			eventually(t, "ECS service "+svc.name+" steady", 20*time.Minute, 15*time.Second,
				func(ctx context.Context) error { return s.serviceSteady(ctx, svc.name, svc.want) })
		}
	})

	// The ECS health check runs `muster -health-probe`, so this is really an
	// assertion about muster's own view: every workload up, readiness passed.
	t.Run("MusterReportsHealthy", func(t *testing.T) {
		for _, svc := range []struct {
			name string
			want int
		}{
			{s.pdService, s.pdCount},
			{s.tikvService, s.tikvCount},
		} {
			eventually(t, "muster health probe on "+svc.name, 15*time.Minute, 15*time.Second,
				func(ctx context.Context) error { return s.tasksHealthy(ctx, svc.name, svc.want) })
		}
	})

	// Everything downstream depends on discovery working, so check it directly
	// rather than only inferring it from the cluster having formed.
	t.Run("CloudMapRegistrations", func(t *testing.T) {
		ctx := context.Background()
		for _, svc := range []struct {
			discovery string
			ecsName   string
			want      int
		}{
			{s.pdDiscovery, s.pdService, s.pdCount},
			{s.tikvDiscover, s.tikvService, s.tikvCount},
		} {
			instances, err := s.discover(ctx, svc.discovery)
			if err != nil {
				t.Fatalf("DiscoverInstances(%s): %v", svc.discovery, err)
			}
			ips := instanceIPs(instances)
			if len(ips) != svc.want {
				t.Errorf("%s: %d healthy instances (%v), want %d", svc.discovery, len(ips), ips, svc.want)
			}
			if dupes := duplicates(ips); len(dupes) > 0 {
				t.Errorf("%s: duplicate registrations for %v", svc.discovery, dupes)
			}

			// CloudMap must agree with ECS about who is actually running,
			// otherwise the scripts are resolving argv from a stale peer list.
			tasks, err := s.listTasks(ctx, svc.ecsName)
			if err != nil {
				t.Fatalf("list tasks for %s: %v", svc.ecsName, err)
			}
			var taskIPs []string
			for _, task := range tasks {
				if ip := taskIP(task); ip != "" {
					taskIPs = append(taskIPs, ip)
				}
			}
			if !sameSet(ips, taskIPs) {
				t.Errorf("%s: CloudMap has %v, ECS tasks are at %v", svc.discovery, sorted(ips), sorted(taskIPs))
			}
		}
	})

	t.Run("PDClusterBootstrapped", func(t *testing.T) {
		ctx := context.Background()

		var info pdClusterInfo
		if err := s.pdGet(ctx, "/pd/api/v1/cluster", &info); err != nil {
			t.Fatalf("get cluster: %v", err)
		}
		if info.ID == 0 {
			t.Fatal("PD reports cluster id 0: the cluster was never bootstrapped")
		}
		clusterID = info.ID
		t.Logf("cluster id %d, max-replicas %d", info.ID, info.MaxPeerCount)

		var members pdMembers
		if err := s.pdGet(ctx, "/pd/api/v1/members", &members); err != nil {
			t.Fatalf("get members: %v", err)
		}
		if len(members.Members) != s.pdCount {
			t.Errorf("PD has %d members (%v), want %d", len(members.Members), members.names(), s.pdCount)
		}
		if members.Leader.Name == "" {
			t.Error("PD has no leader")
		}
		t.Logf("PD members %v, leader %s", sorted(members.names()), members.Leader.Name)

		// Every member should be one of the PD tasks, named after its address.
		// A member that matches nothing is a leftover the teardown path missed.
		instances, err := s.discover(ctx, s.pdDiscovery)
		if err != nil {
			t.Fatalf("DiscoverInstances(%s): %v", s.pdDiscovery, err)
		}
		want := map[string]bool{}
		for _, ip := range instanceIPs(instances) {
			want[memberName(ip)] = true
		}
		for _, name := range members.names() {
			if !want[name] {
				t.Errorf("PD member %q does not correspond to any running task", name)
			}
		}

		var health []pdHealthEntry
		if err := s.pdGet(ctx, "/pd/api/v1/health", &health); err != nil {
			t.Fatalf("get health: %v", err)
		}
		for _, h := range health {
			if !h.Health {
				t.Errorf("PD member %s is unhealthy", h.Name)
			}
		}
	})

	// The point of the seed election. Two replicas that both decided to
	// bootstrap would each be a healthy one-member cluster with its own id, and
	// nothing about querying "the cluster" would reveal it — so ask every
	// replica, on its own loopback, and require them to give the same answer.
	t.Run("NoSplitBrain", func(t *testing.T) {
		if clusterID == 0 {
			t.Skip("cluster id unknown; PDClusterBootstrapped did not run")
		}
		ctx := context.Background()

		tasks, err := s.runningTasks(ctx, s.pdService)
		if err != nil {
			t.Fatalf("list PD tasks: %v", err)
		}
		if len(tasks) != s.pdCount {
			t.Fatalf("%d PD tasks running, want %d", len(tasks), s.pdCount)
		}

		ids := map[uint64][]string{}
		memberSets := map[string][]string{}
		for _, task := range tasks {
			arn := aws.ToString(task.TaskArn)

			var info pdClusterInfo
			if err := s.pd.get(ctx, arn, "/pd/api/v1/cluster", &info); err != nil {
				t.Fatalf("%s: get cluster: %v", taskID(task), err)
			}
			ids[info.ID] = append(ids[info.ID], taskID(task))

			var members pdMembers
			if err := s.pd.get(ctx, arn, "/pd/api/v1/members", &members); err != nil {
				t.Fatalf("%s: get members: %v", taskID(task), err)
			}
			key := strings.Join(sorted(members.names()), ",")
			memberSets[key] = append(memberSets[key], taskID(task))
		}

		if len(ids) != 1 {
			t.Errorf("PD replicas disagree on the cluster id — split brain: %v", ids)
		}
		if _, ok := ids[clusterID]; !ok {
			t.Errorf("cluster id changed from %d to %v", clusterID, ids)
		}
		if len(memberSets) != 1 {
			t.Errorf("PD replicas disagree on the member list: %v", memberSets)
		}
		for set := range memberSets {
			t.Logf("all %d replicas agree: cluster %d, members %s", len(tasks), clusterID, set)
		}
	})

	t.Run("StoresUp", func(t *testing.T) {
		ctx := context.Background()

		// A store is registered as soon as tikv-server reaches PD, but takes a
		// moment longer to report Up.
		eventually(t, "all TiKV stores Up", 10*time.Minute, 10*time.Second, func(ctx context.Context) error {
			var stores pdStores
			if err := s.pdGet(ctx, "/pd/api/v1/stores", &stores); err != nil {
				return err
			}
			if stores.Count != s.tikvCount {
				return fmt.Errorf("%d stores known to PD, want %d", stores.Count, s.tikvCount)
			}
			for _, entry := range stores.Stores {
				if entry.Store.StateName != "Up" {
					return fmt.Errorf("store %d (%s) is %s", entry.Store.ID, entry.Store.Address, entry.Store.StateName)
				}
			}
			return nil
		})

		// The store addresses come from --advertise-addr, which the script
		// derived with ifaddr(); they must be the tasks' real addresses.
		var stores pdStores
		if err := s.pdGet(ctx, "/pd/api/v1/stores", &stores); err != nil {
			t.Fatalf("get stores: %v", err)
		}
		var addrs []string
		for _, entry := range stores.Stores {
			addrs = append(addrs, entry.Store.Address)
		}

		instances, err := s.discover(ctx, s.tikvDiscover)
		if err != nil {
			t.Fatalf("DiscoverInstances(%s): %v", s.tikvDiscover, err)
		}
		var want []string
		for _, ip := range instanceIPs(instances) {
			want = append(want, ip+":20160")
		}
		if !sameSet(addrs, want) {
			t.Errorf("PD knows stores at %v, running tasks are at %v", sorted(addrs), sorted(want))
		}
		t.Logf("stores %v", sorted(addrs))
	})

	// Stores being Up only proves they registered. Replicated regions prove the
	// Raft groups actually formed across them, which is as close to a data-plane
	// check as we get without pulling in a TiKV client.
	t.Run("RegionsReplicated", func(t *testing.T) {
		eventually(t, "regions replicated across all stores", 10*time.Minute, 10*time.Second,
			func(ctx context.Context) error {
				var info pdClusterInfo
				if err := s.pdGet(ctx, "/pd/api/v1/cluster", &info); err != nil {
					return err
				}
				replicas := info.MaxPeerCount
				if replicas == 0 {
					replicas = 3
				}

				var regions pdRegions
				if err := s.pdGet(ctx, "/pd/api/v1/regions", &regions); err != nil {
					return err
				}
				if regions.Count == 0 {
					return errors.New("PD knows no regions yet")
				}
				for _, r := range regions.Regions {
					if len(r.Peers) != replicas {
						return fmt.Errorf("region %d has %d peers, want %d", r.ID, len(r.Peers), replicas)
					}
					if r.Leader.StoreID == 0 {
						return fmt.Errorf("region %d has no leader", r.ID)
					}
				}
				return nil
			})
	})

	// The lease is short-lived by design, so its absence is fine; what must not
	// happen is it holding an address that never belonged to a PD task.
	t.Run("SeedLease", func(t *testing.T) {
		ctx := context.Background()

		out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
			TableName:      aws.String(s.kvTable),
			Key:            map[string]ddbtypes.AttributeValue{"pk": &ddbtypes.AttributeValueMemberS{Value: seedKey}},
			ConsistentRead: aws.Bool(true),
		})
		if err != nil {
			t.Fatalf("get %s from %s: %v", seedKey, s.kvTable, err)
		}
		if out.Item == nil {
			t.Logf("seed lease %q already released or expired, as expected after bootstrap", seedKey)
			return
		}

		val, ok := out.Item["val"].(*ddbtypes.AttributeValueMemberS)
		if !ok {
			t.Fatalf("seed lease %q has no string value: %#v", seedKey, out.Item)
		}

		instances, err := s.discover(ctx, s.pdDiscovery)
		if err != nil {
			t.Fatalf("DiscoverInstances(%s): %v", s.pdDiscovery, err)
		}
		if !contains(instanceIPs(instances), val.Value) {
			t.Errorf("seed lease is held by %q, which is not a running PD task (%v)",
				val.Value, sorted(instanceIPs(instances)))
		} else {
			t.Logf("seed lease held by %s", val.Value)
		}
	})

	// The part that only an end-to-end test can cover: kill a PD replica and
	// watch muster's replacement pick the "join the existing cluster" branch
	// instead of bootstrapping a second one.
	t.Run("PDReplacementRejoins", func(t *testing.T) {
		if testing.Short() {
			t.Skip("-short: skipping the PD replacement check")
		}
		if clusterID == 0 {
			t.Skip("cluster id unknown; PDClusterBootstrapped did not run")
		}
		ctx := context.Background()

		tasks, err := s.listTasks(ctx, s.pdService)
		if err != nil {
			t.Fatalf("list PD tasks: %v", err)
		}
		if len(tasks) == 0 {
			t.Fatal("no PD tasks to stop")
		}
		victim := tasks[0]
		victimIP := taskIP(victim)
		victimMember := memberName(victimIP)
		victimARN := aws.ToString(victim.TaskArn)
		t.Logf("stopping PD task %s (%s, member %s)", taskID(victim), victimIP, victimMember)

		if _, err := s.ecs.StopTask(ctx, &ecs.StopTaskInput{
			Cluster: aws.String(s.cluster),
			Task:    aws.String(victimARN),
			Reason:  aws.String("muster e2e: PD replacement check"),
		}); err != nil {
			t.Fatalf("stop task: %v", err)
		}

		eventually(t, "PD service back to steady state without "+taskID(victim), 20*time.Minute, 15*time.Second,
			func(ctx context.Context) error {
				tasks, err := s.listTasks(ctx, s.pdService)
				if err != nil {
					return err
				}
				for _, task := range tasks {
					if aws.ToString(task.TaskArn) == victimARN {
						return fmt.Errorf("task %s is still %s", taskID(task), aws.ToString(task.LastStatus))
					}
				}
				if err := s.serviceSteady(ctx, s.pdService, s.pdCount); err != nil {
					return err
				}
				return s.tasksHealthy(ctx, s.pdService, s.pdCount)
			})

		// The replacement joined rather than bootstrapped: same cluster.
		var info pdClusterInfo
		if err := s.pdGet(ctx, "/pd/api/v1/cluster", &info); err != nil {
			t.Fatalf("get cluster: %v", err)
		}
		if info.ID != clusterID {
			t.Fatalf("cluster id changed from %d to %d: the replacement bootstrapped a new cluster", clusterID, info.ID)
		}

		// pre_stop removed the old member on the way out, so the Raft group is
		// back to its original size rather than carrying a dead entry.
		eventually(t, "PD membership back to "+fmt.Sprint(s.pdCount), 10*time.Minute, 10*time.Second,
			func(ctx context.Context) error {
				var members pdMembers
				if err := s.pdGet(ctx, "/pd/api/v1/members", &members); err != nil {
					return err
				}
				if contains(members.names(), victimMember) {
					return fmt.Errorf("stopped member %s is still in the cluster: %v", victimMember, sorted(members.names()))
				}
				if len(members.Members) != s.pdCount {
					return fmt.Errorf("PD has %d members (%v), want %d", len(members.Members), sorted(members.names()), s.pdCount)
				}
				if members.Leader.Name == "" {
					return errors.New("PD has no leader")
				}
				return nil
			})

		// Losing and replacing a PD must not have disturbed the stores.
		var stores pdStores
		if err := s.pdGet(ctx, "/pd/api/v1/stores", &stores); err != nil {
			t.Fatalf("get stores: %v", err)
		}
		if stores.Count != s.tikvCount {
			t.Errorf("%d stores after the PD replacement, want %d", stores.Count, s.tikvCount)
		}
		for _, entry := range stores.Stores {
			if entry.Store.StateName != "Up" {
				t.Errorf("store %d (%s) is %s after the PD replacement",
					entry.Store.ID, entry.Store.Address, entry.Store.StateName)
			}
		}
	})
}

// --- small helpers ---------------------------------------------------------

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func duplicates(in []string) []string {
	seen, dupes := map[string]bool{}, []string(nil)
	for _, s := range in {
		if seen[s] {
			dupes = append(dupes, s)
		}
		seen[s] = true
	}
	return dupes
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sa, sb := sorted(a), sorted(b)
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}
