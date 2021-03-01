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

package controllers

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecr"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awsv1 "github.com/jcleira/ecr-credentials-controller/api/v1"
)

const (
	dockerConfigJSONType = "kubernetes.io/dockerconfigjson"
	dockerConfigJSONKey  = ".dockerconfigjson"

	dockerCredentialsSecretName = "docker-registry"
)

// clock knows how to get the current time.
// It can be used to fake out timing for testing.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (_ realClock) Now() time.Time { return time.Now() }

// RegistryReconciler reconciles a Registry object
type RegistryReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme

	Clock
}

var (
	scheduledTimeAnnotation = "ecr-credentials-controller/scheduled-at"
)

// +kubebuilder:rbac:groups=aws.com.ederium.ecr-credentials-controller,resources=registries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=aws.com.ederium.ecr-credentials-controller,resources=registries/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get
// +kubebuilder:rbac:groups=,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=,resources=pods,verbs=get;list;watch;create;update;patch;delete

func (r *RegistryReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("registry", req.NamespacedName)

	var registry awsv1.Registry
	if err := r.Get(ctx, req.NamespacedName, &registry); err != nil {
		log.Error(err, "unable to fetch Registry")

		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.V(1).Info("01")

	var childJobs batchv1.JobList
	if err := r.List(ctx,
		&childJobs,
		client.InNamespace(req.Namespace),
		client.MatchingFields{".metadata.controller": req.Name}); err != nil {
		log.Error(err, "unable to list child Jobs")
		return ctrl.Result{}, err
	}

	log.V(1).Info("02", "childjobs", childJobs)

	isJobFinished := func(job *batchv1.Job) (bool, batchv1.JobConditionType) {
		for _, c := range job.Status.Conditions {
			if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) &&
				c.Status == corev1.ConditionTrue {
				return true, c.Type
			}
		}

		return false, ""
	}

	getScheduledTimeForJob := func(job *batchv1.Job) (*time.Time, error) {
		timeRaw := job.Annotations[scheduledTimeAnnotation]
		if len(timeRaw) == 0 {
			return nil, nil
		}

		timeParsed, err := time.Parse(time.RFC3339, timeRaw)
		if err != nil {
			return nil, err
		}
		return &timeParsed, nil
	}

	var activeJobs, successfulJobs, failedJobs []*batchv1.Job
	var mostRecentTime *time.Time

	for i, job := range childJobs.Items {
		_, finishedType := isJobFinished(&job)

		log.V(1).Info("0201", "finishedType", finishedType)

		switch finishedType {
		case "":
			activeJobs = append(activeJobs, &childJobs.Items[i])
		case batchv1.JobFailed:
			failedJobs = append(failedJobs, &childJobs.Items[i])
		case batchv1.JobComplete:
			successfulJobs = append(successfulJobs, &childJobs.Items[i])
		}

		scheduledTimeForJob, err := getScheduledTimeForJob(&job)
		if err != nil {
			log.Error(err, "unable to parse schedule time for child job", "job", &job)
			continue
		}

		if scheduledTimeForJob != nil {
			if mostRecentTime == nil {
				mostRecentTime = scheduledTimeForJob
			} else if mostRecentTime.Before(*scheduledTimeForJob) {
				mostRecentTime = scheduledTimeForJob
			}
		}
	}

	log.V(1).Info("03", "activeJobs", activeJobs)
	log.V(1).Info("0301", "successfulJobs", successfulJobs)
	log.V(1).Info("0302", "failedJobs", failedJobs)

	if mostRecentTime != nil {
		log.V(1).Info("0303", "mostRecentTime", mostRecentTime)
		registry.Status.LastRefreshTime = &metav1.Time{Time: *mostRecentTime}
	} else {
		registry.Status.LastRefreshTime = nil
	}

	TenHoursAgo := time.Now().Add(-time.Hour * time.Duration(10))

	registry.Status.Valid = false
	if registry.Status.LastRefreshTime != nil &&
		registry.Status.LastRefreshTime.Time.Before(TenHoursAgo) {
		registry.Status.Valid = true
	}

	log.V(1).Info("04", "registry.Status", registry.Status)

	if err := r.Status().Update(ctx, &registry); err != nil {
		log.Error(err, "unable to update Registry status")
		return ctrl.Result{}, err
	}

	// Deleting old failed jobs, leaving the last one as reference.
	if len(failedJobs) > 1 {
		sort.Slice(failedJobs, func(i, j int) bool {
			if failedJobs[i].Status.StartTime == nil {
				return failedJobs[j].Status.StartTime != nil
			}

			return failedJobs[i].Status.StartTime.Before(
				failedJobs[j].Status.StartTime,
			)
		})

		for i, job := range failedJobs {
			if i+1 == len(failedJobs) {
				break
			}

			err := r.Delete(ctx, job,
				client.PropagationPolicy(metav1.DeletePropagationBackground))
			if err != nil {
				log.Error(err, "unable to delete old failed job", "job", job)
				continue
			}

			log.V(0).Info("deleted old failed job", "job", job)
		}
	}

	log.V(1).Info("05", "failedJobs", failedJobs)

	// Deleting old jobs, leaving the last one as reference.
	if len(successfulJobs) > 1 {
		sort.Slice(successfulJobs, func(i, j int) bool {
			if successfulJobs[i].Status.StartTime == nil {
				return successfulJobs[j].Status.StartTime != nil
			}

			return successfulJobs[i].Status.StartTime.Before(
				successfulJobs[j].Status.StartTime,
			)
		})

		for i, job := range successfulJobs {
			if i+1 == len(successfulJobs) {
				break
			}

			err := r.Delete(ctx, job,
				client.PropagationPolicy(metav1.DeletePropagationBackground))
			if err != nil {
				log.Error(err, "unable to delete old successful job", "job", job)
				continue
			}

			log.V(0).Info("deleted old successful job", "job", job)
		}
	}

	log.V(1).Info("06", "successfulJobs", successfulJobs)

	getNextSchedule := func(registry awsv1.Registry, now time.Time) time.Time {
		if registry.Status.LastRefreshTime != nil {
			return registry.Status.LastRefreshTime.Time.Add(time.Hour * 10)
		}

		return registry.ObjectMeta.CreationTimestamp.Time
	}

	nextSchedule := getNextSchedule(registry, r.Now())

	log.V(1).Info("07", "nextSchedule", nextSchedule)

	if nextSchedule.After(r.Now()) {
		return ctrl.Result{RequeueAfter: nextSchedule.Sub(r.Now())}, nil
	}

	secret := &corev1.Secret{}
	err := r.Client.Get(ctx, types.NamespacedName{
		Name:      dockerCredentialsSecretName,
		Namespace: registry.Namespace,
	}, secret)
	if err != nil {
		// We check for "Object not found". This can happens if it's a first-time
		// initialization, if the error is different than IsNotFound we fail, but
		// we will continue (and log) if we don't find the secret.
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}

		log.V(1).Info("docker-credentials secret not found, it could be a first-time registry")
	}

	// We do this comparision to check if the docker credentials secret was found.
	if secret.Name == dockerCredentialsSecretName {
		err = r.Delete(ctx, secret,
			client.PropagationPolicy(metav1.DeletePropagationBackground))
		if err != nil {
			log.Error(err, "unable to delete old auth secret", "secret", secret)
			return ctrl.Result{}, err
		}
	}

	/*
		sess, err := session.NewSession(&aws.Config{
			Region: aws.String("eu-west-1"),
			Credentials: credentials.NewStaticCredentials(
				"AKID",
				"SECRET_KEY",
				"TOKEN"),
		})
	*/

	svc := ecr.New(
		session.New(&aws.Config{
			Region: aws.String("eu-west-1"),
		}),
	)

	ecrAuth, err := svc.GetAuthorizationToken(
		&ecr.GetAuthorizationTokenInput{},
	)
	if err != nil {
		log.Error(err, "unable to get AWS ECR auth token", "ecrAuth", ecrAuth)
		return ctrl.Result{}, err
	}

	if len(ecrAuth.AuthorizationData) == 0 {
		log.Error(err, "AWS ECR auth response doesn't contain auth data")
		return ctrl.Result{}, err
	}

	if ecrAuth.AuthorizationData[0].AuthorizationToken == nil {
		log.Error(err, "AWS ECR auth response has a nill authorization token")
		return ctrl.Result{}, err
	}

	dockerConfigJSON := map[string]interface{}{
		"auths": map[string]interface{}{
			"https://index.docker.io/v1/": map[string]interface{}{
				"auth": *ecrAuth.AuthorizationData[0].AuthorizationToken,
			},
		},
	}

	dockerConfigJSONBytes, err := json.Marshal(dockerConfigJSON)
	if err != nil {
		log.Error(err, "unable to json.Marshal on docker config JSON")
		return ctrl.Result{}, err
	}

	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dockerCredentialsSecretName,
			Namespace: registry.Namespace,
		},
		Type: dockerConfigJSONType,
		Data: map[string][]byte{dockerConfigJSONKey: dockerConfigJSONBytes},
	}
	if err := r.Create(ctx, secret); err != nil {
		log.Error(err, "unable to create docker registry secret", "secret", secret)
		return ctrl.Result{}, err
	}

	log.V(1).Info("09", "secret created", secret)

	return ctrl.Result{}, nil
}

func int32Ptr(i int32) *int32 { return &i }
func int64Ptr(i int64) *int64 { return &i }

var (
	jobOwnerKey = ".metadata.controller"
	apiGVStr    = awsv1.GroupVersion.String()
)

func (r *RegistryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Clock == nil {
		r.Clock = realClock{}
	}

	if err := mgr.GetFieldIndexer().IndexField(
		&batchv1.Job{}, jobOwnerKey, func(rawObj runtime.Object) []string {
			job := rawObj.(*batchv1.Job)
			owner := metav1.GetControllerOf(job)
			if owner == nil {
				return nil
			}

			if owner.APIVersion != apiGVStr || owner.Kind != "Registry" {
				return nil
			}

			return []string{owner.Name}
		}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&awsv1.Registry{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
