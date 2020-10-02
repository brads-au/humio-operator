/*
Copyright 2020 Humio https://humio.com

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
	humioapi "github.com/humio/cli/api"
	"github.com/humio/humio-operator/pkg/helpers"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/errors"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	humiov1alpha1 "github.com/humio/humio-operator/api/v1alpha1"
	"github.com/humio/humio-operator/pkg/humio"
)

// HumioRepositoryReconciler reconciles a HumioRepository object
type HumioRepositoryReconciler struct {
	client.Client
	Log         logr.Logger // TODO: Migrate to *zap.SugaredLogger
	logger      *zap.SugaredLogger
	Scheme      *runtime.Scheme
	HumioClient humio.Client
}

// +kubebuilder:rbac:groups=core.humio.com,resources=humiorepositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.humio.com,resources=humiorepositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

func (r *HumioRepositoryReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	r.logger = logger.Sugar().With("Request.Namespace", req.Namespace, "Request.Name", req.Name, "Request.Type", helpers.GetTypeName(r))
	r.logger.Info("Reconciling HumioRepository")
	// TODO: Add back controllerutil.SetControllerReference everywhere we create k8s objects

	// Fetch the HumioRepository instance
	hr := &humiov1alpha1.HumioRepository{}
	err := r.Get(context.TODO(), req.NamespacedName, hr)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	defer func(ctx context.Context, humioClient humio.Client, hr *humiov1alpha1.HumioRepository) {
		curRepository, err := humioClient.GetRepository(hr)
		if err != nil {
			r.setState(ctx, humiov1alpha1.HumioRepositoryStateUnknown, hr)
			return
		}
		emptyRepository := humioapi.Parser{}
		if reflect.DeepEqual(emptyRepository, *curRepository) {
			r.setState(ctx, humiov1alpha1.HumioRepositoryStateNotFound, hr)
			return
		}
		r.setState(ctx, humiov1alpha1.HumioRepositoryStateExists, hr)
	}(context.TODO(), r.HumioClient, hr)

	r.logger.Info("Checking if repository is marked to be deleted")
	// Check if the HumioRepository instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isHumioRepositoryMarkedToBeDeleted := hr.GetDeletionTimestamp() != nil
	if isHumioRepositoryMarkedToBeDeleted {
		r.logger.Info("Repository marked to be deleted")
		if helpers.ContainsElement(hr.GetFinalizers(), humioFinalizer) {
			// Run finalization logic for humioFinalizer. If the
			// finalization logic fails, don't remove the finalizer so
			// that we can retry during the next reconciliation.
			r.logger.Info("Repository contains finalizer so run finalizer method")
			if err := r.finalize(hr); err != nil {
				r.logger.Infof("Finalizer method returned error: %v", err)
				return reconcile.Result{}, err
			}

			// Remove humioFinalizer. Once all finalizers have been
			// removed, the object will be deleted.
			r.logger.Info("Finalizer done. Removing finalizer")
			hr.SetFinalizers(helpers.RemoveElement(hr.GetFinalizers(), humioFinalizer))
			err := r.Update(context.TODO(), hr)
			if err != nil {
				return reconcile.Result{}, err
			}
			r.logger.Info("Finalizer removed successfully")
		}
		return reconcile.Result{}, nil
	}

	// Add finalizer for this CR
	if !helpers.ContainsElement(hr.GetFinalizers(), humioFinalizer) {
		r.logger.Info("Finalizer not present, adding finalizer to repository")
		if err := r.addFinalizer(hr); err != nil {
			return reconcile.Result{}, err
		}
	}

	cluster, err := helpers.NewCluster(context.TODO(), r, hr.Spec.ManagedClusterName, hr.Spec.ExternalClusterName, hr.Namespace, helpers.UseCertManager())
	if err != nil || cluster.Config() == nil {
		r.logger.Errorf("unable to obtain humio client config: %s", err)
		return reconcile.Result{}, err
	}

	err = r.HumioClient.Authenticate(cluster.Config())
	if err != nil {
		r.logger.Warnf("unable to authenticate humio client: %s", err)
		return reconcile.Result{Requeue: true, RequeueAfter: time.Second * 5}, err
	}

	// Get current repository
	r.logger.Info("get current repository")
	curRepository, err := r.HumioClient.GetRepository(hr)
	if err != nil {
		r.logger.Infof("could not check if repository exists: %s", err)
		return reconcile.Result{}, fmt.Errorf("could not check if repository exists: %s", err)
	}

	emptyRepository := humioapi.Repository{}
	if reflect.DeepEqual(emptyRepository, *curRepository) {
		r.logger.Info("repository doesn't exist. Now adding repository")
		// create repository
		_, err := r.HumioClient.AddRepository(hr)
		if err != nil {
			r.logger.Infof("could not create repository: %s", err)
			return reconcile.Result{}, fmt.Errorf("could not create repository: %s", err)
		}
		r.logger.Infof("created repository: %s", hr.Spec.Name)
		return reconcile.Result{Requeue: true}, nil
	}

	if (curRepository.Description != hr.Spec.Description) ||
		(curRepository.RetentionDays != float64(hr.Spec.Retention.TimeInDays)) ||
		(curRepository.IngestRetentionSizeGB != float64(hr.Spec.Retention.IngestSizeInGB)) ||
		(curRepository.StorageRetentionSizeGB != float64(hr.Spec.Retention.StorageSizeInGB)) {
		r.logger.Infof("repository information differs, triggering update, expected %v/%v/%v/%v, got: %v/%v/%v/%v",
			hr.Spec.Description,
			float64(hr.Spec.Retention.TimeInDays),
			float64(hr.Spec.Retention.IngestSizeInGB),
			float64(hr.Spec.Retention.StorageSizeInGB),
			curRepository.Description,
			curRepository.RetentionDays,
			curRepository.IngestRetentionSizeGB,
			curRepository.StorageRetentionSizeGB)
		_, err = r.HumioClient.UpdateRepository(hr)
		if err != nil {
			r.logger.Infof("could not update repository: %s", err)
			return reconcile.Result{}, fmt.Errorf("could not update repository: %s", err)
		}
	}

	// TODO: handle updates to repositoryName. Right now we just create the new repository,
	// and "leak/leave behind" the old repository.
	// A solution could be to add an annotation that includes the "old name" so we can see if it was changed.
	// A workaround for now is to delete the repository CR and create it again.

	// All done, requeue every 15 seconds even if no changes were made
	return reconcile.Result{Requeue: true, RequeueAfter: time.Second * 15}, nil
}

func (r *HumioRepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&humiov1alpha1.HumioRepository{}).
		Complete(r)
}

func (r *HumioRepositoryReconciler) finalize(hr *humiov1alpha1.HumioRepository) error {
	_, err := helpers.NewCluster(context.TODO(), r, hr.Spec.ManagedClusterName, hr.Spec.ExternalClusterName, hr.Namespace, helpers.UseCertManager())
	if errors.IsNotFound(err) {
		return nil
	}

	return r.HumioClient.DeleteRepository(hr)
}

func (r *HumioRepositoryReconciler) addFinalizer(hr *humiov1alpha1.HumioRepository) error {
	r.logger.Info("Adding Finalizer for the HumioRepository")
	hr.SetFinalizers(append(hr.GetFinalizers(), humioFinalizer))

	// Update CR
	err := r.Update(context.TODO(), hr)
	if err != nil {
		r.logger.Error(err, "Failed to update HumioRepository with finalizer")
		return err
	}
	return nil
}

func (r *HumioRepositoryReconciler) setState(ctx context.Context, state string, hr *humiov1alpha1.HumioRepository) error {
	hr.Status.State = state
	return r.Status().Update(ctx, hr)
}