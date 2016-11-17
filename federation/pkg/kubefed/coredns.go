/*
Copyright 2016 The Kubernetes Authors.

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

package kubefed

import (
	"io"

	"k8s.io/kubernetes/federation/pkg/kubefed/util"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/apis/extensions"
	client "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	"k8s.io/kubernetes/pkg/kubectl/cmd/templates"
	cmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/util/intstr"

	"github.com/spf13/cobra"
)

var (
	coredns_long = templates.LongDesc(`
		Deploy CoreDNS server, which then could be used as dns provider for federation.

	CoreDNS server is deployed inside a Kubernetes cluster. The host cluster must be specified using the
        --host-cluster-context flag.`)
	coredns_example = templates.Examples(`
		# Deploy the CoreDNS server in the host cluster whose local kubeconfig
		# context is bar.
		kubectl coredns --host-cluster-context=bar`)

	componentLabel = map[string]string{
		"app": "federated-cluster",
	}

	corednsPodLabelsAndSelector = map[string]string{
		"app":    "federated-cluster",
		"module": "federation-dns-server",
	}

	coreDNSNamespace = "kube-system"
	coreDNSNodeport  = 32112
)

func NewCmdDeployCoreDNS(cmdOut io.Writer, config util.AdminConfig) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "coredns --host-cluster-context=HOST_CONTEXT",
		Short:   "Deploy CoreDNS server",
		Long:    coredns_long,
		Example: coredns_example,
		Run: func(cmd *cobra.Command, args []string) {
			err := deployCoreDNSServer(cmdOut, config, cmd, args)
			cmdutil.CheckErr(err)
		},
	}

	cmd.Flags().String("kubeconfig", "", "Path to the kubeconfig file to use for CLI requests.")
	cmd.Flags().String("host-cluster-context", "", "Host cluster context")
	cmd.Flags().String("dns-zone-name", "", "DNS suffix for this federation. Federated Service DNS names are published with this suffix.")
	cmd.Flags().String("image", "", "Image to use for federation API server and controller manager binaries.")

	return cmd
}

func deployCoreDNSServer(cmdOut io.Writer, config util.AdminConfig, cmd *cobra.Command, args []string) error {
	host := cmdutil.GetFlagString(cmd, "host-cluster-context")
	kubeconfig := cmdutil.GetFlagString(cmd, "kubeconfig")
	dnsZoneName := cmdutil.GetFlagString(cmd, "dns-zone-name")
	image := cmdutil.GetFlagString(cmd, "image")

	if dnsZoneName == "" {
		return cmdutil.UsageError(cmd, "dns-zone-name is required")
	}

	hostFactory := config.HostFactory(host, kubeconfig)
	hostClientset, err := hostFactory.ClientSet()
	if err != nil {
		return err
	}

	_, err = createCoreDNSService(hostClientset, coreDNSNamespace, "federation-dns-server")
	if err != nil {
		return err
	}

	_, err = createCoreDNSEtcdService(hostClientset, coreDNSNamespace, "federation-dns-server-etcd")
	if err != nil {
		return err
	}

	_, err = createCoreDNSConfigmap(hostClientset, coreDNSNamespace, "coredns-config", dnsZoneName)
	if err != nil {
		return err
	}

	_, err = createCoreDNSServerDeployment(hostClientset, coreDNSNamespace, "federation-dns-server", image)
	if err != nil {
		return err
	}

	return nil
}

func createCoreDNSConfigmap(clientset *client.Clientset, namespace, configName, dnsZoneName string) (*api.ConfigMap, error) {
	configmap := &api.ConfigMap{
		ObjectMeta: api.ObjectMeta{
			Name:      configName,
			Namespace: namespace,
		},
		Data: map[string]string{
			"Corefile": ".:53 {\n    etcd " + dnsZoneName + " {\n        endpoint http://127.0.0.1:2379\n    }\n    loadbalance\n}\n",
		},
	}

	return clientset.Core().ConfigMaps(namespace).Create(configmap)
}

func createCoreDNSServerDeployment(clientset *client.Clientset, namespace, name, image string) (*extensions.Deployment, error) {
	dep := &extensions.Deployment{
		ObjectMeta: api.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    componentLabel,
		},
		Spec: extensions.DeploymentSpec{
			Replicas: 1,
			Template: api.PodTemplateSpec{
				ObjectMeta: api.ObjectMeta{
					Name:   name,
					Labels: corednsPodLabelsAndSelector,
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "etcd",
							Image: "quay.io/coreos/etcd:v2.3.3",
							Command: []string{
								"/etcd",
								"--data-dir",
								"/var/etcd/data",
								"--listen-client-urls",
								"http://0.0.0.0:2379",
								"--advertise-client-urls",
								"http://127.0.0.1:2379",
							},
							Ports: []api.ContainerPort{
								{
									Name:          "etcd",
									ContainerPort: 2379,
								},
							},
						},
						{
							Name:  "coredns",
							Image: image,
							Args: []string{
								"-conf",
								"/tmp/dns-config/Corefile",
							},
							Ports: []api.ContainerPort{
								{
									Name:          "dns",
									ContainerPort: 53,
									Protocol:      api.ProtocolUDP,
								},
							},
							VolumeMounts: []api.VolumeMount{
								{
									Name:      "config-volume",
									MountPath: "/tmp/dns-config",
									ReadOnly:  true,
								},
							},
						},
					},
					Volumes: []api.Volume{
						{
							Name: "config-volume",
							VolumeSource: api.VolumeSource{
								ConfigMap: &api.ConfigMapVolumeSource{
									LocalObjectReference: api.LocalObjectReference{Name: "coredns-config"},
								},
							},
						},
					},
				},
			},
		},
	}

	return clientset.Extensions().Deployments(namespace).Create(dep)
}

func createCoreDNSService(clientset *client.Clientset, namespace, name string) (*api.Service, error) {
	svc := &api.Service{
		ObjectMeta: api.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    componentLabel,
		},
		Spec: api.ServiceSpec{
			Type:     api.ServiceTypeNodePort,
			Selector: corednsPodLabelsAndSelector,
			Ports: []api.ServicePort{
				{
					Name:     "dns",
					Protocol: "UDP",
					Port:     53,
					NodePort: int32(coreDNSNodeport),
				},
			},
		},
	}

	return clientset.Core().Services(namespace).Create(svc)
}

func createCoreDNSEtcdService(clientset *client.Clientset, namespace, name string) (*api.Service, error) {
	svc := &api.Service{
		ObjectMeta: api.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    componentLabel,
		},
		Spec: api.ServiceSpec{
			Type:     api.ServiceTypeClusterIP,
			Selector: corednsPodLabelsAndSelector,
			Ports: []api.ServicePort{
				{
					Name:       "etcd",
					Protocol:   "TCP",
					Port:       2379,
					TargetPort: intstr.FromInt(2379),
				},
			},
		},
	}

	return clientset.Core().Services(namespace).Create(svc)
}
