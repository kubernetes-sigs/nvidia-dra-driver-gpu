/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"encoding/json"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager/checksum"
)

// CheckpointedDevice shares kubeletplugin.Device's layout, i.e.
// kubeletplugin.Device(c) can be done as a no-op cast. We can't serialize
// `kubeletplugin.Device` directly anymore for creating a checkpoint or for
// deserializing from a checkpoint: a field added upstream to `Device` without
// `omitempty` causes re-serialization of an older checkpoint to include that
// field with an empty value. That changes the newly computed checksum compared
// to the one encoded in the checkpoint, resulting in the old checkpoint being
// rejected by the new binary. Circumvent that by adding the `omitempty`
// annotations here for `ShareID` and `Metadata` which were added as part of the
// k8s 1.36 release cycle. Also see issue 1080.
type CheckpointedDevice kubeletplugin.Device

func (c CheckpointedDevice) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Requests     []string                      `json:"Requests"`
		PoolName     string                        `json:"PoolName"`
		DeviceName   string                        `json:"DeviceName"`
		CDIDeviceIDs []string                      `json:"CDIDeviceIDs"`
		ShareID      *types.UID                    `json:"ShareID,omitempty"`
		Metadata     *kubeletplugin.DeviceMetadata `json:"Metadata,omitempty"`
	}{
		Requests:     c.Requests,
		PoolName:     c.PoolName,
		DeviceName:   c.DeviceName,
		CDIDeviceIDs: c.CDIDeviceIDs,
		ShareID:      c.ShareID,
		Metadata:     c.Metadata,
	})
}

type ClaimCheckpointState string

const (
	ClaimCheckpointStateUnset            ClaimCheckpointState = ""
	ClaimCheckpointStatePrepareStarted   ClaimCheckpointState = "PrepareStarted"
	ClaimCheckpointStatePrepareCompleted ClaimCheckpointState = "PrepareCompleted"
)

// Latest version type aliases

type PreparedClaimsByUID = PreparedClaimsByUIDV2
type PreparedClaim = PreparedClaimV2

// V2 types

type CheckpointV2 struct {
	Checksum       checksum.Checksum     `json:"checksum"`
	PreparedClaims PreparedClaimsByUIDV2 `json:"preparedClaims,omitempty"`
	// NodeBootID is the Linux kernel boot_id
	// for the node when this checkpoint was last validated at plugin startup.
	// If it differs from the current boot id, prepared claims are invalid (reboot).
	NodeBootID string `json:"nodeBootID,omitempty"`
}

type PreparedClaimsByUIDV2 map[string]PreparedClaimV2

type PreparedClaimV2 struct {
	CheckpointState ClaimCheckpointState            `json:"checkpointState"`
	Status          resourceapi.ResourceClaimStatus `json:"status,omitempty"`
	PreparedDevices PreparedDevices                 `json:"preparedDevices,omitempty"`
	Name            string                          `json:"name,omitempty"`
	Namespace       string                          `json:"namespace,omitempty"`
}

// V1 types

type CheckpointV1 struct {
	PreparedClaims PreparedClaimsByUIDV1 `json:"preparedClaims,omitempty"`
}

type PreparedClaimsByUIDV1 map[string]PreparedClaimV1

type PreparedClaimV1 struct {
	Status          resourceapi.ResourceClaimStatus `json:"status,omitempty"`
	PreparedDevices PreparedDevices                 `json:"preparedDevices,omitempty"`
}

// Conversion functions

func (v1 *CheckpointV1) ToV2() *CheckpointV2 {
	v2 := &CheckpointV2{
		PreparedClaims: make(PreparedClaimsByUIDV2),
	}
	for claimUID, v1Claim := range v1.PreparedClaims {
		v2.PreparedClaims[claimUID] = PreparedClaimV2{
			CheckpointState: ClaimCheckpointStatePrepareCompleted,
			Status:          ownedStatus(v1Claim.Status),
			PreparedDevices: v1Claim.PreparedDevices,
		}
	}
	return v2
}

func (v2 *CheckpointV2) ToV1() *CheckpointV1 {
	v1 := &CheckpointV1{
		PreparedClaims: make(PreparedClaimsByUIDV1),
	}
	for claimUID, v1Claim := range v2.PreparedClaims {
		if v1Claim.CheckpointState != ClaimCheckpointStatePrepareCompleted {
			continue
		}
		v1.PreparedClaims[claimUID] = PreparedClaimV1{
			Status:          v1Claim.Status,
			PreparedDevices: v1Claim.PreparedDevices,
		}
	}
	return v1
}
