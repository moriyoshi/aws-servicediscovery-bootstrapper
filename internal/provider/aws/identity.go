package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/moriyoshi/muster/internal/provider"
)

// taskMetadataV4 is the subset of the ECS task metadata document muster reads.
type taskMetadataV4 struct {
	Cluster          string `json:"Cluster"`
	ServiceName      string `json:"ServiceName"`
	VPCID            string `json:"VPCID"`
	TaskARN          string `json:"TaskARN"`
	Family           string `json:"Family"`
	Revision         string `json:"Revision"`
	AvailabilityZone string `json:"AvailabilityZone"`
	CreatedAt        string `json:"CreatedAt"`

	// Containers carries the task's own addresses. Under awsvpc every container
	// shares one ENI, so the first network of the first container is the task's
	// address.
	Containers []struct {
		Networks []struct {
			IPv4Addresses []string `json:"IPv4Addresses"`
			IPv6Addresses []string `json:"IPv6Addresses"`
		} `json:"Networks"`
	} `json:"Containers"`
}

// addresses returns the task's own IPv4 and IPv6 addresses, either empty when
// the metadata document does not carry them.
func (m *taskMetadataV4) addresses() (string, string) {
	for _, c := range m.Containers {
		for _, n := range c.Networks {
			var v4, v6 string
			if len(n.IPv4Addresses) > 0 {
				v4 = n.IPv4Addresses[0]
			}
			if len(n.IPv6Addresses) > 0 {
				v6 = n.IPv6Addresses[0]
			}
			if v4 != "" || v6 != "" {
				return v4, v6
			}
		}
	}
	return "", ""
}

// metadataURIEnv lists, in preference order, the environment variables ECS uses
// to advertise the task metadata endpoint. The name is ECS_-prefixed: the
// AWS_CONTAINER_* variables are the credentials ones, and nothing ever sets
// AWS_CONTAINER_METADATA_URI_V4 — reading it meant metadata was silently
// unavailable on every ECS task, taking the identity global and the
// no-argument form of the replica-status builtin down with it.
var metadataURIEnv = []string{
	"ECS_CONTAINER_METADATA_URI_V4", // Fargate platform 1.4.0+, EC2 agent 1.39+
	"ECS_CONTAINER_METADATA_URI",    // v3, for older agents
}

// MetadataURIEnv is the same list, exposed so provider autodetection can key off
// the presence of these variables without dialling anything.
func MetadataURIEnv() []string { return metadataURIEnv }

func metadataURI() string {
	for _, name := range metadataURIEnv {
		if uri := os.Getenv(name); uri != "" {
			return uri
		}
	}
	return ""
}

func fetchContainerMetadata(ctx context.Context) (*taskMetadataV4, error) {
	uri := metadataURI()
	if uri == "" {
		return nil, fmt.Errorf("none of %s is set; not running under ECS?", strings.Join(metadataURIEnv, ", "))
	}
	req, err := http.NewRequest(http.MethodGet, uri+"/task", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build a request: %w", err)
	}
	req = req.WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch container metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch container metadata: %s", resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	metadata := new(taskMetadataV4)
	err = json.Unmarshal(b, metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal container metadata: %w", err)
	}
	// CreatedAt is a per-container field, exposed at the container root endpoint
	// rather than /task. Fetch it best-effort so scripts can use it (e.g. as a
	// deterministic tie-break for seed election).
	if metadata.CreatedAt == "" {
		if creq, cerr := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil); cerr == nil {
			if cresp, cerr := http.DefaultClient.Do(creq); cerr == nil {
				defer cresp.Body.Close()
				if cb, cerr := io.ReadAll(cresp.Body); cerr == nil {
					var container struct {
						CreatedAt string `json:"CreatedAt"`
					}
					if json.Unmarshal(cb, &container) == nil {
						metadata.CreatedAt = container.CreatedAt
					}
				}
			}
		}
	}
	return metadata, nil
}

// regionFromTaskARN pulls the region out of an ECS task ARN
// (arn:aws:ecs:<region>:<account>:task/<cluster>/<id>). Returns "" for anything
// that is not shaped like one, which is the same as not knowing.
func regionFromTaskARN(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 4 || parts[0] != "arn" {
		return ""
	}
	return parts[3]
}

// FetchIdentity reads the ECS task metadata endpoint and maps it onto the
// neutral Identity.
//
// Name is deliberately left empty: a Fargate task gets a fresh id and address
// every time it is replaced, so it has no identity stable enough to persist as
// a cluster member name. Scripts read that emptiness as "derive a name some
// other way" rather than being handed one that silently changes.
func FetchIdentity(ctx context.Context) (*provider.Identity, error) {
	m, err := fetchContainerMetadata(ctx)
	if err != nil {
		return nil, err
	}
	ipv4, ipv6 := m.addresses()
	return &provider.Identity{
		Provider:  Name,
		ID:        m.TaskARN,
		Group:     m.Cluster,
		Service:   m.ServiceName,
		Zone:      m.AvailabilityZone,
		Region:    regionFromTaskARN(m.TaskARN),
		Network:   m.VPCID,
		IPv4:      ipv4,
		IPv6:      ipv6,
		CreatedAt: m.CreatedAt,
		Extra: map[string]string{
			"task_arn": m.TaskARN,
			"cluster":  m.Cluster,
			"family":   m.Family,
			"revision": m.Revision,
			"vpc_id":   m.VPCID,
		},
	}, nil
}
