/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
)

const (
	GroupName = "resource.nvidia.com"
	Version   = "v1beta1"

	GpuConfigKind                  = "GpuConfig"
	MigDeviceConfigKind            = "MigDeviceConfig"
	VfioDeviceConfigKind           = "VfioDeviceConfig"
	LocalIPCConfigKind             = "LocalIPCConfig"
	ComputeDomainChannelConfigKind = "ComputeDomainChannelConfig"
	ComputeDomainDaemonConfigKind  = "ComputeDomainDaemonConfig"
	ComputeDomainKind              = "ComputeDomain"
	ComputeDomainCliqueKind        = "ComputeDomainClique"
)

// Interface defines the set of common APIs for all configs
// +k8s:deepcopy-gen=false
type Interface interface {
	Normalize() error
	Validate() error
}

// StrictDecoder implements a decoder for objects in this API group. Fails upon
// unknown fields in the input. Is the preferable choice when processing input
// directly provided by the user (example: opaque config JSON provided in a
// resource claim, validated only in the NodePrepareResources code path when no
// validating webhook is deployed).
var StrictDecoder runtime.Decoder

// NonstrictDecoder implements a decoder for objects in this API group. Silently
// drops unknown fields in the input. Used for deserializing checkpoint data
// (JSON that may have been created by older or newer versions of this driver).
var NonstrictDecoder runtime.Decoder

func init() {
	// Create a new scheme and add our types to it. If at some point in the
	// future a new version of the configuration API becomes necessary, then
	// conversion functions can be generated and registered to continue
	// supporting older versions.
	scheme := runtime.NewScheme()
	schemeGroupVersion := schema.GroupVersion{
		Group:   GroupName,
		Version: Version,
	}
	scheme.AddKnownTypes(schemeGroupVersion,
		&GpuConfig{},
		&MigDeviceConfig{},
		&VfioDeviceConfig{},
		&LocalIPCConfig{},
		&ComputeDomainChannelConfig{},
		&ComputeDomainDaemonConfig{},
		&ComputeDomain{},
		&ComputeDomainClique{},
	)
	metav1.AddToGroupVersion(scheme, schemeGroupVersion)

	// Note: the strictness applies to all types defined above via
	// AddKnownTypes(), i.e. it cannot be set per-type. That is OK in this case.
	// Unknown fields will simply be dropped (ignored) upon decode, which is
	// what we want. This is relevant in a downgrade case, when a checkpointed
	// JSON document contains fields added in a later version (workload defined
	// with a new version of this driver).
	NonstrictDecoder = json.NewSerializerWithOptions(
		json.DefaultMetaFactory,
		scheme,
		scheme,
		json.SerializerOptions{Strict: false},
	)

	StrictDecoder = json.NewSerializerWithOptions(
		json.DefaultMetaFactory,
		scheme,
		scheme,
		json.SerializerOptions{Strict: true},
	)
}
