/*
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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RegistrySpec defines the desired state of Registry
type RegistrySpec struct {
	// AWS zone of the ECR Registry to refresh credentials.
	AWSZone string `json:"awsZone,omitempty"`

	// AWSAccountSecret secret that contains the AWS account credentials.
	AWSAccountSecret string `json:"awsAccountSecret,omitempty"`
}

// RegistryStatus defines the observed state of Registry
type RegistryStatus struct {
	Status bool `json:"active"`
}

// +kubebuilder:object:root=true

// Registry is the Schema for the registries API
type Registry struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RegistrySpec   `json:"spec,omitempty"`
	Status RegistryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RegistryList contains a list of Registry
type RegistryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Registry `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Registry{}, &RegistryList{})
}
