/*
Copyright 2020 The Kubernetes Authors.

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

package endpointslice

import (
	"testing"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kubernetes/pkg/apis/discovery"
	"k8s.io/kubernetes/pkg/features"
	utilpointer "k8s.io/utils/pointer"
)

func Test_dropDisabledFieldsOnCreate(t *testing.T) {
	testcases := []struct {
		name                   string
		terminatingGateEnabled bool
		nodeNameGateEnabled    bool
		eps                    *discovery.EndpointSlice
		expectedEPS            *discovery.EndpointSlice
	}{
		{
			name:                   "terminating gate enabled, field should be allowed",
			terminatingGateEnabled: true,
			eps: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(false),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(true),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     nil,
							Terminating: nil,
						},
					},
				},
			},
			expectedEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(false),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(true),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     nil,
							Terminating: nil,
						},
					},
				},
			},
		},
		{
			name:                   "terminating gate disabled, field should be set to nil",
			terminatingGateEnabled: false,
			eps: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(false),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(true),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     nil,
							Terminating: nil,
						},
					},
				},
			},
			expectedEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Serving:     nil,
							Terminating: nil,
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     nil,
							Terminating: nil,
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     nil,
							Terminating: nil,
						},
					},
				},
			},
		},
		{
			name:                "node name gate enabled, field should be allowed",
			nodeNameGateEnabled: true,
			eps: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						NodeName: utilpointer.StringPtr("node-1"),
					},
					{
						NodeName: utilpointer.StringPtr("node-2"),
					},
				},
			},
			expectedEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						NodeName: utilpointer.StringPtr("node-1"),
					},
					{
						NodeName: utilpointer.StringPtr("node-2"),
					},
				},
			},
		},
		{
			name:                "node name gate disabled, field should be allowed",
			nodeNameGateEnabled: false,
			eps: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						NodeName: utilpointer.StringPtr("node-1"),
					},
					{
						NodeName: utilpointer.StringPtr("node-2"),
					},
				},
			},
			expectedEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						NodeName: nil,
					},
					{
						NodeName: nil,
					},
				},
			},
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.EndpointSliceTerminatingCondition, testcase.terminatingGateEnabled)()
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.EndpointSliceNodeName, testcase.nodeNameGateEnabled)()

			dropDisabledFieldsOnCreate(testcase.eps)
			if !apiequality.Semantic.DeepEqual(testcase.eps, testcase.expectedEPS) {
				t.Logf("actual endpointslice: %v", testcase.eps)
				t.Logf("expected endpointslice: %v", testcase.expectedEPS)
				t.Errorf("unexpected EndpointSlice on create API strategy")
			}
		})
	}
}

func Test_dropDisabledFieldsOnUpdate(t *testing.T) {
	testcases := []struct {
		name                   string
		terminatingGateEnabled bool
		nodeNameGateEnabled    bool
		oldEPS                 *discovery.EndpointSlice
		newEPS                 *discovery.EndpointSlice
		expectedEPS            *discovery.EndpointSlice
	}{
		{
			name:                   "terminating gate enabled, field should be allowed",
			terminatingGateEnabled: true,
			oldEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(false),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(true),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     nil,
							Terminating: nil,
						},
					},
				},
			},
			newEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(false),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(true),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     nil,
							Terminating: nil,
						},
					},
				},
			},
			expectedEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(false),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(true),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     nil,
							Terminating: nil,
						},
					},
				},
			},
		},
		{
			name:                   "terminating gate disabled, and not set on existing EPS",
			terminatingGateEnabled: false,
			oldEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Serving:     nil,
							Terminating: nil,
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     nil,
							Terminating: nil,
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     nil,
							Terminating: nil,
						},
					},
				},
			},
			newEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(false),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(true),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     nil,
							Terminating: nil,
						},
					},
				},
			},
			expectedEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Serving:     nil,
							Terminating: nil,
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     nil,
							Terminating: nil,
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     nil,
							Terminating: nil,
						},
					},
				},
			},
		},
		{
			name:                   "terminating gate disabled, and set on existing EPS",
			terminatingGateEnabled: false,
			oldEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(false),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(true),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     nil,
							Terminating: nil,
						},
					},
				},
			},
			newEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(false),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(true),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     nil,
							Terminating: nil,
						},
					},
				},
			},
			expectedEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(false),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(true),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     nil,
							Terminating: nil,
						},
					},
				},
			},
		},
		{
			name:                   "terminating gate disabled, and set on existing EPS with new values",
			terminatingGateEnabled: false,
			oldEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(false),
							Terminating: utilpointer.BoolPtr(false),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(true),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Terminating: nil,
						},
					},
				},
			},
			newEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(true),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(false),
							Terminating: utilpointer.BoolPtr(false),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Terminating: utilpointer.BoolPtr(false),
						},
					},
				},
			},
			expectedEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(true),
							Terminating: utilpointer.BoolPtr(true),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Serving:     utilpointer.BoolPtr(false),
							Terminating: utilpointer.BoolPtr(false),
						},
					},
					{
						Conditions: discovery.EndpointConditions{
							Terminating: utilpointer.BoolPtr(false),
						},
					},
				},
			},
		},
		{
			name:                "node name gate enabled, set on new EPS",
			nodeNameGateEnabled: true,
			oldEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						NodeName: nil,
					},
					{
						NodeName: nil,
					},
				},
			},
			newEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						NodeName: utilpointer.StringPtr("node-1"),
					},
					{
						NodeName: utilpointer.StringPtr("node-2"),
					},
				},
			},
			expectedEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						NodeName: utilpointer.StringPtr("node-1"),
					},
					{
						NodeName: utilpointer.StringPtr("node-2"),
					},
				},
			},
		},
		{
			name:                "node name gate disabled, set on new EPS",
			nodeNameGateEnabled: false,
			oldEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						NodeName: nil,
					},
					{
						NodeName: nil,
					},
				},
			},
			newEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						NodeName: utilpointer.StringPtr("node-1"),
					},
					{
						NodeName: utilpointer.StringPtr("node-2"),
					},
				},
			},
			expectedEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						NodeName: nil,
					},
					{
						NodeName: nil,
					},
				},
			},
		},
		{
			name:                "node name gate disabled, set on old and updated EPS",
			nodeNameGateEnabled: false,
			oldEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						NodeName: utilpointer.StringPtr("node-1-old"),
					},
					{
						NodeName: utilpointer.StringPtr("node-2-old"),
					},
				},
			},
			newEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						NodeName: utilpointer.StringPtr("node-1"),
					},
					{
						NodeName: utilpointer.StringPtr("node-2"),
					},
				},
			},
			expectedEPS: &discovery.EndpointSlice{
				Endpoints: []discovery.Endpoint{
					{
						NodeName: utilpointer.StringPtr("node-1"),
					},
					{
						NodeName: utilpointer.StringPtr("node-2"),
					},
				},
			},
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.EndpointSliceTerminatingCondition, testcase.terminatingGateEnabled)()
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.EndpointSliceNodeName, testcase.nodeNameGateEnabled)()

			dropDisabledFieldsOnUpdate(testcase.oldEPS, testcase.newEPS)
			if !apiequality.Semantic.DeepEqual(testcase.newEPS, testcase.expectedEPS) {
				t.Logf("actual endpointslice: %v", testcase.newEPS)
				t.Logf("expected endpointslice: %v", testcase.expectedEPS)
				t.Errorf("unexpected EndpointSlice from update API strategy")
			}
		})
	}
}
