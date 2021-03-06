// Copyright © 2018 The Kubernetes Authors.
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

package actuators

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elb"
	"github.com/pkg/errors"
	"k8s.io/klog"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/apis/awsprovider/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
	client "sigs.k8s.io/cluster-api/pkg/client/clientset_generated/clientset/typed/cluster/v1alpha1"
)

// ScopeParams defines the input parameters used to create a new Scope.
type ScopeParams struct {
	AWSClients
	Cluster *clusterv1.Cluster
	Client  client.ClusterV1alpha1Interface
}

// NewScope creates a new Scope from the supplied parameters.
// This is meant to be called for each different actuator iteration.
func NewScope(params ScopeParams) (*Scope, error) {
	if params.Cluster == nil {
		return nil, errors.New("failed to generate new scope from nil cluster")
	}

	clusterConfig, err := v1alpha1.ClusterConfigFromProviderConfig(params.Cluster.Spec.ProviderConfig)
	if err != nil {
		return nil, errors.Errorf("failed to load cluster provider config: %v", err)
	}

	clusterStatus, err := v1alpha1.ClusterStatusFromProviderStatus(params.Cluster.Status.ProviderStatus)
	if err != nil {
		return nil, errors.Errorf("failed to load cluster provider status: %v", err)
	}

	session, err := session.NewSession(aws.NewConfig().WithRegion(clusterConfig.Region))
	if err != nil {
		return nil, errors.Errorf("failed to create aws session: %v", err)
	}

	if params.AWSClients.EC2 == nil {
		params.AWSClients.EC2 = ec2.New(session)
	}

	if params.AWSClients.ELB == nil {
		params.AWSClients.ELB = elb.New(session)
	}

	// TODO(vincepri): this was probably a temporary hack to pass down the region, it should be revisited.
	clusterStatus.Region = clusterConfig.Region

	var clusterClient client.ClusterInterface
	if params.Client != nil {
		clusterClient = params.Client.Clusters(params.Cluster.Namespace)
	}

	return &Scope{
		AWSClients:    params.AWSClients,
		Cluster:       params.Cluster,
		ClusterClient: clusterClient,
		ClusterConfig: clusterConfig,
		ClusterStatus: clusterStatus,
	}, nil
}

// Scope defines the basic context for an actuator to operate upon.
type Scope struct {
	AWSClients
	Cluster       *clusterv1.Cluster
	ClusterClient client.ClusterInterface
	ClusterConfig *v1alpha1.AWSClusterProviderConfig
	ClusterStatus *v1alpha1.AWSClusterProviderStatus
}

func (s *Scope) storeClusterConfig() error {
	ext, err := v1alpha1.EncodeClusterConfig(s.ClusterConfig)
	if err != nil {
		return err
	}

	s.Cluster.Spec.ProviderConfig.Value = ext

	if _, err := s.ClusterClient.Update(s.Cluster); err != nil {
		return err
	}

	return nil
}

func (s *Scope) storeClusterStatus() error {
	ext, err := v1alpha1.EncodeClusterStatus(s.ClusterStatus)
	if err != nil {
		return err
	}

	s.Cluster.Status.ProviderStatus = ext

	if _, err := s.ClusterClient.UpdateStatus(s.Cluster); err != nil {
		return err
	}

	return nil
}

// Close closes the current scope persisting the cluster configuration and status.
func (s *Scope) Close() {
	if s.ClusterClient == nil {
		return
	}

	if err := s.storeClusterConfig(); err != nil {
		klog.Errorf("[scope] failed to store provider config for cluster %q in namespace %q: %v", s.Cluster.Name, s.Cluster.Namespace, err)
	}

	if err := s.storeClusterStatus(); err != nil {
		klog.Errorf("[scope] failed to store provider status for cluster %q in namespace %q: %v", s.Cluster.Name, s.Cluster.Namespace, err)
	}
}

// MachineScope defines a scope defined around a machine and its cluster.
type MachineScope struct {
	*Scope

	Machine       *clusterv1.Machine
	MachineClient client.MachineInterface
	MachineConfig *v1alpha1.AWSMachineProviderConfig
	MachineStatus *v1alpha1.AWSMachineProviderStatus
}

func (m *MachineScope) Close() {
	defer m.Scope.Close()

	if m.MachineClient == nil {
		return
	}

	if _, err := m.MachineClient.Update(m.Machine); err != nil {
		klog.Errorf("[machinescope] failed to update machine: %v", err)
	}

	if err := m.storeMachineStatus(); err != nil {
		klog.Errorf("[machinescope] failed to store provider status for machine %q in namespace %q: %v", m.Machine.Name, m.Machine.Namespace, err)
	}
}

func (m *MachineScope) storeMachineStatus() error {
	ext, err := v1alpha1.EncodeMachineStatus(m.MachineStatus)
	if err != nil {
		return err
	}

	m.Machine.Status.ProviderStatus = ext

	if _, err := m.MachineClient.UpdateStatus(m.Machine); err != nil {
		return err
	}

	return nil
}

// MachineScopeParams defines the input parameters used to create a new MachineScope.
type MachineScopeParams struct {
	Cluster *clusterv1.Cluster
	Machine *clusterv1.Machine
	Client  client.ClusterV1alpha1Interface
}

// NewMachineScope creates a new MachineScope from the supplied parameters.
// This is meant to be called for each machine actuator operation.
func NewMachineScope(params MachineScopeParams) (*MachineScope, error) {
	scope, err := NewScope(ScopeParams{Client: params.Client, Cluster: params.Cluster})
	if err != nil {
		return nil, err
	}

	machineConfig, err := v1alpha1.MachineConfigFromProviderConfig(params.Machine.Spec.ProviderConfig)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get machine config")
	}

	machineStatus, err := v1alpha1.MachineStatusFromProviderStatus(params.Machine.Status.ProviderStatus)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get machine provider status")
	}

	var machineClient client.MachineInterface
	if params.Client != nil {
		machineClient = params.Client.Machines(params.Cluster.Namespace)
	}

	return &MachineScope{
		Scope:         scope,
		Machine:       params.Machine,
		MachineClient: machineClient,
		MachineConfig: machineConfig,
		MachineStatus: machineStatus,
	}, nil
}
