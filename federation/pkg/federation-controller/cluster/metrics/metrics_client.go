/*
Copyright 2015 The Kubernetes Authors.

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

package metrics

import (
	"encoding/json"
	"fmt"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/v1"
	clientset "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"

	metrics_api "k8s.io/heapster/metrics/apis/metrics/v1alpha1"
)

const (
	DefaultHeapsterNamespace = "kube-system"
	DefaultHeapsterScheme    = "http"
	DefaultHeapsterService   = "heapster"
	DefaultHeapsterPort      = "" // use the first exposed port on the service
)

// MetricsClient is an interface for getting metrics for pods.
type MetricsClient interface {
	// GetClusterMetrics returns the aggregated metrics usage for all the nodes in the cluster.
	GetClusterMetrics() (*v1.ResourceList, error)
}

// HeapsterMetricsClient is Heapster-based implementation of MetricsClient
type HeapsterMetricsClient struct {
	client            clientset.Interface
	heapsterNamespace string
	heapsterScheme    string
	heapsterService   string
	heapsterPort      string
}

// NewHeapsterMetricsClient returns a new instance of Heapster-based implementation of MetricsClient interface.
func NewHeapsterMetricsClient(client clientset.Interface, namespace, scheme, service, port string) *HeapsterMetricsClient {
	return &HeapsterMetricsClient{
		client:            client,
		heapsterNamespace: namespace,
		heapsterScheme:    scheme,
		heapsterService:   service,
		heapsterPort:      port,
	}
}

func (h *HeapsterMetricsClient) GetClusterMetrics() (*v1.ResourceList, error) {
	metricPath := fmt.Sprintf("/apis/metrics/v1alpha1/nodes")
	params := map[string]string{"labelSelector": api.NamespaceAll}

	resultRaw, err := h.client.Core().Services(h.heapsterNamespace).
		ProxyGet(h.heapsterScheme, h.heapsterService, h.heapsterPort, metricPath, params).
		DoRaw()
	if err != nil {
		return nil, fmt.Errorf("failed to get node metrics: %v", err)
	}

	glog.V(4).Infof("Heapster metrics result: %s", string(resultRaw))

	metrics := make([]metrics_api.NodeMetrics, 0)
	err = json.Unmarshal(resultRaw, &metrics)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshall heapster response: %v", err)
	}

	usage := v1.ResourceList{}
	for _, nm := range metrics {
		usage = aggregate(usage, nm.Usage)
	}

	return &usage, nil
}

func aggregate(am, nm v1.ResourceList) v1.ResourceList {
	for resKey, resource := range nm {
		_, found := am[resKey]
		if !found {
			am[resKey] = resource
		}

		resource := am[resKey]
		pam := &resource
		pam.Add(nm[resKey])
		am[resKey] = resource
	}
	return am
}
