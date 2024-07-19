// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build !plan9

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// apiVersion: tailscale.com/v1alpha1
// kind: TSRecorder
// metadata:
//   name: ssh-recorder
// spec:
//   image
//      repo // defaults to tailscale/tsrecorder
//     tag string // defaults to the version of operator that deploys this
//     digest string
//   backends:
//     s3:
//       bucket string // bucket name
//       credsFrom
//         secret
//           name string // name of a Secret in TSRecorder’s namespace
//           accessKeyFromField // Secret field to read access key from, defaults to access_key
//          secretKeyFromField // Secret field to read secret key from, default to secret_key
//     disk:
//       path : string // directory path to write recording files to
//   hostname string // Tailscale hostname for this recorder node
//   tags []string // list of tags to assign to the recorder node
//   enableUI bool // run tsrecorder UI (expose over tailscale maybe?)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=rec
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=`.status.conditions[?(@.type == "RecorderReady")].reason`,description="Status of the deployed TSRecorder resources."

type TSRecorder struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec describes the desired recorder instance.
	Spec TSRecorderSpec `json:"spec"`

	// TSRecorderStatus describes the status of the recorder. This is set
	// and managed by the Tailscale operator.
	// +optional
	Status TSRecorderStatus `json:"status"`
}

// +kubebuilder:object:root=true

type TSRecorderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []TSRecorder `json:"items"`
}

type TSRecorderSpec struct {
	// Tags that the Tailscale node will be tagged with.
	// Defaults to [tag:k8s].
	// To autoapprove the subnet routes or exit node defined by a Connector,
	// you can configure Tailscale ACLs to give these tags the necessary
	// permissions.
	// See https://tailscale.com/kb/1018/acls/#auto-approvers-for-routes-and-exit-nodes.
	// If you specify custom tags here, you must also make the operator an owner of these tags.
	// See  https://tailscale.com/kb/1236/kubernetes-operator/#setting-up-the-kubernetes-operator.
	// Tags cannot be changed once a TSRecorder node has been created.
	// Tag values must be in form ^tag:[a-zA-Z][a-zA-Z0-9-]*$.
	// +optional
	Tags Tags `json:"tags,omitempty"`
	// Hostname is the tailnet hostname that should be assigned to the
	// TSRecorder node. If unset, hostname defaults to <recorder name>-connector.
	// Hostname can contain lower case letters, numbers and dashes, it must not
	// start or end with a dash and must be between 2 and 63 characters long.
	// +optional
	Hostname Hostname `json:"hostname,omitempty"`

	Image Image `json:"image,omitempty"`

	// EnableUI switches on the UI for the recorder instance.
	EnableUI bool `json:"enableUI,omitempty"`

	Replicas int32 `json:"replicas,omitempty"`

	ExtraVolumes []corev1.Volume `json:"extraVolumes,omitempty"`

	ExtraVolumeMounts []corev1.VolumeMount `json:"extraVolumeMounts,omitempty"`

	Backends Backends `json:"backends"`
	//   backends:
	//     s3:
	//       bucket string // bucket name
	//       credsFrom
	//         secret
	//           name string // name of a Secret in TSRecorder’s namespace
	//           accessKeyFromField // Secret field to read access key from, defaults to access_key
	//          secretKeyFromField // Secret field to read secret key from, default to secret_key
	//     disk:
	//       path : string // directory path to write recording files to
}

type Backends struct {
	// +optional
	S3 S3Backend `json:"s3,omitempty"`
	// +optional
	Disk DiskBackend `json:"disk,omitempty"`
}

// S3Backend configures an S3 backend for writing recordings to.
type S3Backend struct {
	// Bucket is the S3 bucket name.
	Bucket string `json:"bucket"`
	// CredsSecret S3CredsSecret
}

// DiskBackend configures a disk backend for writing recordings to.
type DiskBackend struct {
	// Path specifies the path on disk to write recordings to.
	// +optional
	Path string `json:"path,omitempty"`
}

type TSRecorderStatus struct {
	// List of status conditions to indicate the status of the TSRecorder.
	// Known condition types are `RecorderReady`.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions"`
	// TailnetIPs is the set of tailnet IP addresses (both IPv4 and IPv6)
	// assigned to the TSRecorder.
	// +optional
	TailnetIPs []string `json:"tailnetIPs,omitempty"`
	// Hostname is the fully qualified domain name of the TSRecorder.
	// If MagicDNS is enabled in your tailnet, it is the MagicDNS name of the
	// node.
	// +optional
	Hostname string `json:"hostname,omitempty"`
}
