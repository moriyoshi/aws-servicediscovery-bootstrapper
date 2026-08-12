// Package gcp holds muster's TiKV-on-Cloud-Run end-to-end test.
//
// The test provisions real Google Cloud infrastructure and asserts that muster
// brings a three-member PD Raft group and three TiKV stores up from nothing, so
// it is behind the e2e_tikv_gcp build tag and does nothing without
// MUSTER_E2E_TIKV_GCP=1. See README.md for what it costs and how to run it.
package gcp
