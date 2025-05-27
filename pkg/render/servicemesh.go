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

package render

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	sailoperatorv1 "github.com/istio-ecosystem/sail-operator/api/v1"
	sailoperatorv1alpha1 "github.com/istio-ecosystem/sail-operator/api/v1alpha1"
	rmeta "github.com/tigera/operator/pkg/render/common/meta"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextenv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	operatorv1 "github.com/tigera/operator/api/v1"
)

var (
	SailOperatorIstioVersion = "v1.26.0"

	sailOperatorResources     *SailOperatorResources
	waitSailOperatorResources = make(chan struct{})

	//go:embed sail_operator_resources.yaml
	sailOperatorResourceYaml string
)

type ServiceMeshConfig struct {
	ServiceMesh  *operatorv1.ServiceMesh
	Installation *operatorv1.InstallationSpec
}

type serviceMeshComponent struct {
	cfg *ServiceMeshConfig
}

// ResolveImages should call components.GetReference for all images that the Component
// needs, passing 'is' to the GetReference call and if there are any errors those
// are returned. It is valid to pass nil for 'is' as GetReference accepts the value.
// ResolveImages must be called before Objects is called for the component.
func (s *serviceMeshComponent) ResolveImages(is *operatorv1.ImageSet) error {
	return nil
	panic("not implemented") // TODO: Implement
}

func (s *serviceMeshComponent) Objects() (objsToCreate []client.Object, objsToDelete []client.Object) {
	resources := GetSailOperatorResources()
	namespaceName := resources.Deployment.Namespace

	// Add the namespace
	objs := []client.Object{
		CreateNamespace(
			namespaceName,
			s.cfg.Installation.KubernetesProvider,
			PSSPrivileged,
			s.cfg.Installation.Azure,
		),
	}

	// Add sail-operator resources
	objs = append(objs, resources.ServiceAccount.DeepCopyObject().(client.Object))
	for _, resource := range resources.ClusterRoles {
		objs = append(objs, resource.DeepCopyObject().(client.Object))
	}
	for _, resource := range resources.ClusterRoleBindings {
		objs = append(objs, resource.DeepCopyObject().(client.Object))
	}
	objs = append(objs, resources.Role.DeepCopyObject().(client.Object))
	objs = append(objs, resources.RoleBinding.DeepCopyObject().(client.Object))
	objs = append(objs, resources.Service.DeepCopyObject().(client.Object))
	objs = append(objs, resources.Deployment.DeepCopyObject().(client.Object))

	// Add Istio, IstioCNI and ZTunnel resources
	updateStrategyGracePeriod := int64(30)
	enableAccessLogService := true
	accessLogServiceAddr := "accesslog-svc.calico-system:15000"
	/*XXX
	accessLogSkipVerify := true
	*/
	istio := &sailoperatorv1.Istio{
		TypeMeta: metav1.TypeMeta{Kind: "Istio", APIVersion: "sailoperator.io/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "default",
		},
		Spec: sailoperatorv1.IstioSpec{
			Version:   SailOperatorIstioVersion,
			Namespace: namespaceName,
			Profile:   "ambient",
			UpdateStrategy: &sailoperatorv1.IstioUpdateStrategy{
				Type: sailoperatorv1.UpdateStrategyTypeInPlace,

				InactiveRevisionDeletionGracePeriodSeconds: &updateStrategyGracePeriod,
			},
			Values: &sailoperatorv1.Values{
				MeshConfig: &sailoperatorv1.MeshConfig{
					EnableEnvoyAccessLogService: &enableAccessLogService,
					DefaultConfig: &sailoperatorv1.MeshConfigProxyConfig{
						EnvoyAccessLogService: &sailoperatorv1.RemoteService{
							Address: &accessLogServiceAddr,
							/*XXX
							TlsSettings: &sailoperatorv1.ClientTLSSettings{
								InsecureSkipVerify: &accessLogSkipVerify,
							},
							*/
						},
					},
				},
				Pilot: &sailoperatorv1.PilotConfig{
					TrustedZtunnelNamespace: &namespaceName,
				},
			},
		},
	}
	objs = append(objs, istio)

	/*XXX
	ambientDNSCapture := false
	*/
	istioCNI := &sailoperatorv1.IstioCNI{
		TypeMeta: metav1.TypeMeta{Kind: "IstioCNI", APIVersion: "sailoperator.io/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "default",
		},
		Spec: sailoperatorv1.IstioCNISpec{
			Version:   SailOperatorIstioVersion,
			Namespace: namespaceName,
			Profile:   "ambient",
			// Testing with no DNS proxy
			/*XXX
			Values: &sailoperatorv1.CNIValues{
				Cni: &sailoperatorv1.CNIConfig{
					Ambient: &sailoperatorv1.CNIAmbientConfig{
						DnsCapture: &ambientDNSCapture,
					},
				},
			},
			*/
		},
	}
	if s.cfg.Installation.KubernetesProvider == operatorv1.ProviderGKE {
		gkeStr := "gke"
		istioCNI.Spec.Values = &sailoperatorv1.CNIValues{
			Global: &sailoperatorv1.CNIGlobalConfig{
				Platform: &gkeStr,
			},
		}
		/*XXX
		istioCNI.Spec.Values.Global = &sailoperatorv1.CNIGlobalConfig{
			Platform: &gkeStr,
		}
		*/
	}
	objs = append(objs, istioCNI)

	ztunnel := &sailoperatorv1alpha1.ZTunnel{
		TypeMeta: metav1.TypeMeta{Kind: "ZTunnel", APIVersion: "sailoperator.io/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "default",
		},
		Spec: sailoperatorv1alpha1.ZTunnelSpec{
			Version:   SailOperatorIstioVersion,
			Namespace: namespaceName,
			Profile:   "ambient",
			Values: &sailoperatorv1.ZTunnelValues{
				ZTunnel: &sailoperatorv1.ZTunnelConfig{
					IstioNamespace: &namespaceName,
				},
			},
		},
	}
	objs = append(objs, ztunnel)

	return objs, nil
}

func (s *serviceMeshComponent) Ready() bool {
	return true
}

func (s *serviceMeshComponent) SupportedOSType() rmeta.OSType {
	return rmeta.OSTypeLinux
}

func ServiceMeshComponent(cfg *ServiceMeshConfig) Component {
	return &serviceMeshComponent{cfg}
}

// SailOperatorResources struct stores in golang objects the parsed yaml created
// with helm chart of sail-operator project
type SailOperatorResources struct {
	CRDS                []*apiextenv1.CustomResourceDefinition
	ServiceAccount      *corev1.ServiceAccount
	ClusterRoles        []*rbacv1.ClusterRole
	ClusterRoleBindings []*rbacv1.ClusterRoleBinding
	Role                *rbacv1.Role
	RoleBinding         *rbacv1.RoleBinding
	Service             *corev1.Service
	Deployment          *appsv1.Deployment
}

func init() {
	// init sailOperatorResources
	go func() {
		sailOperatorResources = &SailOperatorResources{}
		for _, yml := range strings.Split(sailOperatorResourceYaml, "\n---\n") {
			var yamlKind struct {
				APIVersion string `yaml:"apiVersion"`
				Kind       string `yaml:"kind"`
			}
			if err := yaml.Unmarshal([]byte(yml), &yamlKind); err != nil {
				panic(fmt.Sprintf("unable to unmarshal YAML: %v:\n%v\n", err, yml))
			}
			kindStr := yamlKind.APIVersion + "/" + yamlKind.Kind
			switch kindStr {
			case "apiextensions.k8s.io/v1/CustomResourceDefinition":
				obj := &apiextenv1.CustomResourceDefinition{}
				if err := yaml.Unmarshal([]byte(yml), obj); err != nil {
					panic(fmt.Sprintf("unable to unmarshal %v: %v", kindStr, err))
				}
				sailOperatorResources.CRDS = append(sailOperatorResources.CRDS, obj)
			case "v1/ServiceAccount":
				if sailOperatorResources.ServiceAccount != nil {
					panic("already read a ServiceAccount from YAML")
				}
				sailOperatorResources.ServiceAccount = &corev1.ServiceAccount{}
				if err := yaml.Unmarshal([]byte(yml), sailOperatorResources.ServiceAccount); err != nil {
					panic(fmt.Sprintf("unable to unmarshal %v: %v", kindStr, err))
				}
			case "rbac.authorization.k8s.io/v1/ClusterRole":
				obj := &rbacv1.ClusterRole{}
				if err := yaml.Unmarshal([]byte(yml), obj); err != nil {
					panic(fmt.Sprintf("unable to unmarshal %v: %v", kindStr, err))
				}
				sailOperatorResources.ClusterRoles = append(sailOperatorResources.ClusterRoles, obj)
			case "rbac.authorization.k8s.io/v1/ClusterRoleBinding":
				obj := &rbacv1.ClusterRoleBinding{}
				if err := yaml.Unmarshal([]byte(yml), obj); err != nil {
					panic(fmt.Sprintf("unable to unmarshal %v: %v", kindStr, err))
				}
				sailOperatorResources.ClusterRoleBindings = append(sailOperatorResources.ClusterRoleBindings, obj)
			case "rbac.authorization.k8s.io/v1/Role":
				if sailOperatorResources.Role != nil {
					panic("already read a Role from YAML")
				}
				sailOperatorResources.Role = &rbacv1.Role{}
				if err := yaml.Unmarshal([]byte(yml), sailOperatorResources.Role); err != nil {
					panic(fmt.Sprintf("unable to unmarshal %v: %v", kindStr, err))
				}
			case "rbac.authorization.k8s.io/v1/RoleBinding":
				if sailOperatorResources.RoleBinding != nil {
					panic("already read a RoleBinding from YAML")
				}
				sailOperatorResources.RoleBinding = &rbacv1.RoleBinding{}
				if err := yaml.Unmarshal([]byte(yml), sailOperatorResources.RoleBinding); err != nil {
					panic(fmt.Sprintf("unable to unmarshal %v: %v", kindStr, err))
				}
			case "v1/Service":
				if sailOperatorResources.Service != nil {
					panic("already read operator Service from YAML")
				}
				sailOperatorResources.Service = &corev1.Service{}
				if err := yaml.Unmarshal([]byte(yml), sailOperatorResources.Service); err != nil {
					panic(fmt.Sprintf("unable to unmarshal %v: %v", kindStr, err))
				}
			case "apps/v1/Deployment":
				if sailOperatorResources.Deployment != nil {
					panic("already read operator Deployment from YAML")
				}
				sailOperatorResources.Deployment = &appsv1.Deployment{}
				if err := yaml.Unmarshal([]byte(yml), sailOperatorResources.Deployment); err != nil {
					panic(fmt.Sprintf("unable to unmarshal %v: %v", kindStr, err))
				}
			case "/":
				// No-op.  We see this when there is only a comment between
				// two "---" delimiters.
			default:
				panic(fmt.Sprintf("unhandled type %v", kindStr))
			}
		}
		close(waitSailOperatorResources)
	}()
}

func GetSailOperatorResources() *SailOperatorResources {
	<-waitSailOperatorResources
	return sailOperatorResources
}

func SailOperatorCRDs(log logr.Logger) []client.Object {
	resources := GetSailOperatorResources()
	crds := make([]client.Object, 0, len(resources.CRDS))
	for _, crd := range resources.CRDS {
		crds = append(crds, crd.DeepCopyObject().(client.Object))
	}
	return crds
}
