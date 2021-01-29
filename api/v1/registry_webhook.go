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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// log is for logging in this package.
var registrylog = logf.Log.WithName("registry-resource")

func (r *Registry) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-aws-com-ederium-ecr-credentials-controller-v1-registry,mutating=true,failurePolicy=fail,groups=aws.com.ederium.ecr-credentials-controller,resources=registries,verbs=create;update,versions=v1,name=mregistry.kb.io

var _ webhook.Defaulter = &Registry{}

// Default implements webhook.Defaulter so a webhook will be registered for the type
func (r *Registry) Default() {
	registrylog.Info("default", "name", r.Name)
}

// +kubebuilder:webhook:verbs=create;update,path=/validate-aws-com-ederium-ecr-credentials-controller-v1-registry,mutating=false,failurePolicy=fail,groups=aws.com.ederium.ecr-credentials-controller,resources=registries,versions=v1,name=vregistry.kb.io

var _ webhook.Validator = &Registry{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *Registry) ValidateCreate() error {
	registrylog.Info("validate create", "name", r.Name)

	errorList := r.ValidateRegistrySpec()
	if len(errorList) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: "aws.com.ederium.ecr-credentials-controller", Kind: "Registry"},
			r.Name, errorList)
	}

	return nil
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *Registry) ValidateUpdate(old runtime.Object) error {
	registrylog.Info("validate update", "name", r.Name)

	errorList := r.ValidateRegistrySpec()
	if len(errorList) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: "aws.com.ederium.ecr-credentials-controller", Kind: "Registry"},
			r.Name, errorList)
	}

	return nil
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *Registry) ValidateDelete() error {
	registrylog.Info("validate delete", "name", r.Name)

	// TODO(user): fill in your validation logic upon object deletion.
	return nil
}

const (
	errorEmptyAWSZOne          = "empty AWS zone"
	errorEmptyAWSAccountSecret = "empty AWS account secret"
)

func (r *Registry) ValidateRegistrySpec() (errorList field.ErrorList) {
	if r.Spec.AWSZone == "" {
		errorList = append(errorList,
			field.Invalid(
				field.NewPath("spec").Child("awsZone"),
				r.Spec.AWSZone, errorEmptyAWSZOne,
			),
		)
	}

	if r.Spec.AWSAccountSecret == "" {
		errorList = append(errorList,
			field.Invalid(
				field.NewPath("spec").Child("awsZone"),
				r.Spec.AWSZone, errorEmptyAWSAccountSecret,
			),
		)
	}

	return errorList
}
