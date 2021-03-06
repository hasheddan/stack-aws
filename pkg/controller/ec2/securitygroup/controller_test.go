/*
Copyright 2019 The Crossplane Authors.

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

package securitygroup

import (
	"context"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	runtimev1alpha1 "github.com/crossplane/crossplane-runtime/apis/core/v1alpha1"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/test"

	"github.com/crossplane/provider-aws/apis/ec2/v1beta1"
	awsv1alpha3 "github.com/crossplane/provider-aws/apis/v1alpha3"
	awsclients "github.com/crossplane/provider-aws/pkg/clients"
	"github.com/crossplane/provider-aws/pkg/clients/ec2"
	"github.com/crossplane/provider-aws/pkg/clients/ec2/fake"
)

const (
	providerName    = "aws-creds"
	secretNamespace = "crossplane-system"
	testRegion      = "us-east-1"

	connectionSecretName = "my-little-secret"
	secretKey            = "credentials"
	credData             = "confidential!"
)

var (
	sgID              = "some sgID"
	port80      int64 = 80
	port100     int64 = 100
	cidr              = "192.168.0.0/32"
	tcpProtocol       = "tcp"

	errBoom = errors.New("boom")
)

type args struct {
	sg   ec2.SecurityGroupClient
	kube client.Client
	cr   *v1beta1.SecurityGroup
}

type sgModifier func(*v1beta1.SecurityGroup)

func specPermissions() []v1beta1.IPPermission {
	return []v1beta1.IPPermission{
		{
			FromPort: aws.Int64(port80),
			ToPort:   aws.Int64(80),
			IPRanges: []v1beta1.IPRange{
				{CIDRIP: cidr},
			},
			IPProtocol: tcpProtocol,
		},
	}
}

func sgPersmissions() []awsec2.IpPermission {
	return []awsec2.IpPermission{
		{
			FromPort:   aws.Int64(port100),
			ToPort:     aws.Int64(port100),
			IpProtocol: aws.String(tcpProtocol),
			IpRanges: []awsec2.IpRange{{
				CidrIp: aws.String(cidr),
			}},
		},
	}
}

func withExternalName(name string) sgModifier {
	return func(r *v1beta1.SecurityGroup) { meta.SetExternalName(r, name) }
}

func withSpec(p v1beta1.SecurityGroupParameters) sgModifier {
	return func(r *v1beta1.SecurityGroup) { r.Spec.ForProvider = p }
}

func withStatus(s v1beta1.SecurityGroupObservation) sgModifier {
	return func(r *v1beta1.SecurityGroup) { r.Status.AtProvider = s }
}

func withConditions(c ...runtimev1alpha1.Condition) sgModifier {
	return func(r *v1beta1.SecurityGroup) { r.Status.ConditionedStatus.Conditions = c }
}

func sg(m ...sgModifier) *v1beta1.SecurityGroup {
	cr := &v1beta1.SecurityGroup{
		Spec: v1beta1.SecurityGroupSpec{
			ResourceSpec: runtimev1alpha1.ResourceSpec{
				ProviderReference: runtimev1alpha1.Reference{Name: providerName},
			},
		},
	}
	for _, f := range m {
		f(cr)
	}
	return cr
}

var _ managed.ExternalClient = &external{}
var _ managed.ExternalConnecter = &connector{}

func TestConnect(t *testing.T) {
	secret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      connectionSecretName,
			Namespace: secretNamespace,
		},
		Data: map[string][]byte{
			secretKey: []byte(credData),
		},
	}

	providerSA := func(saVal bool) awsv1alpha3.Provider {
		return awsv1alpha3.Provider{
			Spec: awsv1alpha3.ProviderSpec{
				Region:            testRegion,
				UseServiceAccount: &saVal,
				ProviderSpec: runtimev1alpha1.ProviderSpec{
					CredentialsSecretRef: &runtimev1alpha1.SecretKeySelector{
						SecretReference: runtimev1alpha1.SecretReference{
							Namespace: secretNamespace,
							Name:      connectionSecretName,
						},
						Key: secretKey,
					},
				},
			},
		}
	}
	type args struct {
		kube        client.Client
		newClientFn func(ctx context.Context, credentials []byte, region string, auth awsclients.AuthMethod) (ec2.SecurityGroupClient, error)
		cr          *v1beta1.SecurityGroup
	}
	type want struct {
		err error
	}

	cases := map[string]struct {
		args
		want
	}{
		"Successful": {
			args: args{
				kube: &test.MockClient{
					MockGet: func(_ context.Context, key client.ObjectKey, obj runtime.Object) error {
						switch key {
						case client.ObjectKey{Name: providerName}:
							p := providerSA(false)
							p.DeepCopyInto(obj.(*awsv1alpha3.Provider))
							return nil
						case client.ObjectKey{Namespace: secretNamespace, Name: connectionSecretName}:
							secret.DeepCopyInto(obj.(*corev1.Secret))
							return nil
						}
						return errBoom
					},
				},
				newClientFn: func(_ context.Context, credentials []byte, region string, _ awsclients.AuthMethod) (i ec2.SecurityGroupClient, e error) {
					if diff := cmp.Diff(credData, string(credentials)); diff != "" {
						t.Errorf("r: -want, +got:\n%s", diff)
					}
					if diff := cmp.Diff(testRegion, region); diff != "" {
						t.Errorf("r: -want, +got:\n%s", diff)
					}
					return nil, nil
				},
				cr: sg(),
			},
		},
		"SuccessfulUseServiceAccount": {
			args: args{
				kube: &test.MockClient{
					MockGet: func(_ context.Context, key client.ObjectKey, obj runtime.Object) error {
						if key == (client.ObjectKey{Name: providerName}) {
							p := providerSA(true)
							p.DeepCopyInto(obj.(*awsv1alpha3.Provider))
							return nil
						}
						return errBoom
					},
				},
				newClientFn: func(_ context.Context, credentials []byte, region string, _ awsclients.AuthMethod) (i ec2.SecurityGroupClient, e error) {
					if diff := cmp.Diff("", string(credentials)); diff != "" {
						t.Errorf("r: -want, +got:\n%s", diff)
					}
					if diff := cmp.Diff(testRegion, region); diff != "" {
						t.Errorf("r: -want, +got:\n%s", diff)
					}
					return nil, nil
				},
				cr: sg(),
			},
		},
		"ProviderGetFailed": {
			args: args{
				kube: &test.MockClient{
					MockGet: func(_ context.Context, key client.ObjectKey, obj runtime.Object) error {
						return errBoom
					},
				},
				cr: sg(),
			},
			want: want{
				err: errors.Wrap(errBoom, errGetProvider),
			},
		},
		"SecretGetFailed": {
			args: args{
				kube: &test.MockClient{
					MockGet: func(_ context.Context, key client.ObjectKey, obj runtime.Object) error {
						switch key {
						case client.ObjectKey{Name: providerName}:
							p := providerSA(false)
							p.DeepCopyInto(obj.(*awsv1alpha3.Provider))
							return nil
						case client.ObjectKey{Namespace: secretNamespace, Name: connectionSecretName}:
							return errBoom
						default:
							return nil
						}
					},
				},
				cr: sg(),
			},
			want: want{
				err: errors.Wrap(errBoom, errGetProviderSecret),
			},
		},
		"SecretGetFailedNil": {
			args: args{
				kube: &test.MockClient{
					MockGet: func(_ context.Context, key client.ObjectKey, obj runtime.Object) error {
						switch key {
						case client.ObjectKey{Name: providerName}:
							p := providerSA(false)
							p.SetCredentialsSecretReference(nil)
							p.DeepCopyInto(obj.(*awsv1alpha3.Provider))
							return nil
						case client.ObjectKey{Namespace: secretNamespace, Name: connectionSecretName}:
							return errBoom
						default:
							return nil
						}
					},
				},
				cr: sg(),
			},
			want: want{
				err: errors.New(errGetProviderSecret),
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := &connector{kube: tc.kube, newClientFn: tc.newClientFn}
			_, err := c.Connect(context.Background(), tc.args.cr)
			if diff := cmp.Diff(tc.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("r: -want, +got:\n%s", diff)
			}
		})
	}
}

func TestObserve(t *testing.T) {
	type want struct {
		cr     *v1beta1.SecurityGroup
		result managed.ExternalObservation
		err    error
	}

	cases := map[string]struct {
		args
		want
	}{
		"SuccessfulAvailable": {
			args: args{
				sg: &fake.MockSecurityGroupClient{
					MockDescribe: func(input *awsec2.DescribeSecurityGroupsInput) awsec2.DescribeSecurityGroupsRequest {
						return awsec2.DescribeSecurityGroupsRequest{
							Request: &aws.Request{HTTPRequest: &http.Request{}, Retryer: aws.NoOpRetryer{}, Data: &awsec2.DescribeSecurityGroupsOutput{
								SecurityGroups: []awsec2.SecurityGroup{{}},
							}},
						}
					},
				},
				cr: sg(withStatus(v1beta1.SecurityGroupObservation{
					SecurityGroupID: sgID,
				}),
					withExternalName(sgID)),
			},
			want: want{
				cr: sg(withExternalName(sgID),
					withConditions(runtimev1alpha1.Available())),
				result: managed.ExternalObservation{
					ResourceExists:   true,
					ResourceUpToDate: true,
				},
			},
		},
		"MultipleSGs": {
			args: args{
				sg: &fake.MockSecurityGroupClient{
					MockDescribe: func(input *awsec2.DescribeSecurityGroupsInput) awsec2.DescribeSecurityGroupsRequest {
						return awsec2.DescribeSecurityGroupsRequest{
							Request: &aws.Request{HTTPRequest: &http.Request{}, Retryer: aws.NoOpRetryer{}, Data: &awsec2.DescribeSecurityGroupsOutput{
								SecurityGroups: []awsec2.SecurityGroup{{}, {}},
							}},
						}
					},
				},
				cr: sg(withStatus(v1beta1.SecurityGroupObservation{
					SecurityGroupID: sgID,
				}),
					withExternalName(sgID)),
			},
			want: want{
				cr: sg(withStatus(v1beta1.SecurityGroupObservation{
					SecurityGroupID: sgID,
				}),
					withExternalName(sgID)),
				err: errors.New(errMultipleItems),
			},
		},
		"DescribeFailure": {
			args: args{
				sg: &fake.MockSecurityGroupClient{
					MockDescribe: func(input *awsec2.DescribeSecurityGroupsInput) awsec2.DescribeSecurityGroupsRequest {
						return awsec2.DescribeSecurityGroupsRequest{
							Request: &aws.Request{HTTPRequest: &http.Request{}, Error: errBoom},
						}
					},
				},
				cr: sg(withStatus(v1beta1.SecurityGroupObservation{
					SecurityGroupID: sgID,
				}),
					withExternalName(sgID)),
			},
			want: want{
				cr: sg(withStatus(v1beta1.SecurityGroupObservation{
					SecurityGroupID: sgID,
				}),
					withExternalName(sgID)),
				err: errors.Wrap(errBoom, errDescribe),
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := &external{kube: tc.kube, sg: tc.sg}
			o, err := e.Observe(context.Background(), tc.args.cr)

			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("r: -want, +got:\n%s", diff)
			}
			if diff := cmp.Diff(tc.want.cr, tc.args.cr, test.EquateConditions()); diff != "" {
				t.Errorf("r: -want, +got:\n%s", diff)
			}
			if diff := cmp.Diff(tc.want.result, o); diff != "" {
				t.Errorf("r: -want, +got:\n%s", diff)
			}
		})
	}
}

func TestCreate(t *testing.T) {
	type want struct {
		cr     *v1beta1.SecurityGroup
		result managed.ExternalCreation
		err    error
	}

	cases := map[string]struct {
		args
		want
	}{
		"Successful": {
			args: args{
				kube: &test.MockClient{
					MockUpdate:       test.NewMockClient().Update,
					MockStatusUpdate: test.NewMockClient().MockStatusUpdate,
				},
				sg: &fake.MockSecurityGroupClient{
					MockCreate: func(input *awsec2.CreateSecurityGroupInput) awsec2.CreateSecurityGroupRequest {
						return awsec2.CreateSecurityGroupRequest{
							Request: &aws.Request{HTTPRequest: &http.Request{}, Retryer: aws.NoOpRetryer{}, Data: &awsec2.CreateSecurityGroupOutput{
								GroupId: aws.String(sgID),
							}},
						}
					},
				},
				cr: sg(),
			},
			want: want{
				cr: sg(withExternalName(sgID),
					withConditions(runtimev1alpha1.Creating())),
			},
		},
		"CreateFail": {
			args: args{
				kube: &test.MockClient{
					MockUpdate:       test.NewMockClient().Update,
					MockStatusUpdate: test.NewMockClient().MockStatusUpdate,
				},
				sg: &fake.MockSecurityGroupClient{
					MockCreate: func(input *awsec2.CreateSecurityGroupInput) awsec2.CreateSecurityGroupRequest {
						return awsec2.CreateSecurityGroupRequest{
							Request: &aws.Request{HTTPRequest: &http.Request{}, Error: errBoom},
						}
					},
				},
				cr: sg(),
			},
			want: want{
				cr:  sg(withConditions(runtimev1alpha1.Creating())),
				err: errors.Wrap(errBoom, errCreate),
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := &external{kube: tc.kube, sg: tc.sg}
			o, err := e.Create(context.Background(), tc.args.cr)

			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("r: -want, +got:\n%s", diff)
			}
			if diff := cmp.Diff(tc.want.cr, tc.args.cr, test.EquateConditions()); diff != "" {
				t.Errorf("r: -want, +got:\n%s", diff)
			}
			if diff := cmp.Diff(tc.want.result, o); diff != "" {
				t.Errorf("r: -want, +got:\n%s", diff)
			}
		})
	}
}

func TestUpdate(t *testing.T) {
	type want struct {
		cr     *v1beta1.SecurityGroup
		result managed.ExternalUpdate
		err    error
	}

	cases := map[string]struct {
		args
		want
	}{
		"Successful": {
			args: args{
				sg: &fake.MockSecurityGroupClient{
					MockDescribe: func(input *awsec2.DescribeSecurityGroupsInput) awsec2.DescribeSecurityGroupsRequest {
						return awsec2.DescribeSecurityGroupsRequest{
							Request: &aws.Request{HTTPRequest: &http.Request{}, Retryer: aws.NoOpRetryer{}, Data: &awsec2.DescribeSecurityGroupsOutput{
								SecurityGroups: []awsec2.SecurityGroup{{
									IpPermissions:       sgPersmissions(),
									IpPermissionsEgress: sgPersmissions(),
								}},
							}},
						}
					},
					MockAuthorizeIgress: func(input *awsec2.AuthorizeSecurityGroupIngressInput) awsec2.AuthorizeSecurityGroupIngressRequest {
						return awsec2.AuthorizeSecurityGroupIngressRequest{
							Request: &aws.Request{HTTPRequest: &http.Request{}, Retryer: aws.NoOpRetryer{}, Data: &awsec2.AuthorizeSecurityGroupIngressOutput{}},
						}
					},
					MockAuthorizeEgress: func(input *awsec2.AuthorizeSecurityGroupEgressInput) awsec2.AuthorizeSecurityGroupEgressRequest {
						return awsec2.AuthorizeSecurityGroupEgressRequest{
							Request: &aws.Request{HTTPRequest: &http.Request{}, Retryer: aws.NoOpRetryer{}, Data: &awsec2.AuthorizeSecurityGroupEgressOutput{}},
						}
					},
				},
				cr: sg(withSpec(v1beta1.SecurityGroupParameters{
					Ingress: specPermissions(),
					Egress:  specPermissions(),
				}),
					withStatus(v1beta1.SecurityGroupObservation{
						SecurityGroupID: sgID,
					})),
			},
			want: want{
				cr: sg(withSpec(v1beta1.SecurityGroupParameters{
					Ingress: specPermissions(),
					Egress:  specPermissions(),
				}),
					withStatus(v1beta1.SecurityGroupObservation{
						SecurityGroupID: sgID,
					})),
			},
		},
		"IngressFail": {
			args: args{
				sg: &fake.MockSecurityGroupClient{
					MockDescribe: func(input *awsec2.DescribeSecurityGroupsInput) awsec2.DescribeSecurityGroupsRequest {
						return awsec2.DescribeSecurityGroupsRequest{
							Request: &aws.Request{HTTPRequest: &http.Request{}, Retryer: aws.NoOpRetryer{}, Data: &awsec2.DescribeSecurityGroupsOutput{
								SecurityGroups: []awsec2.SecurityGroup{{
									IpPermissions:       sgPersmissions(),
									IpPermissionsEgress: sgPersmissions(),
								}},
							}},
						}
					},
					MockAuthorizeIgress: func(input *awsec2.AuthorizeSecurityGroupIngressInput) awsec2.AuthorizeSecurityGroupIngressRequest {
						return awsec2.AuthorizeSecurityGroupIngressRequest{
							Request: &aws.Request{HTTPRequest: &http.Request{}, Error: errBoom},
						}
					},
				},
				cr: sg(withSpec(v1beta1.SecurityGroupParameters{
					Ingress: specPermissions(),
					Egress:  specPermissions(),
				}),
					withStatus(v1beta1.SecurityGroupObservation{
						SecurityGroupID: sgID,
					})),
			},
			want: want{
				cr: sg(withSpec(v1beta1.SecurityGroupParameters{
					Ingress: specPermissions(),
					Egress:  specPermissions(),
				}),
					withStatus(v1beta1.SecurityGroupObservation{
						SecurityGroupID: sgID,
					})),
				err: errors.Wrap(errBoom, errAuthorizeIngress),
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := &external{kube: tc.kube, sg: tc.sg}
			o, err := e.Update(context.Background(), tc.args.cr)

			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("r: -want, +got:\n%s", diff)
			}
			if diff := cmp.Diff(tc.want.cr, tc.args.cr, test.EquateConditions()); diff != "" {
				t.Errorf("r: -want, +got:\n%s", diff)
			}
			if diff := cmp.Diff(tc.want.result, o); diff != "" {
				t.Errorf("r: -want, +got:\n%s", diff)
			}
		})
	}
}

func TestDelete(t *testing.T) {
	type want struct {
		cr  *v1beta1.SecurityGroup
		err error
	}

	cases := map[string]struct {
		args
		want
	}{
		"Successful": {
			args: args{
				sg: &fake.MockSecurityGroupClient{
					MockDelete: func(input *awsec2.DeleteSecurityGroupInput) awsec2.DeleteSecurityGroupRequest {
						return awsec2.DeleteSecurityGroupRequest{
							Request: &aws.Request{HTTPRequest: &http.Request{}, Retryer: aws.NoOpRetryer{}, Data: &awsec2.DeleteSecurityGroupOutput{}},
						}
					},
				},
				cr: sg(withStatus(v1beta1.SecurityGroupObservation{
					SecurityGroupID: sgID,
				})),
			},
			want: want{
				cr: sg(withStatus(v1beta1.SecurityGroupObservation{
					SecurityGroupID: sgID,
				}), withConditions(runtimev1alpha1.Deleting())),
			},
		},
		"InvalidSgId": {
			args: args{
				sg: &fake.MockSecurityGroupClient{
					MockDelete: func(input *awsec2.DeleteSecurityGroupInput) awsec2.DeleteSecurityGroupRequest {
						return awsec2.DeleteSecurityGroupRequest{
							Request: &aws.Request{HTTPRequest: &http.Request{}, Retryer: aws.NoOpRetryer{}, Data: &awsec2.DeleteSecurityGroupOutput{}},
						}
					},
				},
				cr: sg(),
			},
			want: want{
				cr: sg(withConditions(runtimev1alpha1.Deleting())),
			},
		},
		"DeleteFailure": {
			args: args{
				sg: &fake.MockSecurityGroupClient{
					MockDelete: func(input *awsec2.DeleteSecurityGroupInput) awsec2.DeleteSecurityGroupRequest {
						return awsec2.DeleteSecurityGroupRequest{
							Request: &aws.Request{HTTPRequest: &http.Request{}, Error: errBoom},
						}
					},
				},
				cr: sg(withStatus(v1beta1.SecurityGroupObservation{
					SecurityGroupID: sgID,
				})),
			},
			want: want{
				cr: sg(withStatus(v1beta1.SecurityGroupObservation{
					SecurityGroupID: sgID,
				}), withConditions(runtimev1alpha1.Deleting())),
				err: errors.Wrap(errBoom, errDelete),
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := &external{kube: tc.kube, sg: tc.sg}
			err := e.Delete(context.Background(), tc.args.cr)

			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("r: -want, +got:\n%s", diff)
			}
			if diff := cmp.Diff(tc.want.cr, tc.args.cr, test.EquateConditions()); diff != "" {
				t.Errorf("r: -want, +got:\n%s", diff)
			}
		})
	}
}
