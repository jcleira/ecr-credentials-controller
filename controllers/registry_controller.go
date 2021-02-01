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
	"fmt"
	"sort"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awsv1 "github.com/jcleira/ecr-credentials-controller/api/v1"
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

func (r *RegistryReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("registry", req.NamespacedName)

	var registry awsv1.Registry
	if err := r.Get(ctx, req.NamespacedName, &registry); err != nil {
		log.Error(err, "unable to fetch Registry")

		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var childJobs batchv1.JobList
	if err := r.List(ctx,
		&childJobs,
		client.InNamespace(req.Namespace),
		client.MatchingFields{".metadata.controller": req.Name}); err != nil {
		log.Error(err, "unable to list child Jobs")
		return ctrl.Result{}, err
	}

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

		switch finishedType {
		case "":
			activeJobs = append(activeJobs, &childJobs.Items[i])
		case batchv1.JobFailed:
			continue
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

	if mostRecentTime != nil {
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

	getNextSchedule := func(registry awsv1.Registry, now time.Time) time.Time {
		if registry.Status.LastRefreshTime != nil {
			return registry.Status.LastRefreshTime.Time.Add(time.Hour * 10)
		}

		return registry.ObjectMeta.CreationTimestamp.Time
	}

	nextSchedule := getNextSchedule(registry, r.Now())

	if nextSchedule.Before(r.Now()) {
		return ctrl.Result{RequeueAfter: nextSchedule.Sub(r.Now())}, nil
	}

	buildJobforRegistry := func(
		registry *awsv1.Registry, scheduledTime time.Time) (*batchv1.Job, error) {
		// We want job names for a given nominal start time to have a
		// deterministic name to avoid the same job being created twice.
		name := fmt.Sprintf("%s-%d", registry.Name, scheduledTime.Unix())

		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      make(map[string]string),
				Annotations: make(map[string]string),
				Name:        name,
				Namespace:   registry.Namespace,
			},
			Spec: batchv1.JobSpec{
				Parallelism:             int32Ptr(1),
				Completions:             int32Ptr(1),
				ActiveDeadlineSeconds:   int64Ptr(5),
				BackoffLimit:            int32Ptr(3),
				TTLSecondsAfterFinished: int32Ptr(500),
				Template: v1.PodTemplateSpec{
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:            "ecr-registry-credentials",
								Image:           "jcorral/awscli-kubectl:latest",
								ImagePullPolicy: "IfNotPresent",
								Command: []string{
									"bin/sh",
									"-c",
									"SECRET_NAME=${AWS_REGION}-ecr-registry-credentials",
									"EMAIL=no@local.info",
									"TOKEN=`aws ecr get-login-password --region ${AWS_REGION}`",
									"kubectl delete secret --ignore-not-found ${SECRET_NAME}",
									`kubectl create secret docker-registry ${SECRET_NAME}
									 --docker-server=https://${AWS_ACCOUNT}.dkr.ecr.${AWS_REGION}.amazonaws.com
									 --docker-username=AWS
									 --docker-password="${TOKEN}"
									 --docker-email="${EMAIL}"
									`,
									`kubectl patch sa default -p '{"imagePullSecrets":[{"name":"'${SECRET_NAME}'"}]}'`,
								},
								Env: []v1.EnvVar{
									{
										Name:  "AWS_REGION",
										Value: "eu-west-1",
									},
									{
										Name: "AWS_ACCOUNT",
										ValueFrom: &v1.EnvVarSource{
											SecretKeyRef: &v1.SecretKeySelector{
												LocalObjectReference: v1.LocalObjectReference{
													Name: "aws-credentials",
												},
												Key: "AWS_ACCOUNT",
											},
										},
									},
									{
										Name: "AWS_ACCESS_KEY_ID",
										ValueFrom: &v1.EnvVarSource{
											SecretKeyRef: &v1.SecretKeySelector{
												LocalObjectReference: v1.LocalObjectReference{
													Name: "aws-credentials",
												},
												Key: "AWS_ACCESS_KEY_ID",
											},
										},
									},
									{
										Name: "AWS_SECRET_ACCESS_KEY",
										ValueFrom: &v1.EnvVarSource{
											SecretKeyRef: &v1.SecretKeySelector{
												LocalObjectReference: v1.LocalObjectReference{
													Name: "aws-credentials",
												},
												Key: "AWS_SECRET_ACCESS_KEY",
											},
										},
									},
								},
							},
						},
					},
				},
			},
		}

		job.Annotations[scheduledTimeAnnotation] = scheduledTime.Format(time.RFC3339)

		if err := ctrl.SetControllerReference(registry, job, r.Scheme); err != nil {
			return nil, err
		}

		return job, nil
	}

	job, err := buildJobforRegistry(&registry, nextSchedule)
	if err != nil {
		log.Error(err, "unable to construct job from template")
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, job); err != nil {
		log.Error(err, "unable to create Job for Registry", "job", job)
		return ctrl.Result{}, err
	}

	log.V(1).Info("created Job for Registry run", "job", job)
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
