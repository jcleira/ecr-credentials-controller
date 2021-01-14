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
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awsv1 "github.com/jcleira/ecr-credentials-controller/api/v1"
)

// RegistryReconciler reconciles a Registry object
type RegistryReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
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
		case kbatch.JobComplete:
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
	if registry.Status.LastRefreshTime.Time.Before(TenHoursAgo) {
		registry.Status.Valid = true
	}

	if err := r.Status().Update(ctx, &registry); err != nil {
		log.Error(err, "unable to update Registry status")
		return ctrl.Result{}, err
	}

	// Deleting old failed jobs, leaving the last one as reference.
	if len(failedJobs) > 1 {
		sort.Slice(failedJobs, func(i, j int) bool {
			if failedJobs[i].Status.LastRefreshTime == nil {
				return failedJobs[j].Status.LastRefreshTime != nil
			}

			return failedJobs[i].Status.LastRefreshTime.Before(
				failedJobs[j].Status.LastRefreshTime,
			)
		})

		for i, job := range failedJobs {
			if i+1 == len(failedJobs) {
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

	// Deleting old jobs, leaving the last one as reference.
	if len(successfulJobs) > 1 {
		sort.Slice(successfulJobs, func(i, j int) bool {
			if successfulJobs[i].Status.LastRefreshTime == nil {
				return successfulJobs[j].Status.LastRefreshTime != nil
			}

			return successfulJobs[i].Status.LastRefreshTime.Before(
				successfulJobs[j].Status.LastRefreshTime,
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

	return ctrl.Result{}, nil
}

func (r *RegistryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&awsv1.Registry{}).
		Complete(r)
}
