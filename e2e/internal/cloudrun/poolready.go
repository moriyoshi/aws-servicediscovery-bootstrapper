// Package cloudrun reads Cloud Run resource state as `gcloud run` renders it.
//
// It exists so that the parsing can be tested without a project. The check it
// replaced could not be, and was wrong: it read the Cloud Run v2 API's field
// names out of gcloud's output, which uses the Knative ones, so it decoded an
// empty struct and reported an empty condition rather than a mismatch.
package cloudrun

import (
	"encoding/json"
	"fmt"
)

// pool is a worker pool as `gcloud run worker-pools describe --format=json`
// renders it: the Knative shape, whatever the surface underneath.
//
// The v2 API answers the same question with a flat `terminalCondition` object
// whose `state` is CONDITION_SUCCEEDED, and the Terraform provider surfaces
// that spelling too -- so a reader who has just been looking at the state file
// will reach for the wrong names, which is exactly how this went wrong.
type pool struct {
	Metadata struct {
		Generation int `json:"generation"`
	} `json:"metadata"`
	Status struct {
		ObservedGeneration int    `json:"observedGeneration"`
		LatestCreated      string `json:"latestCreatedRevisionName"`
		LatestReady        string `json:"latestReadyRevisionName"`
		Conditions         []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

// ReadyRevision names the revision a pool has settled on, given the same
// describe output. Assertions scope their log reads to it: a worker pool's logs
// outlive the instances that wrote them, so a window wide enough to be useful
// also contains replicas from revisions that are gone. Their reports describe a
// different cluster, and counting them turns a healthy pool into a split brain.
func ReadyRevision(raw []byte) (string, error) {
	var p pool
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", fmt.Errorf("parsing describe output: %w", err)
	}
	if p.Status.LatestReady == "" {
		return "", fmt.Errorf("no ready revision in the describe output")
	}
	return p.Status.LatestReady, nil
}

// CreatedRevision names the revision a pool most recently created, which after
// an unpromoted deploy is the one carrying no instances yet.
func CreatedRevision(raw []byte) (string, error) {
	var p pool
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", fmt.Errorf("parsing describe output: %w", err)
	}
	if p.Status.LatestCreated == "" {
		return "", fmt.Errorf("no created revision in the describe output")
	}
	return p.Status.LatestCreated, nil
}

// PoolReady reports whether a worker pool has reconciled onto a ready revision,
// given the JSON `gcloud run worker-pools describe` produced for it. A worker
// pool serves no requests, so this is the only signal that its instances were
// started rather than merely asked for.
//
// A nil error means ready. Every other outcome is an error that names what is
// wrong, including "these are not the fields I expected" -- a readiness check
// that cannot find its own field has to say so, because an empty value reads as
// a workload problem and sends you to the wrong logs.
func PoolReady(name string, raw []byte) error {
	var p pool
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("%s: parsing describe output: %w", name, err)
	}

	// Knative's stand-in for the v2 API's `reconciling`: the controller has not
	// caught up with the spec, so the conditions below describe the previous one.
	if p.Status.ObservedGeneration != p.Metadata.Generation {
		return fmt.Errorf("%s is still reconciling: spec generation %d, observed %d",
			name, p.Metadata.Generation, p.Status.ObservedGeneration)
	}
	if p.Status.LatestCreated != p.Status.LatestReady {
		return fmt.Errorf("%s created revision %q but its ready revision is %q",
			name, p.Status.LatestCreated, p.Status.LatestReady)
	}
	for _, c := range p.Status.Conditions {
		if c.Type != "Ready" {
			continue
		}
		if c.Status == "True" {
			return nil
		}
		return fmt.Errorf("%s is not ready (Ready=%s): %s %s", name, c.Status, c.Reason, c.Message)
	}
	return fmt.Errorf("%s reports no Ready condition at all, in %d conditions -- "+
		"gcloud's output shape is not the one this reads (Knative: status.conditions[].type)",
		name, len(p.Status.Conditions))
}
