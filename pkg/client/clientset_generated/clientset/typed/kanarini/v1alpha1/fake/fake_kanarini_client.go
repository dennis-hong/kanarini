// Generated file, do not modify manually!

// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	v1alpha1 "github.com/nilebox/kanarini/pkg/client/clientset_generated/clientset/typed/kanarini/v1alpha1"
	rest "k8s.io/client-go/rest"
	testing "k8s.io/client-go/testing"
)

type FakeKanariniV1alpha1 struct {
	*testing.Fake
}

func (c *FakeKanariniV1alpha1) CanaryDeployments(namespace string) v1alpha1.CanaryDeploymentInterface {
	return &FakeCanaryDeployments{c, namespace}
}

// RESTClient returns a RESTClient that is used to communicate
// with API server by this client implementation.
func (c *FakeKanariniV1alpha1) RESTClient() rest.Interface {
	var ret *rest.RESTClient
	return ret
}