/*
Copyright 2019 The Kubernetes Authors.

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

package flagutil

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	prow "k8s.io/test-infra/prow/client/clientset/versioned"
	prowv1 "k8s.io/test-infra/prow/client/clientset/versioned/typed/prowjobs/v1"
	"k8s.io/test-infra/prow/kube"
)

// KubernetesOptions holds options for interacting with Kubernetes.
// These options are both useful for clients interacting with ProwJobs
// and other resources on the infrastructure cluster, as well as Pods
// on build clusters.
type KubernetesOptions struct {
	buildCluster string
	kubeconfig   string
	infraContext string

	DeckURI string

	// from resolution
	resolved                   bool
	dryRun                     bool
	prowJobClientset           prow.Interface
	kubernetesClientsByContext map[string]kubernetes.Interface
}

// AddFlags injects Kubernetes options into the given FlagSet.
func (o *KubernetesOptions) AddFlags(fs *flag.FlagSet) {
	fs.StringVar(&o.buildCluster, "build-cluster", "", "Path to kube.Cluster YAML file. If empty, uses the local cluster. All clusters are used as build clusters. Cannot be combined with --kubeconfig.")
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "Path to .kube/config file. If empty, uses the local cluster. All contexts other than the default or whichever is passed to --context are used as build clusters. . Cannot be combined with --build-cluster.")
	fs.StringVar(&o.infraContext, "context", "", "The name of the kubeconfig context to use for the infrastructure client. If empty and --kubeconfig is not set, uses the local cluster.")
	fs.StringVar(&o.DeckURI, "deck-url", "", "Deck URI for read-only access to the infrastructure cluster.")
}

// Validate validates Kubernetes options.
func (o *KubernetesOptions) Validate(dryRun bool) error {
	if dryRun && o.DeckURI == "" {
		return errors.New("a dry-run was requested but required flag -deck-url was unset")
	}

	if o.DeckURI != "" {
		if _, err := url.ParseRequestURI(o.DeckURI); err != nil {
			return fmt.Errorf("invalid -deck-url URI: %q", o.DeckURI)
		}
	}

	if o.kubeconfig != "" {
		if _, err := os.Stat(o.kubeconfig); err != nil {
			return fmt.Errorf("error accessing --kubeconfig: %v", err)
		}
	}

	if o.infraContext != "" && o.kubeconfig == "" {
		return errors.New("cannot provide --context without --kubeconfig")
	}

	if o.kubeconfig != "" && o.buildCluster != "" {
		return errors.New("must provide only --build-cluster OR --kubeconfig")
	}

	return nil
}

// resolve loads all of the clients we need and caches them for future calls.
func (o *KubernetesOptions) resolve(dryRun bool) (err error) {
	if o.resolved {
		return nil
	}

	o.dryRun = dryRun
	if dryRun {
		return nil
	}

	clusterConfigs, defaultContext, err := kube.LoadClusterConfigs(o.kubeconfig, o.buildCluster)
	clients := map[string]kubernetes.Interface{}
	for context, config := range clusterConfigs {
		client, err := kubernetes.NewForConfig(&config)
		if err != nil {
			return err
		}
		clients[context] = client
	}

	if o.infraContext == "" {
		o.infraContext = defaultContext
	}
	infraConfig, ok := clusterConfigs[o.infraContext]
	if !ok {
		return fmt.Errorf("resolved infrastructure cluster context to %q but did not find it in the kubeconfig", o.infraContext)
	}
	pjClient, err := prow.NewForConfig(&infraConfig)
	if err != nil {
		return err
	}

	o.prowJobClientset = pjClient
	o.kubernetesClientsByContext = clients
	o.resolved = true

	return nil
}

// ProwJobClientset returns a ProwJob clientset for use in informer factories.
func (o *KubernetesOptions) ProwJobClientset(namespace string, dryRun bool) (prowJobClientset prow.Interface, err error) {
	if o.dryRun {
		return nil, errors.New("no dry-run prowjob clientset is supported in dry-run mode")
	}

	if err := o.resolve(dryRun); err != nil {
		return nil, err
	}

	return o.prowJobClientset, nil
}

// ProwJobClient returns a ProwJob client.
func (o *KubernetesOptions) ProwJobClient(namespace string, dryRun bool) (prowJobClient prowv1.ProwJobInterface, err error) {
	if err := o.resolve(dryRun); err != nil {
		return nil, err
	}

	if o.dryRun {
		return kube.NewDryRunProwJobClient(o.DeckURI), nil
	}

	return o.prowJobClientset.ProwV1().ProwJobs(namespace), nil
}

// InfrastructureClusterClient returns a Kubernetes client for the infrastructure cluster.
func (o *KubernetesOptions) InfrastructureClusterClient(dryRun bool) (kubernetesClient kubernetes.Interface, err error) {
	if o.dryRun {
		return nil, errors.New("no dry-run kubernetes client is supported in dry-run mode")
	}

	if err := o.resolve(dryRun); err != nil {
		return nil, err
	}

	return o.kubernetesClientsByContext[o.infraContext], nil
}

// BuildClusterClients returns Pod clients for build clusters, mapped by their buildCluster alias, not by context.
func (o *KubernetesOptions) BuildClusterClients(namespace string, dryRun bool, knownAliases sets.String) (buildClusterClients map[string]corev1.PodInterface, err error) {
	if o.dryRun {
		return nil, errors.New("no dry-run pod client is supported for build clusters in dry-run mode")
	}

	if err := o.resolve(dryRun); err != nil {
		return nil, err
	}

	buildClusterAliases := sets.NewString()
	buildClients := map[string]corev1.PodInterface{}
	for alias, client := range contextsToAliases(o.kubernetesClientsByContext, o.infraContext) {
		buildClusterAliases.Insert(alias)
		buildClients[alias] = client.CoreV1().Pods(namespace)
	}
	if !buildClusterAliases.HasAll(knownAliases.UnsortedList()...) {
		return nil, fmt.Errorf("the following cluster aliases declared for jobs were not found in loaded clusters: %v", knownAliases.Difference(buildClusterAliases).List())
	}
	return buildClients, nil
}

// contextsToAliases remaps Kubernetes clients from context to alias. This is
// almost always a no-op except for when there is no literal "default" context
// provided in the input map. In this case, the default context is the infra
// cluster context (often the in-cluster context ("")) but the default alias
// is Prow-specific ("default") and we need to ensure that the default alias
// always exists.
func contextsToAliases(clientsByContext map[string]kubernetes.Interface, infraContext string) map[string]kubernetes.Interface {
	_, literalDefaultProvided := clientsByContext[kube.DefaultClusterAlias]
	clientsByAlias := map[string]kubernetes.Interface{}
	for context, client := range clientsByContext {
		alias := context
		if !literalDefaultProvided && context == infraContext {
			alias = kube.DefaultClusterAlias
		}
		clientsByAlias[alias] = client
	}
	return clientsByAlias
}
