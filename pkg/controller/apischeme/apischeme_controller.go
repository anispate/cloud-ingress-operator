package apischeme

import (
	"context"
	"fmt"
	"time"

	cloudingressv1alpha1 "github.com/openshift/cloud-ingress-operator/pkg/apis/cloudingress/v1alpha1"
	"github.com/openshift/cloud-ingress-operator/pkg/cloudclient"
	utils "github.com/openshift/cloud-ingress-operator/pkg/controller/utils"
	cioerrors "github.com/openshift/cloud-ingress-operator/pkg/errors"
	localmetrics "github.com/openshift/cloud-ingress-operator/pkg/localmetrics"
	baseutils "github.com/openshift/cloud-ingress-operator/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	reconcileFinalizerDNS = "dns.cloudingress.managed.openshift.io"
	elbAnnotationKey      = "service.beta.kubernetes.io/aws-load-balancer-connection-idle-timeout"
	elbAnnotationValue    = "1800"
	longwait              = 60
	shortwait             = 10
)

var (
	log = logf.Log.WithName("controller_apischeme")
	// for testing to set it to something else
	cloudClient cloudclient.CloudClient
)

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new APIScheme Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileAPIScheme{client: mgr.GetClient(), scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("apischeme-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource APIScheme
	err = c.Watch(&source.Kind{Type: &cloudingressv1alpha1.APIScheme{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileAPIScheme implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileAPIScheme{}

// ReconcileAPIScheme reconciles a APIScheme object
type ReconcileAPIScheme struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
}

// LoadBalancer contains the relevant information to create a Load Balancer
// TODO: Move this into pkg/client
type LoadBalancer struct {
	EndpointName string // FQDN of what it should be called
	BaseDomain   string // What is the base domain (DNS zone) for the EndpointName record?
}

// Reconcile will ensure that the rh-api management api endpoint is created and ready.
// Rough Steps:
// 1. Create Service
// 2. Add DNS CNAME from rh-api to the ELB created by AWS provider
// 3. Add Forwarding rule in GCP for the lb service
// 3. Ready for work (Ready)
func (r *ReconcileAPIScheme) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling APIScheme")

	// Fetch the APIScheme instance
	instance := &cloudingressv1alpha1.APIScheme{}
	err := r.client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			reqLogger.Info("Couldn't find the APIScheme object")
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		reqLogger.Error(err, "Error reading APIScheme object")
		return reconcile.Result{}, err
	}

	// If the management API isn't enabled, we have nothing to do!
	if !instance.Spec.ManagementAPIServerIngress.Enabled {
		reqLogger.Info("Not enabled", "instance", instance)
		return reconcile.Result{}, nil
	}

	if cloudClient == nil {
		cloudPlatform, err := baseutils.GetPlatformType(r.client)
		if err != nil {
			r.SetAPISchemeStatus(instance, "Couldn't reconcile", "Couldn't create a Cloud Client", cloudingressv1alpha1.ConditionError)
			r.SetAPISchemeStatusMetric(instance)
			return reconcile.Result{}, err
		}
		cloudClient = cloudclient.GetClientFor(r.client, *cloudPlatform)
	}

	serviceNamespacedName := types.NamespacedName{
		Name:      instance.Spec.ManagementAPIServerIngress.DNSName,
		Namespace: "openshift-kube-apiserver",
	}

	// Check for a deletion timestamp.
	if instance.DeletionTimestamp.IsZero() {
		// Request object is alive, so ensure it has the DNS finalizer.
		if !controllerutil.ContainsFinalizer(instance, reconcileFinalizerDNS) {
			controllerutil.AddFinalizer(instance, reconcileFinalizerDNS)
			if err = r.client.Update(context.TODO(), instance); err != nil {
				return reconcile.Result{}, err
			}
		}
	} else {
		// Request object is being deleted.
		if controllerutil.ContainsFinalizer(instance, reconcileFinalizerDNS) {
			found := &corev1.Service{}
			if err = r.client.Get(context.TODO(), serviceNamespacedName, found); err != nil {
				if errors.IsNotFound(err) {
					// Service was not found!
					//
					// Skip the DeleteAdminAPIDNS call and remove the
					// finalizer anyway so the CR deletion can proceed.
					// This could leave DNS entries behind!
					//
					// TODO As a future enhancement, the CloudClient
					//      provider should handle this scenario and
					//      look up the necessary information itself
					//      to proceed with the DNS deletion.
					found = nil
				} else {
					reqLogger.Error(err, "Couldn't get the Service")
					return reconcile.Result{}, err
				}
			}

			if found != nil {
				err = cloudClient.DeleteAdminAPIDNS(context.TODO(), r.client, instance, found)
				switch err := err.(type) {
				case nil:
					// all good
				case *cioerrors.LoadBalancerNotReadyError:
					// couldn't find the load balancer - it's likely still queued for creation
					r.SetAPISchemeStatus(instance, "Couldn't reconcile", "Load balancer isn't ready", cloudingressv1alpha1.ConditionError)
					r.SetAPISchemeStatusMetric(instance)
					return reconcile.Result{Requeue: true, RequeueAfter: shortwait * time.Second}, nil
				default:
					reqLogger.Error(err, "Failed to delete the DNS record")
					r.SetAPISchemeStatus(instance, "Couldn't reconcile", "Failed to delete the DNS record", cloudingressv1alpha1.ConditionError)
					r.SetAPISchemeStatusMetric(instance)
					return reconcile.Result{}, err
				}
			}

			// Remove the DNS finalizer and update the request object.
			controllerutil.RemoveFinalizer(instance, reconcileFinalizerDNS)
			if err = r.client.Update(context.TODO(), instance); err != nil {
				return reconcile.Result{}, err
			}

			// Requeue once more after updating.  Without a finalizer,
			// the next pass should delete the request object.
			return reconcile.Result{Requeue: true}, nil
		}

		// Halt the reconciliation.
		return reconcile.Result{}, nil
	}

	// Does the Service exist already?
	found := &corev1.Service{}
	err = r.client.Get(context.TODO(), serviceNamespacedName, found)
	if err != nil {
		if errors.IsNotFound(err) {
			// need to create it
			dep := r.newServiceFor(instance)
			reqLogger.Info("Service not found. Creating", "service", dep)
			err = r.client.Create(context.TODO(), dep)
			if err != nil {
				reqLogger.Error(err, "Failure to create new Service")
				return reconcile.Result{}, err
			}
			// Reconcile again to get the new Service and give cloud provider time to create the LB
			reqLogger.Info("Service was just created, so let's try to requeue to set it up")
			return reconcile.Result{Requeue: true, RequeueAfter: longwait * time.Second}, nil
		} else if err != nil {
			reqLogger.Error(err, "Couldn't get the Service")
			return reconcile.Result{}, err
		}
	}
	// Reconcile the access list in the Service
	if !sliceEquals(found.Spec.LoadBalancerSourceRanges, instance.Spec.ManagementAPIServerIngress.AllowedCIDRBlocks) {
		reqLogger.Info(fmt.Sprintf("Mismatch svc %s != %s\n", found.Spec.LoadBalancerSourceRanges, instance.Spec.ManagementAPIServerIngress.AllowedCIDRBlocks))
		reqLogger.Info(fmt.Sprintf("Mismatch between %s/service/%s LoadBalancerSourceRanges and AllowedCIDRBlocks. Updating...", found.GetNamespace(), found.GetName()))
		found.Spec.LoadBalancerSourceRanges = instance.Spec.ManagementAPIServerIngress.AllowedCIDRBlocks
		err = r.client.Update(context.TODO(), found)
		if err != nil {
			reqLogger.Error(err, fmt.Sprintf("Failed to update the %s/service/%s LoadBalancerSourceRanges", found.GetNamespace(), found.GetName()))
			return reconcile.Result{}, err
		}
		// let's re-queue just in case
		reqLogger.Info("Requeuing after svc update")
		return reconcile.Result{Requeue: true, RequeueAfter: shortwait * time.Second}, nil
	}

	if !metav1.HasAnnotation(found.ObjectMeta, elbAnnotationKey) ||
		found.Annotations[elbAnnotationKey] != elbAnnotationValue {
		metav1.SetMetaDataAnnotation(&found.ObjectMeta, elbAnnotationKey, elbAnnotationValue)
		err = r.client.Update(context.TODO(), found)
		if err != nil {
			reqLogger.Error(err, "Error updating service annotation")
			return reconcile.Result{}, err
		}
		reqLogger.Info(fmt.Sprintf("Updated %s svc idle timeout to %s", found.Name, elbAnnotationValue))
	}

	err = cloudClient.EnsureAdminAPIDNS(context.TODO(), r.client, instance, found)
	// Check for error types that this operator knows about
	switch err := err.(type) {
	case nil:
		// no problems
		r.SetAPISchemeStatus(instance, "Success", "Admin API Endpoint created", cloudingressv1alpha1.ConditionReady)
		r.SetAPISchemeStatusMetric(instance)
		return reconcile.Result{RequeueAfter: longwait * time.Second}, nil
	case *cioerrors.DnsUpdateError:
		// couldn't update DNS
		r.SetAPISchemeStatus(instance, "Couldn't reconcile", "Couldn't ensure the admin API endpoint: "+err.Error(), cloudingressv1alpha1.ConditionError)
		r.SetAPISchemeStatusMetric(instance)
		return reconcile.Result{}, err
	case *cioerrors.ForwardingRuleNotFoundError:
		// This error handles the missing/deleted forwarding rule/LB in cloud provider
		r.SetAPISchemeStatus(instance, "Couldn't reconcile", "Forwarding rule was deleted on cloud provider", cloudingressv1alpha1.ConditionError)

		// To recover from this case we will need to delete the lb service.
		// It will be recreated  at the next reconcile.
		reqLogger.Info(fmt.Sprintf("Forwarding rule was deleted on cloud provider, deleting service %s/service/%s to force recreation", found.GetNamespace(), found.GetName()))
		deleteSvcErr := r.client.Delete(context.TODO(), found)
		if deleteSvcErr != nil {
			if instance.DeletionTimestamp.IsZero() {
				reqLogger.Error(err, fmt.Sprintf("Failed to delete the %s/service/%s service. It could already be deleted. Waiting %d seconds to complete possible deletion.", found.GetNamespace(), found.GetName(), longwait))
			} else {
				reqLogger.Error(err, fmt.Sprintf("Service %s/service/%s already deleted. Waiting %d seconds to complete deletion.", found.GetNamespace(), found.GetName(), longwait))
			}
		}
		// Need to wait till deletion is completely finished to avoid race condition.
		return reconcile.Result{Requeue: true, RequeueAfter: longwait * time.Second}, nil
	case *cioerrors.LoadBalancerNotReadyError:
		r.SetAPISchemeStatusMetric(instance)
		if utils.FindAPISchemeCondition(instance.Status.Conditions, cloudingressv1alpha1.ConditionReady) == nil {
			// The APIscheme was never ready. The Load Balancer is likely still creating
			r.SetAPISchemeStatus(instance, "Couldn't reconcile", "Load balancer isn't ready", cloudingressv1alpha1.ConditionError)
			reqLogger.Info("LoadBalancer isn't ready yet")
		} else {
			// The APIScheme had been ready previously. The Load Balancer has likely been deleted
			r.SetAPISchemeStatus(instance, "Couldn't reconcile", "Load balancer was deleted", cloudingressv1alpha1.ConditionError)

			// To recover from this case we will need to delete the service. It will be recreated  at the next reconcile
			reqLogger.Info(fmt.Sprintf("LoadBalancer was deleted, deleting service %s/service/%s to recover", found.GetNamespace(), found.GetName()))
			err := r.client.Delete(context.TODO(), found)
			if err != nil {
				reqLogger.Error(err, fmt.Sprintf("Failed to delete the %s/service/%s service, it could already be deleted. Waiting to complete possible deletion.", found.GetNamespace(), found.GetName()))
			}
		}

		return reconcile.Result{Requeue: true, RequeueAfter: longwait * time.Second}, nil
	default:
		// not one of ours
		log.Error(err, "Error ensuring Admin API", "instance", instance, "Service", found)
		return reconcile.Result{}, err
	}
}

func (r *ReconcileAPIScheme) newServiceFor(instance *cloudingressv1alpha1.APIScheme) *corev1.Service {
	labels := map[string]string{
		"app":          "cloud-ingress-operator-" + instance.Spec.ManagementAPIServerIngress.DNSName,
		"apischeme_cr": instance.GetName(),
	}
	selector := map[string]string{
		"apiserver": "true",
		"app":       "openshift-kube-apiserver",
	}
	annotations := map[string]string{
		elbAnnotationKey: elbAnnotationValue,
	}
	// Note: This owner reference should nbnot be expected to work
	//ref := metav1.NewControllerRef(instance, instance.GetObjectKind().GroupVersionKind())
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        instance.Spec.ManagementAPIServerIngress.DNSName,
			Namespace:   "openshift-kube-apiserver",
			Labels:      labels,
			Annotations: annotations,
			//OwnerReferences: []metav1.OwnerReference{*ref},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Protocol:   "TCP",
					Port:       6443,
					TargetPort: intstr.FromInt(6443),
				},
			},
			Selector:                 selector,
			Type:                     corev1.ServiceTypeLoadBalancer,
			LoadBalancerSourceRanges: instance.Spec.ManagementAPIServerIngress.AllowedCIDRBlocks,
		},
	}
}

// SetAPISchemeStatus will set the status on the APISscheme object with a human message, as in an error situation
func (r *ReconcileAPIScheme) SetAPISchemeStatus(crObject *cloudingressv1alpha1.APIScheme, reason, message string, ctype cloudingressv1alpha1.APISchemeConditionType) {
	crObject.Status.Conditions = utils.SetAPISchemeCondition(
		crObject.Status.Conditions,
		ctype,
		corev1.ConditionTrue,
		reason,
		message,
		utils.UpdateConditionIfReasonOrMessageChange)
	crObject.Status.State = ctype
	err := r.client.Status().Update(context.TODO(), crObject)
	// TODO: Should we return an error here if this update fails?
	if err != nil {
		log.Error(err, "Error updating cr status")
	}
}

// SetAPISchemeStatusMetric updates a gauge in localmetrics
func (r *ReconcileAPIScheme) SetAPISchemeStatusMetric(crObject *cloudingressv1alpha1.APIScheme) {
	if crObject.Status.State == "Ready" {
		localmetrics.MetricAPISchemeConditionStatus.Set(float64(1))
		return
	}
	localmetrics.MetricAPISchemeConditionStatus.Set(float64(0))
}

func sliceEquals(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := 0; i < len(left); i++ {
		if left[i] != right[i] {
			fmt.Printf("Mismatch %s != %s\n", left[i], right[i])
			return false
		}
	}
	return true
}
