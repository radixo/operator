// Copyright (c) 2025 Tigera, Inc. All rights reserved.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package servicemesh

import (
	"context"
	liberrors "errors"
	"fmt"
	"time"

	sailoperatorv1 "github.com/istio-ecosystem/sail-operator/api/v1"
	sailoperatorv1alpha1 "github.com/istio-ecosystem/sail-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/go-logr/logr"
	operatorv1 "github.com/tigera/operator/api/v1"
	"github.com/tigera/operator/pkg/controller/options"
	"github.com/tigera/operator/pkg/controller/status"
	"github.com/tigera/operator/pkg/controller/utils"
	"github.com/tigera/operator/pkg/ctrlruntime"
	"github.com/tigera/operator/pkg/render"
)

var (
	log = logf.Log.WithName("controller_servicemesh")

	ServiceMeshFinalizer = "servisemesh.operator.tigera.io/finalizer"
)

// Add creates a new ServiceMesh Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
//
// Start Watches within the Add function for any resources that this controller creates or monitors. This will trigger
// calls to Reconcile() when an instance of one of the watched resources is modified.
func Add(mgr manager.Manager, opts options.AddOptions) error {
	r := newReconciler(mgr, opts)

	c, err := ctrlruntime.NewController("servicemesh-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return fmt.Errorf("failed to create servicemesh-controller: %w", err)
	}

	// Watch for changes to primary resource ServiceMesh
	err = c.WatchObject(&operatorv1.ServiceMesh{}, &handler.EnqueueRequestForObject{})
	if err != nil {
		log.V(5).Info("Failed to create ServiceMesh watch", "err", err)
		return fmt.Errorf("servicemesh-controller failed to watch primary resource: %v", err)
	}

	if err = utils.AddInstallationWatch(c); err != nil {
		log.V(5).Info("Failed to create network watch", "err", err)
		return fmt.Errorf("servicemesh-controller failed to watch Tigera network resource: %v", err)
	}

	for _, obj := range []client.Object{
		&corev1.Service{},
		&appsv1.Deployment{},
		&sailoperatorv1.Istio{},
		&sailoperatorv1.IstioCNI{},
		&sailoperatorv1alpha1.ZTunnel{},
	} {
		if err = c.WatchObject(obj, handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(), &operatorv1.ServiceMesh{}, handler.OnlyControllerOwner())); err != nil {
			log.V(5).Info("Failed to create owned for ServiceMesh watch", "err", err)
			return fmt.Errorf("servicemesh-controller failed to watch %v resources owned by ServiceMesth resource: %v", obj.GetObjectKind().GroupVersionKind().Kind, err)
		}
	}
	return nil
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, opts options.AddOptions) *ReconcileServiceMesh {
	r := &ReconcileServiceMesh{
		client:              mgr.GetClient(),
		scheme:              mgr.GetScheme(),
		provider:            opts.DetectedProvider,
		enterpriseCRDsExist: opts.EnterpriseCRDExists,
		status:              status.New(mgr.GetClient(), "servicemesh", opts.KubernetesVersion),
		clusterDomain:       opts.ClusterDomain,
		multiTenant:         opts.MultiTenant,
	}

	// Render CRDs.  For these we specify nil for the owning CR
	for _, crd := range render.SailOperatorCRDs(log) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := r.client.Create(ctx, crd); err != nil {
			// Ignore if the CRD already exists
			if !errors.IsAlreadyExists(err) {
				cancel()
				panic(fmt.Errorf("failed to create CustomResourceDefinition %s: %s", crd.GetName(), err))
			}
		}
		cancel()
	}

	r.status.Run(opts.ShutdownContext)
	return r
}

// blank assignment to verify that ReconcileServiceMesh implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileServiceMesh{}

// ReconcileServiceMesh reconciles a ServiceMesh object
type ReconcileServiceMesh struct {
	client              client.Client
	scheme              *runtime.Scheme
	provider            operatorv1.Provider
	enterpriseCRDsExist bool
	status              status.StatusManager
	clusterDomain       string
	multiTenant         bool
}

// Reconcile reads that state of the cluster for a ServiceMesh object and makes changes based on the state read
// and what is in the ServiceMesh.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileServiceMesh) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling ServiceMesh")

	// Get the GatewayAPI CR.
	svcMesh, msg, err := GetServiceMesh(ctx, r.client)
	if err != nil {
		if errors.IsNotFound(err) {
			reqLogger.Info("ServiceMesh object not found")
			r.status.OnCRNotFound()
			return reconcile.Result{}, nil
		}
		r.status.SetDegraded(operatorv1.ResourceReadError, "Error querying for ServiceMesh CR: "+msg, err, reqLogger)
		return reconcile.Result{}, err
	}
	r.status.OnCRFound()

	// SetMetaData in the TigeraStatus such as observedGenerations.
	defer r.status.SetMetaData(&svcMesh.ObjectMeta)

	// Check is is marked to delete
	if svcMesh.GetDeletionTimestamp() != nil {
		if controllerutil.ContainsFinalizer(svcMesh, ServiceMeshFinalizer) {
			err = r.finalizeServiceMesh(ctx, reqLogger, svcMesh)
			if err != nil {
				return reconcile.Result{}, err
			}

			controllerutil.RemoveFinalizer(svcMesh, ServiceMeshFinalizer)
			err = r.client.Update(ctx, svcMesh)
			if err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}

	// Add finalizer
	if !controllerutil.ContainsFinalizer(svcMesh, ServiceMeshFinalizer) {
		controllerutil.AddFinalizer(svcMesh, ServiceMeshFinalizer)
		err = r.client.Update(ctx, svcMesh)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	// Check namespace
	var namespace corev1.Namespace
	err = r.client.Get(ctx, client.ObjectKey{Name: render.GetSailOperatorResources().Deployment.Namespace}, &namespace)
	if err != nil {
		if !errors.IsNotFound(err) {
			return reconcile.Result{}, err
		}
	} else if namespace.GetDeletionTimestamp() != nil {
		// wait for the clean up
		return reconcile.Result{RequeueAfter: utils.StandardRetry}, nil
	}

	// Get the Installation, for private registry and pull secret config.
	variant, installation, err := utils.GetInstallation(ctx, r.client)
	if err != nil {
		if errors.IsNotFound(err) {
			r.status.SetDegraded(operatorv1.ResourceNotFound, "Installation not found", err, reqLogger)
			return reconcile.Result{}, nil
		}
		r.status.SetDegraded(operatorv1.ResourceReadError, "Error querying installation", err, reqLogger)
		return reconcile.Result{}, err
	}

	if variant == "" {
		r.status.SetDegraded(operatorv1.ResourceNotReady, "Waiting for Installation Variant to be set", nil, reqLogger)
		return reconcile.Result{}, nil
	}

	/*XXX
	pullSecrets, err := utils.GetNetworkingPullSecrets(installation, r.client)
	if err != nil {
		r.status.SetDegraded(operatorv1.ResourceReadError, "Error retrieving pull secrets", err, reqLogger)
		return reconcile.Result{}, err
	}
	*/

	// Render non-CRD resources for ServiceMesh support
	nonCRDComponent := render.ServiceMeshComponent(&render.ServiceMeshConfig{
		Installation: installation,
		//PullSecrets:  pullSecrets,
		ServiceMesh: svcMesh,
	})
	//err = imageset.ApplyImageSet(ctx, r.client, variant, nonCRDComponent)
	//if err != nil {
	//	r.status.SetDegraded(operatorv1.ResourceCreateError, "Error with images from ImageSet", err, log)
	//	return reconcile.Result{}, err
	//}
	err = utils.NewComponentHandler(log, r.client, r.scheme, svcMesh).CreateOrUpdateOrDelete(ctx, nonCRDComponent, r.status)
	if err != nil {
		r.status.SetDegraded(operatorv1.ResourceCreateError, "Error rendering ServiceMesh resources", err, log)
		return reconcile.Result{}, err
	}

	// Clear the degraded bit if we've reached this far.
	r.status.ClearDegraded()

	// Update the status of the ServiceMesh instance and StatusManager.
	return reconcile.Result{}, nil
}

func (r *ReconcileServiceMesh) finalizeServiceMesh(ctx context.Context, reqLogger logr.Logger, svcMesh *operatorv1.ServiceMesh) error {
	// Delete sailoperator resources and wait the operator to remove all
	// components
	var (
		ztunnel  sailoperatorv1alpha1.ZTunnel
		istioCNI sailoperatorv1.IstioCNI
		istio    sailoperatorv1.Istio
		delError error = liberrors.New("Waiting for delete")
	)

	err := r.client.Get(ctx, client.ObjectKey{Name: "default"}, &ztunnel)
	if err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
	} else {
		if ztunnel.GetDeletionTimestamp() == nil {
			if err = r.client.Delete(ctx, &ztunnel); err != nil {
				return err
			}
		}
		return delError
	}

	err = r.client.Get(ctx, client.ObjectKey{Name: "default"}, &istioCNI)
	if err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
	} else {
		if istioCNI.GetDeletionTimestamp() == nil {
			if err = r.client.Delete(ctx, &istioCNI); err != nil {
				return err
			}
		}
		return delError
	}

	err = r.client.Get(ctx, client.ObjectKey{Name: "default"}, &istio)
	if err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
	} else {
		if istio.GetDeletionTimestamp() == nil {
			if err = r.client.Delete(ctx, &istio); err != nil {
				return err
			}
		}
		return delError
	}

	return nil
}

// GetServiceMesh finds the correct ServiceMesh resource and returns a message and error in the case of an error.
func GetServiceMesh(ctx context.Context, client client.Client) (*operatorv1.ServiceMesh, string, error) {
	// Fetch the ServiceMesh resource.  Look for "default" first.
	resource := &operatorv1.ServiceMesh{}
	err := client.Get(ctx, utils.DefaultInstanceKey, resource)
	if err != nil {
		if !errors.IsNotFound(err) {
			return nil, fmt.Sprintf("failed to get ServiceMesh '%s'", utils.DefaultInstanceKey), err
		}

		// Default resource doesn't exist. Check for the legacy (enterprise only) CR.
		err = client.Get(ctx, utils.DefaultTSEEInstanceKey, resource)
		if err != nil {
			return nil, fmt.Sprintf("failed to get ServiceMesh '%s'", utils.DefaultTSEEInstanceKey), err
		}
	} else {
		// Assert there is no legacy "tigera-secure" resource present.
		err = client.Get(ctx, utils.DefaultTSEEInstanceKey, resource)
		if err == nil {
			return nil,
				"Duplicate configuration detected",
				fmt.Errorf("multiple ServiceMesh CRs provided. To fix, run \"kubectl delete gatewayapi %s\"", utils.DefaultTSEEInstanceKey)
		}
	}
	return resource, "", nil
}
