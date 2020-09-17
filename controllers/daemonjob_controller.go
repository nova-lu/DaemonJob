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

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	djv1 "github.com/Dysproz/DaemonJob/api/v1"
)

// DaemonJobReconciler reconciles a DaemonJob object
type DaemonJobReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=dj.dysproz.io,resources=daemonjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dj.dysproz.io,resources=daemonjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

// SetupWithManager function specifies how the controller is built to watch a CR and
// other resources that are owned and managed by that controller.
func (r *DaemonJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&djv1.DaemonJob{}).
		Complete(r); err != nil {
		return err
	}

	if err := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Watches(&source.Kind{Type: &corev1.Node{}}, &handler.EnqueueRequestsFromMapFunc{
			ToRequests: handler.ToRequestsFunc(func(nodeObject handler.MapObject) []reconcile.Request {
				var djObjects djv1.DaemonJobList
				_ = mgr.GetClient().List(context.TODO(), &djObjects)
				var requests = []reconcile.Request{}
				for _, djObject := range djObjects.Items {
					requests = append(requests, reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name:      djObject.Name,
							Namespace: djObject.Namespace,
						},
					})
				}
				return requests
			}),
		}).
		Complete(r); err != nil {
		return err
	}

	return nil
}

// Reconcile method that implements the reconcile loop.
// The reconcile loop is passed the Request argument which is a Namespace/Name key
// used to lookup the primary resource object
func (r *DaemonJobReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	_ = context.Background()
	_ = r.Log.WithValues("daemonjob", req.NamespacedName)
	r.Log.Info("Reconciling DaemonJob", "request name", req.Name, "request namespace", req.Namespace)
	instance := &djv1.DaemonJob{}
	instanceType := "daemonjob"
	ctx := context.TODO()

	if err := r.Client.Get(ctx, req.NamespacedName, instance); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	if !instance.GetDeletionTimestamp().IsZero() {
		return reconcile.Result{}, nil
	}

	nodeSelector := client.MatchingLabels{}
	if instance.Spec.Selector != nil {
		nodeSelector = instance.Spec.Selector.MatchLabels
	}
	var nodesListOptions client.MatchingLabels = nodeSelector
	var nodes corev1.NodeList
	if err := r.Client.List(ctx, &nodes, nodesListOptions); err != nil && errors.IsNotFound(err) {
		return reconcile.Result{}, nil
	}
	jobReplicas := int32(len(nodes.Items))
	job := getJob(instance, &jobReplicas, req.Name, instanceType)
	err := controllerutil.SetControllerReference(instance, job, r.Scheme)
	if err != nil {
		return reconcile.Result{}, err
	}

	var clusterJob batchv1.Job
	if err := r.Client.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, &clusterJob); err == nil {
		if *clusterJob.Spec.Completions != jobReplicas || clusterJob.Labels["controller_generation"] != job.Labels["controller_generation"] {
			_ = r.Client.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: job.Name, Namespace: job.Namespace}}, client.PropagationPolicy("Background"))
			return reconcile.Result{RequeueAfter: 5}, nil
		}
	} else if errors.IsNotFound(err) {
		if err := r.Client.Create(ctx, job); err != nil {
			return reconcile.Result{}, err
		}
	} else {
		return reconcile.Result{}, err
	}

	if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: clusterJob.Name, Namespace: clusterJob.Namespace},
		job); err != nil {
		return ctrl.Result{}, err
	}

	instance.Status = &clusterJob.Status

	return ctrl.Result{}, r.Client.Status().Update(context.TODO(), instance)
}

func getJob(instance *djv1.DaemonJob, replicas *int32, reqName, instanceType string) *batchv1.Job {
	var jobAffinity = corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      instanceType,
						Operator: "In",
						Values:   []string{reqName},
					}},
				},
				TopologyKey: "kubernetes.io/hostname",
			}},
		},
	}

	var podSpec = instance.Spec.Template
	podSpec.Spec.Affinity = &jobAffinity

	if podSpec.Spec.RestartPolicy == "Always" {
		podSpec.Spec.RestartPolicy = "OnFailure"
	}

	return &batchv1.Job{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Job",
			APIVersion: "batch/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name + "-job",
			Namespace: instance.Namespace,
			Labels:    instance.Labels,
		},
		Spec: batchv1.JobSpec{
			Parallelism:             replicas,
			Completions:             replicas,
			Selector:                instance.Spec.Selector,
			Template:                podSpec,
			ManualSelector:          instance.Spec.ManualSelector,
			TTLSecondsAfterFinished: instance.Spec.TTLSecondsAfterFinished,
			BackoffLimit:            instance.Spec.BackoffLimit,
			ActiveDeadlineSeconds:   instance.Spec.ActiveDeadlineSeconds,
		},
	}
}
