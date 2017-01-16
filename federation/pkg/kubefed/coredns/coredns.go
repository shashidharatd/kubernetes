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

package coredns

import (
	"fmt"
	"io"
	"io/ioutil"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/federation/pkg/kubefed/util"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/apis/extensions"
	client "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	"k8s.io/kubernetes/pkg/kubectl/cmd/templates"
	cmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"

	"github.com/spf13/cobra"
)

const (
	etcdPort                  = 2379
	coreDNSListenPort         = 53
	coreDNSNodeport           = 32112
	coreDNSName               = "federation-dns-server"
	coreDNSNamespace          = "kube-system"
	coreDNSEtcd               = coreDNSName + "-etcd"
	coreDNSConfigMap          = "coredns-config"
	dnsProviderConfigFilename = "/tmp/federation-dns-provider.conf"

	lbAddrRetryInterval = 5 * time.Second
	podWaitInterval     = 2 * time.Second
)

var (
	coredns_long = templates.LongDesc(`
		Deploy CoreDNS server, which then could be used as dns provider for federation.

	CoreDNS server is deployed inside a Kubernetes cluster. The host cluster must be specified using the
        --host-cluster-context flag.`)
	coredns_example = templates.Examples(`
		# Deploy the CoreDNS server in the host cluster whose local kubeconfig context is bar.
		kubectl coredns --host-cluster-context=bar`)

	componentLabel = map[string]string{
		"app": "federated-cluster",
	}

	corednsPodLabelsAndSelector = map[string]string{
		"app":    "federated-cluster",
		"module": coreDNSName,
	}
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
	cmd.Flags().String("coredns-service-type", "nodeport", "Service type to use for CoreDNS. Options: 'nodeport' (default), 'loadbalancer'.")

	return cmd
}

func deployCoreDNSServer(cmdOut io.Writer, config util.AdminConfig, cmd *cobra.Command, args []string) error {
	host := cmdutil.GetFlagString(cmd, "host-cluster-context")
	kubeconfig := cmdutil.GetFlagString(cmd, "kubeconfig")
	dnsZoneName := cmdutil.GetFlagString(cmd, "dns-zone-name")
	image := cmdutil.GetFlagString(cmd, "image")
	corednsServiceType := cmdutil.GetFlagString(cmd, "coredns-service-type")

	if dnsZoneName == "" {
		return cmdutil.UsageError(cmd, "dns-zone-name is required")
	}

	if image == "" {
		return cmdutil.UsageError(cmd, "image is required")
	}

	hostFactory := config.HostFactory(host, kubeconfig)
	hostClientset, err := hostFactory.ClientSet()
	if err != nil {
		return err
	}

	coreDNSAddress, err := createCoreDNSService(hostClientset, coreDNSNamespace, coreDNSName, corednsServiceType)
	if err != nil {
		return err
	}

	_, err = createCoreDNSEtcdService(hostClientset, coreDNSNamespace, coreDNSEtcd)
	if err != nil {
		return err
	}

	_, err = createCoreDNSConfigmap(hostClientset, coreDNSNamespace, coreDNSConfigMap, dnsZoneName)
	if err != nil {
		return err
	}

	_, err = createCoreDNSServerDeployment(hostClientset, coreDNSNamespace, coreDNSName, image)
	if err != nil {
		return err
	}

	err = WaitForPods(hostClientset, []string{coreDNSName}, coreDNSNamespace)
	if err != nil {
		return err
	}

	fmt.Fprintf(cmdOut, "CoreDNS server is running at: %s\n", coreDNSAddress)

	err = createDNSProviderConfig(dnsZoneName)
	if err != nil {
		return err
	}

	return nil
}

func createCoreDNSConfigmap(clientset *client.Clientset, namespace, configName, dnsZoneName string) (*api.ConfigMap, error) {
	configmap := &api.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configName,
			Namespace: namespace,
		},
		Data: map[string]string{
			"Corefile": ".:" + strconv.Itoa(coreDNSListenPort) + " {\n" +
				"    etcd " + dnsZoneName + " {\n" +
				"        endpoint http://127.0.0.1:" + strconv.Itoa(etcdPort) + "\n" +
				"    }\n" +
				"    loadbalance\n" +
				"}\n",
		},
	}

	return clientset.Core().ConfigMaps(namespace).Create(configmap)
}

func createCoreDNSServerDeployment(clientset *client.Clientset, namespace, name, image string) (*extensions.Deployment, error) {
	dep := &extensions.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    componentLabel,
		},
		Spec: extensions.DeploymentSpec{
			Replicas: 1,
			Template: api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:   name,
					Labels: corednsPodLabelsAndSelector,
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "etcd",
							Image: "172.17.0.1:5000/etcd:v2.3.3",
							Command: []string{
								"/etcd",
								"--data-dir",
								"/var/etcd/data",
								"--listen-client-urls",
								"http://0.0.0.0:" + strconv.Itoa(etcdPort),
								"--advertise-client-urls",
								"http://127.0.0.1:" + strconv.Itoa(etcdPort),
							},
							Ports: []api.ContainerPort{
								{
									Name:          "etcd",
									ContainerPort: etcdPort,
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
									ContainerPort: coreDNSListenPort,
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
									LocalObjectReference: api.LocalObjectReference{Name: coreDNSConfigMap},
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

func createCoreDNSService(clientset *client.Clientset, namespace, name, serviceType string) (string, error) {
	svc := &api.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    componentLabel,
		},
		Spec: api.ServiceSpec{
			Type:     api.ServiceTypeLoadBalancer,
			Selector: corednsPodLabelsAndSelector,
			Ports: []api.ServicePort{
				{
					Name:     "dns",
					Protocol: "UDP",
					Port:     coreDNSListenPort,
				},
			},
		},
	}

	if serviceType == "nodeport" {
		svc.Spec.Type = api.ServiceTypeNodePort
		svc.Spec.Ports[0].NodePort = int32(coreDNSNodeport)
	}

	svc, err := clientset.Core().Services(namespace).Create(svc)
	if err != nil {
		return "", err
	}

	coreDNSAddress := ""
	if serviceType == "nodeport" {
		coreDNSAddress, err = GetFirstNodeIpOfCluster(clientset)
		if err != nil {
			return "", err
		}
		coreDNSAddress = coreDNSAddress + ":" + strconv.Itoa(coreDNSNodeport)
	} else {
		ips, hostnames, err := WaitForLoadBalancerAddress(clientset, svc)
		if err != nil {
			return "", err
		}
		if len(ips) > 0 {
			coreDNSAddress = ips[0]
		} else if len(hostnames) > 0 {
			coreDNSAddress = hostnames[0]
		}
		coreDNSAddress = coreDNSAddress + ":" + strconv.Itoa(coreDNSListenPort)
	}

	return coreDNSAddress, nil
}

func createCoreDNSEtcdService(clientset *client.Clientset, namespace, name string) (*api.Service, error) {
	svc := &api.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    componentLabel,
		},
		Spec: api.ServiceSpec{
			Type:     api.ServiceTypeClusterIP,
			Selector: corednsPodLabelsAndSelector,
			Ports: []api.ServicePort{
				{
					Name:     "etcd",
					Protocol: "TCP",
					Port:     etcdPort,
				},
			},
		},
	}

	return clientset.Core().Services(namespace).Create(svc)
}

func createDNSProviderConfig(dnsZoneName string) error {
	configFileBytes := []byte(
		"[Global]\n" +
		"etcd-endpoints = http://" + coreDNSEtcd + "." + coreDNSNamespace + ":" + strconv.Itoa(etcdPort) + "\n" +
		"zones = " + dnsZoneName + "\n")

	err := ioutil.WriteFile(dnsProviderConfigFilename, configFileBytes, 0644)
	return err
}

func WaitForLoadBalancerAddress(clientset *client.Clientset, svc *api.Service) ([]string, []string, error) {
	ips := []string{}
	hostnames := []string{}

	err := wait.PollImmediateInfinite(lbAddrRetryInterval, func() (bool, error) {
		pollSvc, err := clientset.Core().Services(svc.Namespace).Get(svc.Name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		if ings := pollSvc.Status.LoadBalancer.Ingress; len(ings) > 0 {
			for _, ing := range ings {
				if len(ing.IP) > 0 {
					ips = append(ips, ing.IP)
				}
				if len(ing.Hostname) > 0 {
					hostnames = append(hostnames, ing.Hostname)
				}
			}
			if len(ips) > 0 || len(hostnames) > 0 {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return nil, nil, err
	}

	return ips, hostnames, nil
}

func WaitForPods(clientset *client.Clientset, pods []string, namespace string) error {
	err := wait.PollInfinite(podWaitInterval, func() (bool, error) {
		podCheck := len(pods)
		podList, err := clientset.Core().Pods(namespace).List(metav1.ListOptions{})
		if err != nil {
			return false, nil
		}
		for _, pod := range podList.Items {
			for _, fedPod := range pods {
				if strings.HasPrefix(pod.Name, fedPod) && pod.Status.Phase == "Running" {
					podCheck -= 1
				}
			}
			//ensure that all pods are in running state or keep waiting
			if podCheck == 0 {
				return true, nil
			}
		}
		return false, nil
	})
	return err
}

func GetFirstNodeIpOfCluster(clientset *client.Clientset) (string, error) {
	preferredAddressTypes := []api.NodeAddressType{
		api.NodeExternalIP,
		api.NodeLegacyHostIP,
		api.NodeInternalIP,
	}

	nodes, err := clientset.Nodes().List(metav1.ListOptions{})
	if err != nil {
		return "", err
	} else if len(nodes.Items) < 1 {
		return "", fmt.Errorf("No nodes in the cluster")
	}

	// TODO: use nodeutil.GetPreferredNodeAddress
	address, err := GetPreferredNodeAddress(&nodes.Items[0], preferredAddressTypes)
	if err != nil {
		return "", err
	}

	return address, nil
}

// GetPreferredNodeAddress returns the address of the provided node, using the provided preference order.
// If none of the preferred address types are found, an error is returned.
func GetPreferredNodeAddress(node *api.Node, preferredAddressTypes []api.NodeAddressType) (string, error) {
	for _, addressType := range preferredAddressTypes {
		for _, address := range node.Status.Addresses {
			if address.Type == addressType {
				return address.Address, nil
			}
		}
		// If hostname was requested and no Hostname address was registered...
		if addressType == api.NodeHostName {
			// ...fall back to the kubernetes.io/hostname label for compatibility with kubelets before 1.5
			if hostname, ok := node.Labels[metav1.LabelHostname]; ok && len(hostname) > 0 {
				return hostname, nil
			}
		}
	}
	return "", fmt.Errorf("no preferred addresses found; known addresses: %v", node.Status.Addresses)
}
