/*
Copyright 2017 The Kubernetes Authors.

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

package plank

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"text/template"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	kapierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakectrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	prowapi "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/kube"
	"sigs.k8s.io/prow/pkg/pjutil"
	"sigs.k8s.io/prow/pkg/testutil"
)

type fca struct {
	sync.Mutex
	c *config.Config
}

const (
	podPendingTimeout     = time.Hour
	podRunningTimeout     = time.Hour * 2
	podUnscheduledTimeout = time.Minute * 5

	podDeletionPreventionFinalizer = "keep-from-vanishing"
)

var maxRevivals = 3

func newFakeConfigAgent(t *testing.T, maxConcurrency int, queueCapacities map[string]int) *fca {
	presubmits := []config.Presubmit{
		{
			JobBase: config.JobBase{
				Name: "test-bazel-build",
			},
		},
		{
			JobBase: config.JobBase{
				Name: "test-e2e",
			},
		},
		{
			AlwaysRun: true,
			JobBase: config.JobBase{
				Name: "test-bazel-test",
			},
		},
	}
	if err := config.SetPresubmitRegexes(presubmits); err != nil {
		t.Fatal(err)
	}
	presubmitMap := map[string][]config.Presubmit{
		"kubernetes/kubernetes": presubmits,
	}

	return &fca{
		c: &config.Config{
			ProwConfig: config.ProwConfig{
				ProwJobNamespace: "prowjobs",
				PodNamespace:     "pods",
				Plank: config.Plank{
					Controller: config.Controller{
						JobURLTemplate: template.Must(template.New("test").Parse("{{.ObjectMeta.Name}}/{{.Status.State}}")),
						MaxConcurrency: maxConcurrency,
						MaxGoroutines:  20,
					},
					JobQueueCapacities:    queueCapacities,
					PodPendingTimeout:     &metav1.Duration{Duration: podPendingTimeout},
					PodRunningTimeout:     &metav1.Duration{Duration: podRunningTimeout},
					PodUnscheduledTimeout: &metav1.Duration{Duration: podUnscheduledTimeout},
					MaxRevivals:           &maxRevivals,
				},
			},
			JobConfig: config.JobConfig{
				PresubmitsStatic: presubmitMap,
			},
		},
	}
}

func (f *fca) Config() *config.Config {
	f.Lock()
	defer f.Unlock()
	return f.c
}

func TestTerminateDupes(t *testing.T) {
	now := time.Now()
	nowFn := func() *metav1.Time {
		reallyNow := metav1.NewTime(now)
		return &reallyNow
	}

	testcases := []struct {
		Name          string
		PJs           []prowapi.ProwJob
		TerminatedPJs sets.Set[string]
	}{
		{
			Name: "terminate all duplicates",

			PJs: []prowapi.ProwJob{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "newest", Namespace: "prowjobs"},
					Spec: prowapi.ProwJobSpec{
						Agent: prowapi.KubernetesAgent,
						Type:  prowapi.PresubmitJob,
						Job:   "j1",
						Refs:  &prowapi.Refs{Pulls: []prowapi.Pull{{}}},
					},
					Status: prowapi.ProwJobStatus{
						State:     prowapi.PendingState,
						StartTime: metav1.NewTime(now.Add(-time.Minute)),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "prowjobs"},
					Spec: prowapi.ProwJobSpec{
						Agent: prowapi.KubernetesAgent,
						Type:  prowapi.PresubmitJob,
						Job:   "j1",
						Refs:  &prowapi.Refs{Pulls: []prowapi.Pull{{}}},
					},
					Status: prowapi.ProwJobStatus{
						State:     prowapi.TriggeredState,
						StartTime: metav1.NewTime(now.Add(-time.Hour)),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "older", Namespace: "prowjobs"},
					Spec: prowapi.ProwJobSpec{
						Agent: prowapi.KubernetesAgent,
						Type:  prowapi.PresubmitJob,
						Job:   "j1",
						Refs:  &prowapi.Refs{Pulls: []prowapi.Pull{{}}},
					},
					Status: prowapi.ProwJobStatus{
						State:     prowapi.TriggeredState,
						StartTime: metav1.NewTime(now.Add(-2 * time.Hour)),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "complete", Namespace: "prowjobs"},
					Spec: prowapi.ProwJobSpec{
						Agent: prowapi.KubernetesAgent,
						Type:  prowapi.PresubmitJob,
						Job:   "j1",
						Refs:  &prowapi.Refs{Pulls: []prowapi.Pull{{}}},
					},
					Status: prowapi.ProwJobStatus{
						State:          prowapi.SuccessState,
						StartTime:      metav1.NewTime(now.Add(-3 * time.Hour)),
						CompletionTime: nowFn(),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "newest_j2", Namespace: "prowjobs"},
					Spec: prowapi.ProwJobSpec{
						Agent: prowapi.KubernetesAgent,
						Type:  prowapi.PresubmitJob,
						Job:   "j2",
						Refs:  &prowapi.Refs{Pulls: []prowapi.Pull{{}}},
					},
					Status: prowapi.ProwJobStatus{
						State:     prowapi.TriggeredState,
						StartTime: metav1.NewTime(now.Add(-time.Minute)),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "old_j2", Namespace: "prowjobs"},
					Spec: prowapi.ProwJobSpec{
						Agent: prowapi.KubernetesAgent,
						Type:  prowapi.PresubmitJob,
						Job:   "j2",
						Refs:  &prowapi.Refs{Pulls: []prowapi.Pull{{}}},
					},
					Status: prowapi.ProwJobStatus{
						State:     prowapi.TriggeredState,
						StartTime: metav1.NewTime(now.Add(-time.Hour)),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "old_j3", Namespace: "prowjobs"},
					Spec: prowapi.ProwJobSpec{
						Agent: prowapi.KubernetesAgent,
						Type:  prowapi.PresubmitJob,
						Job:   "j3",
						Refs:  &prowapi.Refs{Pulls: []prowapi.Pull{{}}},
					},
					Status: prowapi.ProwJobStatus{
						State:     prowapi.TriggeredState,
						StartTime: metav1.NewTime(now.Add(-time.Hour)),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "new_j3", Namespace: "prowjobs"},
					Spec: prowapi.ProwJobSpec{
						Agent: prowapi.KubernetesAgent,
						Type:  prowapi.PresubmitJob,
						Job:   "j3",
						Refs:  &prowapi.Refs{Pulls: []prowapi.Pull{{}}},
					},
					Status: prowapi.ProwJobStatus{
						State:     prowapi.TriggeredState,
						StartTime: metav1.NewTime(now.Add(-time.Minute)),
					},
				},
			},

			TerminatedPJs: sets.New[string]("old", "older", "old_j2", "old_j3"),
		},
	}

	for _, tc := range testcases {
		t.Run(tc.Name, func(t *testing.T) {
			var prowJobs []runtime.Object
			for i := range tc.PJs {
				pj := &tc.PJs[i]
				prowJobs = append(prowJobs, pj)
			}

			ctx := context.Background()
			fca := &fca{
				c: &config.Config{
					ProwConfig: config.ProwConfig{
						ProwJobNamespace: "prowjobs",
						PodNamespace:     "pods",
					},
				},
			}

			fakeMgr, err := testutil.NewFakeManager(
				ctx,
				prowJobs,
				func(ctx context.Context, indexer ctrlruntimeclient.FieldIndexer) error {
					return setupIndexes(ctx, indexer, fca.Config)
				},
			)
			if err != nil {
				t.Fatalf("Failed to setup fake manager: %v", err)
			}

			fakeProwJobClient := &patchTrackingFakeClient{
				Client: fakeMgr.GetClient(),
			}
			log := logrus.NewEntry(logrus.StandardLogger())

			r := &reconciler{
				pjClient: fakeProwJobClient,
				log:      log,
				config:   fca.Config,
				clock:    clock.RealClock{},
			}
			for _, pj := range tc.PJs {
				err := r.terminateDupes(ctx, &pj)
				if err != nil {
					t.Fatalf("Error terminating dupes: %v", err)
				}
			}

			observedCompletedProwJobs := fakeProwJobClient.patched
			if missing := tc.TerminatedPJs.Difference(observedCompletedProwJobs); missing.Len() > 0 {
				t.Errorf("did not delete expected prowJobs: %v", sets.List(missing))
			}
			if extra := observedCompletedProwJobs.Difference(tc.TerminatedPJs); extra.Len() > 0 {
				t.Errorf("found unexpectedly deleted prowJobs: %v", sets.List(extra))
			}
		})
	}
}

func handleTot(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "0987654321")
}

func TestSyncTriggeredJobs(t *testing.T) {
	fakeClock := clocktesting.NewFakeClock(time.Now().Truncate(1 * time.Second))
	pendingTime := metav1.NewTime(fakeClock.Now())

	type testCase struct {
		Name string

		PJ             prowapi.ProwJob
		PendingJobs    map[string]int
		MaxConcurrency int
		Pods           map[string][]v1.Pod
		PodErr         error

		ExpectedState       prowapi.ProwJobState
		ExpectedPodHasName  bool
		ExpectedNumPods     map[string]int
		ExpectedCreatedPJs  int
		ExpectedComplete    bool
		ExpectedURL         string
		ExpectedBuildID     string
		ExpectError         bool
		ExpectedPendingTime *metav1.Time
	}

	testcases := []testCase{
		{
			Name: "start new pod",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "blabla",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:     "boop",
					Type:    prowapi.PeriodicJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			Pods:                map[string][]v1.Pod{"default": {}},
			ExpectedState:       prowapi.PendingState,
			ExpectedPendingTime: &pendingTime,
			ExpectedPodHasName:  true,
			ExpectedNumPods:     map[string]int{"default": 1},
			ExpectedURL:         "blabla/pending",
			ExpectedBuildID:     "0987654321",
		},
		{
			Name: "pod with a max concurrency of 1",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "blabla",
					Namespace:         "prowjobs",
					CreationTimestamp: metav1.Now(),
				},
				Spec: prowapi.ProwJobSpec{
					Job:            "same",
					Type:           prowapi.PeriodicJob,
					MaxConcurrency: 1,
					PodSpec:        &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			PendingJobs: map[string]int{
				"same": 1,
			},
			Pods: map[string][]v1.Pod{
				"default": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "same-42",
							Namespace: "pods",
						},
						Status: v1.PodStatus{
							Phase: v1.PodRunning,
						},
					},
				},
			},
			ExpectedState:   prowapi.TriggeredState,
			ExpectedNumPods: map[string]int{"default": 1},
		},
		{
			Name: "trusted pod with a max concurrency of 1",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "blabla",
					Namespace:         "prowjobs",
					CreationTimestamp: metav1.Now(),
				},
				Spec: prowapi.ProwJobSpec{
					Job:            "same",
					Type:           prowapi.PeriodicJob,
					Cluster:        "trusted",
					MaxConcurrency: 1,
					PodSpec:        &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			PendingJobs: map[string]int{
				"same": 1,
			},
			Pods: map[string][]v1.Pod{
				"trusted": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "same-42",
							Namespace: "pods",
						},
						Status: v1.PodStatus{
							Phase: v1.PodRunning,
						},
					},
				},
			},
			ExpectedState:   prowapi.TriggeredState,
			ExpectedNumPods: map[string]int{"trusted": 1},
		},
		{
			Name: "trusted pod with a max concurrency of 1 (can start)",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "some",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:            "some",
					Type:           prowapi.PeriodicJob,
					Cluster:        "trusted",
					MaxConcurrency: 1,
					PodSpec:        &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			Pods: map[string][]v1.Pod{
				"default": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "other-42",
							Namespace: "pods",
						},
						Status: v1.PodStatus{
							Phase: v1.PodRunning,
						},
					},
				},
				"trusted": {},
			},
			ExpectedState:       prowapi.PendingState,
			ExpectedNumPods:     map[string]int{"default": 1, "trusted": 1},
			ExpectedPodHasName:  true,
			ExpectedPendingTime: &pendingTime,
			ExpectedURL:         "some/pending",
		},
		{
			Name: "do not exceed global maxconcurrency",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "beer",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:     "same",
					Type:    prowapi.PeriodicJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			MaxConcurrency: 20,
			PendingJobs:    map[string]int{"motherearth": 10, "allagash": 8, "krusovice": 2},
			ExpectedState:  prowapi.TriggeredState,
		},
		{
			Name: "global maxconcurrency allows new jobs when possible",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "beer",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:     "same",
					Type:    prowapi.PeriodicJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			Pods:                map[string][]v1.Pod{"default": {}},
			MaxConcurrency:      21,
			PendingJobs:         map[string]int{"motherearth": 10, "allagash": 8, "krusovice": 2},
			ExpectedState:       prowapi.PendingState,
			ExpectedNumPods:     map[string]int{"default": 1},
			ExpectedURL:         "beer/pending",
			ExpectedPendingTime: &pendingTime,
		},
		{
			Name: "unprocessable prow job",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "beer",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:     "boop",
					Type:    prowapi.PeriodicJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			Pods: map[string][]v1.Pod{"default": {}},
			PodErr: &kapierrors.StatusError{ErrStatus: metav1.Status{
				Status: metav1.StatusFailure,
				Code:   http.StatusUnprocessableEntity,
				Reason: metav1.StatusReasonInvalid,
			}},
			ExpectedState:    prowapi.ErrorState,
			ExpectedComplete: true,
		},
		{
			Name: "forbidden prow job",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "beer",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:     "boop",
					Type:    prowapi.PeriodicJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			Pods: map[string][]v1.Pod{"default": {}},
			PodErr: &kapierrors.StatusError{ErrStatus: metav1.Status{
				Status: metav1.StatusFailure,
				Code:   http.StatusForbidden,
				Reason: metav1.StatusReasonForbidden,
			}},
			ExpectedState:    prowapi.ErrorState,
			ExpectedComplete: true,
		},
		{
			Name: "conflict error starting pod",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "beer",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:     "boop",
					Type:    prowapi.PeriodicJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			Pods: map[string][]v1.Pod{"default": {}},
			PodErr: &kapierrors.StatusError{ErrStatus: metav1.Status{
				Status: metav1.StatusFailure,
				Code:   http.StatusConflict,
				Reason: metav1.StatusReasonAlreadyExists,
			}},
			ExpectedState:    prowapi.ErrorState,
			ExpectedComplete: true,
		},
		{
			Name: "unknown error starting pod",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "beer",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:     "boop",
					Type:    prowapi.PeriodicJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			PodErr:        errors.New("no way unknown jose"),
			ExpectedState: prowapi.TriggeredState,
			ExpectError:   true,
		},
		{
			Name: "running pod, failed prowjob update",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:     "boop",
					Type:    prowapi.PeriodicJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			Pods: map[string][]v1.Pod{
				"default": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "foo",
							Namespace: "pods",
							Labels: map[string]string{
								kube.ProwBuildIDLabel: "0987654321",
							},
						},
						Status: v1.PodStatus{
							Phase: v1.PodRunning,
						},
					},
				},
			},
			ExpectedState:       prowapi.PendingState,
			ExpectedNumPods:     map[string]int{"default": 1},
			ExpectedPendingTime: &pendingTime,
			ExpectedURL:         "foo/pending",
			ExpectedBuildID:     "0987654321",
			ExpectedPodHasName:  true,
		},
		{
			Name: "running pod, failed prowjob update, backwards compatible on pods with build label not set",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:     "boop",
					Type:    prowapi.PeriodicJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			Pods: map[string][]v1.Pod{
				"default": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "foo",
							Namespace: "pods",
							Labels: map[string]string{
								kube.ProwBuildIDLabel: "",
							},
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name: "test-name",
									Env: []v1.EnvVar{
										{
											Name:  "BUILD_ID",
											Value: "0987654321",
										},
									},
								},
							},
						},
						Status: v1.PodStatus{
							Phase: v1.PodRunning,
						},
					},
				},
			},
			ExpectedState:       prowapi.PendingState,
			ExpectedNumPods:     map[string]int{"default": 1},
			ExpectedPendingTime: &pendingTime,
			ExpectedURL:         "foo/pending",
			ExpectedBuildID:     "0987654321",
			ExpectedPodHasName:  true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.Name, func(t *testing.T) {
			totServ := httptest.NewServer(http.HandlerFunc(handleTot))
			defer totServ.Close()
			pm := make(map[string]v1.Pod)
			for _, pods := range tc.Pods {
				for i := range pods {
					pm[pods[i].ObjectMeta.Name] = pods[i]
				}
			}
			tc.PJ.Spec.Agent = prowapi.KubernetesAgent

			ctx := context.Background()
			config := newFakeConfigAgent(t, tc.MaxConcurrency, nil).Config
			fakeMgr, err := testutil.NewFakeManager(
				ctx,
				[]runtime.Object{&tc.PJ},
				func(ctx context.Context, indexer ctrlruntimeclient.FieldIndexer) error {
					return setupIndexes(ctx, indexer, config)
				},
			)
			if err != nil {
				t.Fatalf("Failed to setup fake manager: %v", err)
			}

			fakeProwJobClient := fakeMgr.GetClient()

			buildClients := map[string]buildClient{}
			for alias, pods := range tc.Pods {
				builder := fakectrlruntimeclient.NewClientBuilder()
				for i := range pods {
					builder.WithRuntimeObjects(&pods[i])
				}
				fakeClient := &clientWrapper{
					Client:      builder.Build(),
					createError: tc.PodErr,
				}
				buildClients[alias] = buildClient{
					Client: fakeClient,
				}
			}
			if _, exists := buildClients[prowapi.DefaultClusterAlias]; !exists {
				buildClients[prowapi.DefaultClusterAlias] = buildClient{
					Client: &clientWrapper{
						Client:      fakectrlruntimeclient.NewClientBuilder().Build(),
						createError: tc.PodErr,
					},
				}
			}

			for jobName, numJobsToCreate := range tc.PendingJobs {
				for i := 0; i < numJobsToCreate; i++ {
					if err := fakeProwJobClient.Create(ctx, &prowapi.ProwJob{
						ObjectMeta: metav1.ObjectMeta{
							Name:      fmt.Sprintf("%s-%d", jobName, i),
							Namespace: "prowjobs",
						},
						Spec: prowapi.ProwJobSpec{
							Agent: prowapi.KubernetesAgent,
							Job:   jobName,
						},
						Status: prowapi.ProwJobStatus{
							State: prowapi.PendingState,
						},
					}); err != nil {
						t.Fatalf("failed to create prowJob: %v", err)
					}
				}
			}
			r := &reconciler{
				pjClient:     fakeProwJobClient,
				buildClients: buildClients,
				log:          logrus.NewEntry(logrus.StandardLogger()),
				config:       config,
				totURL:       totServ.URL,
				clock:        fakeClock,
			}
			pj := tc.PJ.DeepCopy()
			pj.UID = types.UID("under-test")
			if _, err := r.syncTriggeredJob(ctx, pj); (err != nil) != tc.ExpectError {
				if tc.ExpectError {
					t.Errorf("for case %q expected an error, but got none", tc.Name)
				} else {
					t.Errorf("for case %q got an unexpected error: %v", tc.Name, err)
				}
				return
			}
			// In PlankV2 we throw them all into the same client and then count the resulting number
			for _, pendingJobs := range tc.PendingJobs {
				tc.ExpectedCreatedPJs += pendingJobs
			}

			actualProwJobs := &prowapi.ProwJobList{}
			if err := fakeProwJobClient.List(ctx, actualProwJobs); err != nil {
				t.Errorf("could not list prowJobs from the client: %v", err)
			}
			if len(actualProwJobs.Items) != tc.ExpectedCreatedPJs+1 {
				t.Errorf("got %d created prowjobs, expected %d", len(actualProwJobs.Items)-1, tc.ExpectedCreatedPJs)
			}
			var actual prowapi.ProwJob
			if err := fakeProwJobClient.Get(ctx, ctrlruntimeclient.ObjectKeyFromObject(&tc.PJ), &actual); err != nil {
				t.Errorf("failed to get prowjob from client: %v", err)
			}
			if actual.Status.State != tc.ExpectedState {
				t.Errorf("expected state %v, got state %v", tc.ExpectedState, actual.Status.State)
			}
			if !reflect.DeepEqual(actual.Status.PendingTime, tc.ExpectedPendingTime) {
				t.Errorf("got pending time %v, expected %v", actual.Status.PendingTime, tc.ExpectedPendingTime)
			}
			if (actual.Status.PodName == "") && tc.ExpectedPodHasName {
				t.Errorf("got no pod name, expected one")
			}
			if tc.ExpectedBuildID != "" && actual.Status.BuildID != tc.ExpectedBuildID {
				t.Errorf("expected BuildID: %q, got: %q", tc.ExpectedBuildID, actual.Status.BuildID)
			}
			for alias, expected := range tc.ExpectedNumPods {
				actualPods := &v1.PodList{}
				if err := buildClients[alias].List(ctx, actualPods); err != nil {
					t.Errorf("could not list pods from the client: %v", err)
				}
				if got := len(actualPods.Items); got != expected {
					t.Errorf("got %d pods for alias %q, but expected %d", got, alias, expected)
				}
			}
			if actual.Complete() != tc.ExpectedComplete {
				t.Error("got wrong completion")
			}
		})
	}
}

func startTime(s time.Time) *metav1.Time {
	start := metav1.NewTime(s)
	return &start
}

func TestSyncPendingJob(t *testing.T) {
	type testCase struct {
		Name string

		PJ   prowapi.ProwJob
		Pods []v1.Pod
		Err  error

		expectedReconcileResult       *reconcile.Result
		ExpectedState                 prowapi.ProwJobState
		ExpectedNumPods               int
		ExpectedComplete              bool
		ExpectedCreatedPJs            int
		ExpectedReport                bool
		ExpectedURL                   string
		ExpectedBuildID               string
		ExpectedPodRunningTimeout     *metav1.Duration
		ExpectedPodPendingTimeout     *metav1.Duration
		ExpectedPodUnscheduledTimeout *metav1.Duration
	}
	testcases := []testCase{
		{
			Name: "reset when pod goes missing",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-41",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Type:    prowapi.PostsubmitJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
					Refs:    &prowapi.Refs{Org: "fejtaverse"},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-41",
				},
			},
			ExpectedState:   prowapi.PendingState,
			ExpectedReport:  true,
			ExpectedNumPods: 1,
			ExpectedURL:     "boop-41/pending",
			ExpectedBuildID: "0987654321",
		},
		{
			Name: "delete pod in unknown state",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-41",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-41",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "boop-41",
						Namespace: "pods",
					},
					Status: v1.PodStatus{
						Phase: v1.PodUnknown,
					},
				},
			},
			ExpectedState:   prowapi.PendingState,
			ExpectedNumPods: 0,
		},
		{
			Name: "delete pod in unknown state with gcsreporter finalizer",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-41",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-41",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "boop-41",
						Namespace:  "pods",
						Finalizers: []string{"prow.x-k8s.io/gcsk8sreporter"},
					},
					Status: v1.PodStatus{
						Phase: v1.PodUnknown,
					},
				},
			},
			ExpectedState:   prowapi.PendingState,
			ExpectedNumPods: 0,
		},
		{
			Name: "succeeded pod",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-42",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Type:    prowapi.BatchJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
					Refs:    &prowapi.Refs{Org: "fejtaverse"},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-42",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "boop-42",
						Namespace: "pods",
					},
					Status: v1.PodStatus{
						Phase: v1.PodSucceeded,
					},
				},
			},
			ExpectedComplete:   true,
			ExpectedState:      prowapi.SuccessState,
			ExpectedNumPods:    1,
			ExpectedCreatedPJs: 0,
			ExpectedURL:        "boop-42/success",
		},
		{
			Name: "succeeded pod with unfinished containers",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-42",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Type:    prowapi.BatchJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
					Refs:    &prowapi.Refs{Org: "fejtaverse"},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-42",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "boop-42",
						Namespace: "pods",
					},
					Status: v1.PodStatus{
						Phase:             v1.PodSucceeded,
						ContainerStatuses: []v1.ContainerStatus{{LastTerminationState: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{}}}},
					},
				},
			},
			ExpectedComplete:   true,
			ExpectedState:      prowapi.ErrorState,
			ExpectedNumPods:    1,
			ExpectedCreatedPJs: 0,
			ExpectedURL:        "boop-42/success",
		},
		{
			Name: "succeeded pod with unfinished initcontainers",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-42",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Type:    prowapi.BatchJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
					Refs:    &prowapi.Refs{Org: "fejtaverse"},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-42",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "boop-42",
						Namespace: "pods",
					},
					Status: v1.PodStatus{
						Phase:                 v1.PodSucceeded,
						InitContainerStatuses: []v1.ContainerStatus{{LastTerminationState: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{}}}},
					},
				},
			},
			ExpectedComplete:   true,
			ExpectedState:      prowapi.ErrorState,
			ExpectedNumPods:    1,
			ExpectedCreatedPJs: 0,
			ExpectedURL:        "boop-42/success",
		},
		{
			Name: "failed pod",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-42",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Type: prowapi.PresubmitJob,
					Refs: &prowapi.Refs{
						Org: "kubernetes", Repo: "kubernetes",
						BaseRef: "baseref", BaseSHA: "basesha",
						Pulls: []prowapi.Pull{{Number: 100, Author: "me", SHA: "sha"}},
					},
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-42",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "boop-42",
						Namespace: "pods",
					},
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
					},
				},
			},
			ExpectedComplete: true,
			ExpectedState:    prowapi.FailureState,
			ExpectedNumPods:  1,
			ExpectedURL:      "boop-42/failure",
		},
		{
			Name: "delete evicted pod",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-42",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-42",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "boop-42",
						Namespace: "pods",
					},
					Status: v1.PodStatus{
						Phase:  v1.PodFailed,
						Reason: Evicted,
					},
				},
			},
			ExpectedComplete: false,
			ExpectedState:    prowapi.PendingState,
			ExpectedNumPods:  0,
		},
		{
			Name: "delete evicted pod and remove its k8sreporter finalizer",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-42",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-42",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "boop-42",
						Namespace:  "pods",
						Finalizers: []string{"prow.x-k8s.io/gcsk8sreporter"},
					},
					Status: v1.PodStatus{
						Phase:  v1.PodFailed,
						Reason: Evicted,
					},
				},
			},
			ExpectedComplete: false,
			ExpectedState:    prowapi.PendingState,
			ExpectedNumPods:  0,
		},
		{
			Name: "don't delete evicted pod w/ error_on_eviction, complete PJ instead",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-42",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					ErrorOnEviction: true,
					PodSpec:         &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-42",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "boop-42",
						Namespace: "pods",
					},
					Status: v1.PodStatus{
						Phase:  v1.PodFailed,
						Reason: Evicted,
					},
				},
			},
			ExpectedComplete: true,
			ExpectedState:    prowapi.ErrorState,
			ExpectedNumPods:  1,
			ExpectedURL:      "boop-42/error",
		},
		{
			Name: "don't delete evicted pod w/ revivalCount == maxRevivals, complete PJ instead",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-42",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					PodRevivalCount: maxRevivals,
					State:           prowapi.PendingState,
					PodName:         "boop-42",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "boop-42",
						Namespace: "pods",
					},
					Status: v1.PodStatus{
						Phase:  v1.PodFailed,
						Reason: Evicted,
					},
				},
			},
			ExpectedComplete: true,
			ExpectedState:    prowapi.ErrorState,
			ExpectedNumPods:  1,
			ExpectedURL:      "boop-42/error",
		},
		{
			// TODO: this test case tests the current behavior, but the behavior
			// is non-ideal: the pod execution did not fail, instead the node on which
			// the pod was running terminated
			Name: "a terminated pod is handled as-if it failed",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-42",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-42",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "boop-42",
						Namespace: "pods",
					},
					Status: v1.PodStatus{
						Phase:  v1.PodFailed,
						Reason: Terminated,
					},
				},
			},
			ExpectedComplete: true,
			ExpectedState:    prowapi.FailureState,
			ExpectedNumPods:  1,
			ExpectedURL:      "boop-42/error",
		},
		{
			Name: "running pod",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-42",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-42",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "boop-42",
						Namespace: "pods",
					},
					Status: v1.PodStatus{
						Phase: v1.PodRunning,
					},
				},
			},
			ExpectedState:   prowapi.PendingState,
			ExpectedNumPods: 1,
		},
		{
			Name: "pod changes url status",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-42",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-42",
					URL:     "boop-42/pending",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "boop-42",
						Namespace: "pods",
					},
					Status: v1.PodStatus{
						Phase: v1.PodSucceeded,
					},
				},
			},
			ExpectedComplete:   true,
			ExpectedState:      prowapi.SuccessState,
			ExpectedNumPods:    1,
			ExpectedCreatedPJs: 0,
			ExpectedURL:        "boop-42/success",
		},
		{
			Name: "unprocessable prow job",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "jose",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:     "boop",
					Type:    prowapi.PostsubmitJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
					Refs:    &prowapi.Refs{Org: "fejtaverse"},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.PendingState,
				},
			},
			Err: &kapierrors.StatusError{ErrStatus: metav1.Status{
				Status: metav1.StatusFailure,
				Code:   http.StatusUnprocessableEntity,
				Reason: metav1.StatusReasonInvalid,
			}},
			ExpectedState:    prowapi.ErrorState,
			ExpectedComplete: true,
			ExpectedURL:      "jose/error",
		},
		{
			Name: "stale pending prow job",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "nightmare",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "nightmare",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "nightmare",
						Namespace:         "pods",
						CreationTimestamp: metav1.Time{Time: time.Now().Add(-podPendingTimeout)},
					},
					Status: v1.PodStatus{
						Phase:     v1.PodPending,
						StartTime: startTime(time.Now().Add(-podPendingTimeout)),
					},
				},
			},
			ExpectedState:    prowapi.ErrorState,
			ExpectedNumPods:  0,
			ExpectedComplete: true,
			ExpectedURL:      "nightmare/error",
		},
		{
			Name: "stale pending prow job with specific podPendingTimeout",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "nightmare",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					DecorationConfig: &prowapi.DecorationConfig{
						PodPendingTimeout: &metav1.Duration{Duration: 2 * time.Hour},
					},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "nightmare",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "nightmare",
						Namespace:         "pods",
						CreationTimestamp: metav1.Time{Time: time.Now().Add(-time.Hour * 2)},
					},
					Status: v1.PodStatus{
						Phase:     v1.PodPending,
						StartTime: startTime(time.Now().Add(-time.Hour * 2)),
					},
				},
			},
			ExpectedState:             prowapi.ErrorState,
			ExpectedNumPods:           0,
			ExpectedComplete:          true,
			ExpectedURL:               "nightmare/error",
			ExpectedPodPendingTimeout: &metav1.Duration{Duration: 2 * time.Hour},
		},
		{
			Name: "stale running prow job",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "endless",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "endless",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "endless",
						Namespace:         "pods",
						CreationTimestamp: metav1.Time{Time: time.Now().Add(-podRunningTimeout)},
					},
					Status: v1.PodStatus{
						Phase:     v1.PodRunning,
						StartTime: startTime(time.Now().Add(-podRunningTimeout)),
					},
				},
			},
			ExpectedState:    prowapi.AbortedState,
			ExpectedNumPods:  0,
			ExpectedComplete: true,
			ExpectedURL:      "endless/aborted",
		},
		{
			Name: "stale running prow job with specific podRunningTimeout",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "endless",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					DecorationConfig: &prowapi.DecorationConfig{
						PodRunningTimeout: &metav1.Duration{Duration: 1 * time.Hour},
					},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "endless",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "endless",
						Namespace:         "pods",
						CreationTimestamp: metav1.Time{Time: time.Now().Add(-time.Hour)},
					},
					Status: v1.PodStatus{
						Phase:     v1.PodRunning,
						StartTime: startTime(time.Now().Add(-time.Hour)),
					},
				},
			},
			ExpectedState:             prowapi.AbortedState,
			ExpectedNumPods:           0,
			ExpectedComplete:          true,
			ExpectedURL:               "endless/aborted",
			ExpectedPodRunningTimeout: &metav1.Duration{Duration: 1 * time.Hour},
		},
		{
			Name: "stale unschedulable prow job",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "homeless",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "homeless",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "homeless",
						Namespace:         "pods",
						CreationTimestamp: metav1.Time{Time: time.Now().Add(-podUnscheduledTimeout - time.Second)},
					},
					Status: v1.PodStatus{
						Phase: v1.PodPending,
					},
				},
			},
			ExpectedState:    prowapi.ErrorState,
			ExpectedNumPods:  0,
			ExpectedComplete: true,
			ExpectedURL:      "homeless/error",
		},
		{
			Name: "stale unschedulable prow job with specific podUnscheduledTimeout",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "homeless",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					DecorationConfig: &prowapi.DecorationConfig{
						PodUnscheduledTimeout: &metav1.Duration{Duration: 2 * time.Minute},
					},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "homeless",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "homeless",
						Namespace:         "pods",
						CreationTimestamp: metav1.Time{Time: time.Now().Add(-2*time.Minute - time.Second)},
					},
					Status: v1.PodStatus{
						Phase: v1.PodPending,
					},
				},
			},
			ExpectedState:                 prowapi.ErrorState,
			ExpectedNumPods:               0,
			ExpectedComplete:              true,
			ExpectedURL:                   "homeless/error",
			ExpectedPodUnscheduledTimeout: &metav1.Duration{Duration: 2 * time.Minute},
		},
		{
			Name: "pending, created less than podPendingTimeout ago",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "slowpoke",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "slowpoke",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "slowpoke",
						Namespace:         "pods",
						CreationTimestamp: metav1.Time{Time: time.Now().Add(-(podPendingTimeout - 10*time.Minute))},
					},
					Status: v1.PodStatus{
						Phase:     v1.PodPending,
						StartTime: startTime(time.Now().Add(-(podPendingTimeout - 10*time.Minute))),
					},
				},
			},
			expectedReconcileResult: &reconcile.Result{RequeueAfter: 10 * time.Minute},
			ExpectedState:           prowapi.PendingState,
			ExpectedNumPods:         1,
		},
		{
			Name: "unscheduled, created less than podUnscheduledTimeout ago",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "just-waiting",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "just-waiting",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "just-waiting",
						Namespace:         "pods",
						CreationTimestamp: metav1.Time{Time: time.Now().Add(-time.Second)},
					},
					Status: v1.PodStatus{
						Phase: v1.PodPending,
					},
				},
			},
			expectedReconcileResult: &reconcile.Result{RequeueAfter: podUnscheduledTimeout},
			ExpectedState:           prowapi.PendingState,
			ExpectedNumPods:         1,
		},
		{
			Name: "Pod deleted in pending phase, job marked as errored",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deleted-pod-in-pending-marks-job-as-errored",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "deleted-pod-in-pending-marks-job-as-errored",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "deleted-pod-in-pending-marks-job-as-errored",
						Namespace:         "pods",
						CreationTimestamp: metav1.Time{Time: time.Now().Add(-time.Second)},
						DeletionTimestamp: &metav1.Time{Time: time.Now()},
						Finalizers:        []string{podDeletionPreventionFinalizer},
					},
					Status: v1.PodStatus{
						Phase: v1.PodPending,
					},
				},
			},
			ExpectedState:    prowapi.ErrorState,
			ExpectedComplete: true,
			ExpectedNumPods:  1,
		},
		{
			Name: "Pod deleted in unset phase, job marked as errored",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod-deleted-in-unset-phase",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "pod-deleted-in-unset-phase",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "pod-deleted-in-unset-phase",
						Namespace:         "pods",
						CreationTimestamp: metav1.Time{Time: time.Now().Add(-time.Second)},
						DeletionTimestamp: &metav1.Time{Time: time.Now()},
						Finalizers:        []string{podDeletionPreventionFinalizer},
					},
				},
			},
			ExpectedState:    prowapi.ErrorState,
			ExpectedComplete: true,
			ExpectedNumPods:  1,
		},
		{
			Name: "Pod deleted in running phase, job marked as errored",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod-deleted-in-unset-phase",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "pod-deleted-in-unset-phase",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "pod-deleted-in-unset-phase",
						Namespace:         "pods",
						CreationTimestamp: metav1.Time{Time: time.Now().Add(-time.Second)},
						DeletionTimestamp: &metav1.Time{Time: time.Now()},
						Finalizers:        []string{podDeletionPreventionFinalizer},
					},
					Status: v1.PodStatus{
						Phase: v1.PodRunning,
					},
				},
			},
			ExpectedState:    prowapi.ErrorState,
			ExpectedComplete: true,
			ExpectedNumPods:  1,
		},
		{
			Name: "Pod deleted with NodeLost reason in running phase, pod finalizer gets cleaned up",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod-deleted-in-running-phase",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "pod-deleted-in-running-phase",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "pod-deleted-in-running-phase",
						Namespace:         "pods",
						CreationTimestamp: metav1.Time{Time: time.Now().Add(-time.Second)},
						DeletionTimestamp: &metav1.Time{Time: time.Now()},
						Finalizers:        []string{"prow.x-k8s.io/gcsk8sreporter"},
					},
					Status: v1.PodStatus{
						Phase:  v1.PodRunning,
						Reason: "NodeLost",
					},
				},
			},
			ExpectedState:    prowapi.PendingState,
			ExpectedComplete: false,
			ExpectedNumPods:  0,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.Name, func(t *testing.T) {
			totServ := httptest.NewServer(http.HandlerFunc(handleTot))
			defer totServ.Close()
			pm := make(map[string]v1.Pod)
			for i := range tc.Pods {
				pm[tc.Pods[i].ObjectMeta.Name] = tc.Pods[i]
			}
			ctx := context.Background()
			config := newFakeConfigAgent(t, 0, nil).Config

			fakeMgr, err := testutil.NewFakeManager(
				ctx,
				[]runtime.Object{&tc.PJ},
				func(ctx context.Context, indexer ctrlruntimeclient.FieldIndexer) error {
					return setupIndexes(ctx, indexer, config)
				},
			)
			if err != nil {
				t.Fatalf("Failed to setup fake manager: %v", err)
			}
			fakeProwJobClient := fakeMgr.GetClient()

			var data []runtime.Object
			for i := range tc.Pods {
				pod := tc.Pods[i]
				data = append(data, &pod)
			}
			fakeClient := &clientWrapper{
				Client:                   fakectrlruntimeclient.NewFakeClient(data...),
				createError:              tc.Err,
				errOnDeleteWithFinalizer: true,
			}
			buildClients := map[string]buildClient{
				prowapi.DefaultClusterAlias: {
					Client: fakeClient,
				},
			}

			r := &reconciler{
				pjClient:     fakeProwJobClient,
				buildClients: buildClients,
				log:          logrus.NewEntry(logrus.StandardLogger()),
				config:       config,
				totURL:       totServ.URL,
				clock:        clock.RealClock{},
			}
			reconcileResult, err := r.syncPendingJob(ctx, &tc.PJ)
			if err != nil {
				t.Fatalf("syncPendingJob failed: %v", err)
			}
			if reconcileResult != nil {
				// Round this to minutes so we can compare the value without risking flaky tests
				reconcileResult.RequeueAfter = reconcileResult.RequeueAfter.Round(time.Minute)
			}
			if diff := cmp.Diff(tc.expectedReconcileResult, reconcileResult); diff != "" {
				t.Errorf("expected reconcileResult differs from actual: %s", diff)
			}

			actualProwJobs := &prowapi.ProwJobList{}
			if err := fakeProwJobClient.List(ctx, actualProwJobs); err != nil {
				t.Errorf("could not list prowJobs from the client: %v", err)
			}
			if len(actualProwJobs.Items) != tc.ExpectedCreatedPJs+1 {
				t.Errorf("got %d created prowjobs", len(actualProwJobs.Items)-1)
			}
			actual := actualProwJobs.Items[0]
			if actual.Status.State != tc.ExpectedState {
				t.Errorf("got state %v", actual.Status.State)
			}
			if tc.ExpectedBuildID != "" && actual.Status.BuildID != tc.ExpectedBuildID {
				t.Errorf("expected BuildID %q, got %q", tc.ExpectedBuildID, actual.Status.BuildID)
			}
			if actual.Spec.DecorationConfig != nil && actual.Spec.DecorationConfig.PodRunningTimeout != nil &&
				tc.ExpectedPodRunningTimeout.Duration != actual.Spec.DecorationConfig.PodRunningTimeout.Duration {
				t.Errorf("expected PodRunningTimeout %v, got %v",
					tc.ExpectedPodRunningTimeout.Duration, actual.Spec.DecorationConfig.PodRunningTimeout.Duration)
			}
			if actual.Spec.DecorationConfig != nil && actual.Spec.DecorationConfig.PodPendingTimeout != nil &&
				tc.ExpectedPodPendingTimeout.Duration != actual.Spec.DecorationConfig.PodPendingTimeout.Duration {
				t.Errorf("expected PodPendingTimeout %v, got %v",
					tc.ExpectedPodPendingTimeout.Duration, actual.Spec.DecorationConfig.PodPendingTimeout.Duration)
			}
			if actual.Spec.DecorationConfig != nil && actual.Spec.DecorationConfig.PodUnscheduledTimeout != nil &&
				tc.ExpectedPodUnscheduledTimeout.Duration != actual.Spec.DecorationConfig.PodUnscheduledTimeout.Duration {
				t.Errorf("expected PodUnscheduledTimeout %v, got %v",
					tc.ExpectedPodUnscheduledTimeout.Duration, actual.Spec.DecorationConfig.PodUnscheduledTimeout.Duration)
			}
			actualPods := &v1.PodList{}
			if err := buildClients[prowapi.DefaultClusterAlias].List(ctx, actualPods); err != nil {
				t.Errorf("could not list pods from the client: %v", err)
			}
			if got := len(actualPods.Items); got != tc.ExpectedNumPods {
				t.Errorf("got %d pods, expected %d", len(actualPods.Items), tc.ExpectedNumPods)
			}
			for _, pod := range actualPods.Items {
				if !podWouldBeGone(pod) {
					t.Errorf("pod %s was deleted but still had finalizers: %v", pod.Name, pod.Finalizers)
				}
			}
			if actual := actual.Complete(); actual != tc.ExpectedComplete {
				t.Errorf("expected complete: %t, got complete: %t", tc.ExpectedComplete, actual)
			}
		})
	}
}

func podWouldBeGone(pod corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return true
	}

	actual := sets.New(pod.Finalizers...)
	allowed := sets.New(podDeletionPreventionFinalizer)

	return allowed.IsSuperset(actual)
}

// TestPeriodic walks through the happy path of a periodic job.
func TestPeriodic(t *testing.T) {
	per := config.Periodic{
		JobBase: config.JobBase{
			Name:    "ci-periodic-job",
			Agent:   "kubernetes",
			Cluster: "trusted",
			Spec:    &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
		},
	}

	totServ := httptest.NewServer(http.HandlerFunc(handleTot))
	defer totServ.Close()
	pj := pjutil.NewProwJob(pjutil.PeriodicSpec(per), nil, nil)
	pj.Namespace = "prowjobs"

	ctx := context.Background()
	config := newFakeConfigAgent(t, 0, nil).Config

	fakeMgr, err := testutil.NewFakeManager(
		ctx,
		[]runtime.Object{&pj},
		func(ctx context.Context, indexer ctrlruntimeclient.FieldIndexer) error {
			return setupIndexes(ctx, indexer, config)
		},
	)
	if err != nil {
		t.Fatalf("Failed to setup fake manager: %v", err)
	}
	fakeProwJobClient := fakeMgr.GetClient()

	buildClients := map[string]buildClient{
		prowapi.DefaultClusterAlias: {
			Client: fakectrlruntimeclient.NewClientBuilder().Build(),
		},
		"trusted": {
			Client: fakectrlruntimeclient.NewClientBuilder().Build(),
		},
	}

	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)
	log := logrus.NewEntry(logger)
	r := reconciler{
		pjClient:     fakeProwJobClient,
		buildClients: buildClients,
		log:          log,
		config:       config,
		totURL:       totServ.URL,
		clock:        clock.RealClock{},
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: ctrlruntimeclient.ObjectKeyFromObject(&pj)}); err != nil {
		t.Fatalf("Error on first sync: %v", err)
	}

	afterFirstSync := &prowapi.ProwJobList{}
	if err := fakeProwJobClient.List(ctx, afterFirstSync); err != nil {
		t.Fatalf("could not list prowJobs from the client: %v", err)
	}
	if len(afterFirstSync.Items) != 1 {
		t.Fatalf("saw %d prowjobs after sync, not 1", len(afterFirstSync.Items))
	}
	if len(afterFirstSync.Items[0].Spec.PodSpec.Containers) != 1 || afterFirstSync.Items[0].Spec.PodSpec.Containers[0].Name != "test-name" {
		t.Fatalf("Sync step updated the pod spec: %#v", afterFirstSync.Items[0].Spec.PodSpec)
	}
	podsAfterSync := &v1.PodList{}
	if err := buildClients["trusted"].List(ctx, podsAfterSync); err != nil {
		t.Fatalf("could not list pods from the client: %v", err)
	}
	if len(podsAfterSync.Items) != 1 {
		t.Fatalf("expected exactly one pod, got %d", len(podsAfterSync.Items))
	}
	if len(podsAfterSync.Items[0].Spec.Containers) != 1 {
		t.Fatal("Wiped container list.")
	}
	if len(podsAfterSync.Items[0].Spec.Containers[0].Env) == 0 {
		t.Fatal("Container has no env set.")
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: ctrlruntimeclient.ObjectKeyFromObject(&pj)}); err != nil {
		t.Fatalf("Error on second sync: %v", err)
	}
	podsAfterSecondSync := &v1.PodList{}
	if err := buildClients["trusted"].List(ctx, podsAfterSecondSync); err != nil {
		t.Fatalf("could not list pods from the client: %v", err)
	}
	if len(podsAfterSecondSync.Items) != 1 {
		t.Fatalf("Wrong number of pods after second sync: %d", len(podsAfterSecondSync.Items))
	}
	update := podsAfterSecondSync.Items[0].DeepCopy()
	update.Status.Phase = v1.PodSucceeded
	if err := buildClients["trusted"].Status().Update(ctx, update); err != nil {
		t.Fatalf("could not update pod to be succeeded: %v", err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: ctrlruntimeclient.ObjectKeyFromObject(&pj)}); err != nil {
		t.Fatalf("Error on third sync: %v", err)
	}
	afterThirdSync := &prowapi.ProwJobList{}
	if err := fakeProwJobClient.List(ctx, afterThirdSync); err != nil {
		t.Fatalf("could not list prowJobs from the client: %v", err)
	}
	if len(afterThirdSync.Items) != 1 {
		t.Fatalf("Wrong number of prow jobs: %d", len(afterThirdSync.Items))
	}
	if !afterThirdSync.Items[0].Complete() {
		t.Fatal("Prow job didn't complete.")
	}
	if afterThirdSync.Items[0].Status.State != prowapi.SuccessState {
		t.Fatalf("Should be success: %v", afterThirdSync.Items[0].Status.State)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: ctrlruntimeclient.ObjectKeyFromObject(&pj)}); err != nil {
		t.Fatalf("Error on fourth sync: %v", err)
	}
}

func TestMaxConcurrencyWithNewlyTriggeredJobs(t *testing.T) {
	type testCase struct {
		Name         string
		PJs          []prowapi.ProwJob
		PendingJobs  map[string]int
		ExpectedPods int
	}

	tests := []testCase{
		{
			Name: "avoid starting a triggered job",
			PJs: []prowapi.ProwJob{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "first",
					},
					Spec: prowapi.ProwJobSpec{
						Job:            "test-bazel-build",
						Type:           prowapi.PostsubmitJob,
						MaxConcurrency: 1,
						PodSpec:        &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
						Refs:           &prowapi.Refs{Org: "fejtaverse"},
					},
					Status: prowapi.ProwJobStatus{
						State: prowapi.TriggeredState,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "second",
						CreationTimestamp: metav1.Now(),
					},
					Spec: prowapi.ProwJobSpec{
						Job:            "test-bazel-build",
						Type:           prowapi.PostsubmitJob,
						MaxConcurrency: 1,
						PodSpec:        &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
						Refs:           &prowapi.Refs{Org: "fejtaverse"},
					},
					Status: prowapi.ProwJobStatus{
						State: prowapi.TriggeredState,
					},
				},
			},
			PendingJobs:  make(map[string]int),
			ExpectedPods: 1,
		},
		{
			Name: "both triggered jobs can start",
			PJs: []prowapi.ProwJob{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "first",
					},
					Spec: prowapi.ProwJobSpec{
						Job:            "test-bazel-build",
						Type:           prowapi.PostsubmitJob,
						MaxConcurrency: 2,
						PodSpec:        &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
						Refs:           &prowapi.Refs{Org: "fejtaverse"},
					},
					Status: prowapi.ProwJobStatus{
						State: prowapi.TriggeredState,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "second",
					},
					Spec: prowapi.ProwJobSpec{
						Job:            "test-bazel-build",
						Type:           prowapi.PostsubmitJob,
						MaxConcurrency: 2,
						PodSpec:        &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
						Refs:           &prowapi.Refs{Org: "fejtaverse"},
					},
					Status: prowapi.ProwJobStatus{
						State: prowapi.TriggeredState,
					},
				},
			},
			PendingJobs:  make(map[string]int),
			ExpectedPods: 2,
		},
		{
			Name: "no triggered job can start",
			PJs: []prowapi.ProwJob{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "first",
						CreationTimestamp: metav1.Now(),
					},
					Spec: prowapi.ProwJobSpec{
						Job:            "test-bazel-build",
						Type:           prowapi.PostsubmitJob,
						MaxConcurrency: 5,
						PodSpec:        &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
						Refs:           &prowapi.Refs{Org: "fejtaverse"},
					},
					Status: prowapi.ProwJobStatus{
						State: prowapi.TriggeredState,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "second",
						CreationTimestamp: metav1.Now(),
					},
					Spec: prowapi.ProwJobSpec{
						Job:            "test-bazel-build",
						Type:           prowapi.PostsubmitJob,
						MaxConcurrency: 5,
						PodSpec:        &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
						Refs:           &prowapi.Refs{Org: "fejtaverse"},
					},
					Status: prowapi.ProwJobStatus{
						State: prowapi.TriggeredState,
					},
				},
			},
			PendingJobs:  map[string]int{"test-bazel-build": 5},
			ExpectedPods: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			jobs := make(chan prowapi.ProwJob, len(test.PJs))
			for _, pj := range test.PJs {
				jobs <- pj
			}
			close(jobs)

			prowJobs := make([]runtime.Object, 0)
			for i := range test.PJs {
				test.PJs[i].Namespace = "prowjobs"
				test.PJs[i].Spec.Agent = prowapi.KubernetesAgent
				test.PJs[i].UID = types.UID(strconv.Itoa(i))
				prowJobs = append(prowJobs, &test.PJs[i])
			}

			ctx := context.Background()
			config := newFakeConfigAgent(t, 0, nil).Config

			fakeMgr, err := testutil.NewFakeManager(
				ctx,
				prowJobs,
				func(ctx context.Context, indexer ctrlruntimeclient.FieldIndexer) error {
					return setupIndexes(ctx, indexer, config)
				},
			)
			if err != nil {
				t.Fatalf("Failed to setup fake manager: %v", err)
			}
			fakeProwJobClient := fakeMgr.GetClient()

			buildClients := map[string]buildClient{
				prowapi.DefaultClusterAlias: {
					Client: fakectrlruntimeclient.NewClientBuilder().Build(),
				},
			}

			for jobName, numJobsToCreate := range test.PendingJobs {
				for i := 0; i < numJobsToCreate; i++ {
					if err := fakeProwJobClient.Create(ctx, &prowapi.ProwJob{
						ObjectMeta: metav1.ObjectMeta{
							Name:      fmt.Sprintf("%s-%d", jobName, i),
							Namespace: "prowjobs",
						},
						Spec: prowapi.ProwJobSpec{
							Agent: prowapi.KubernetesAgent,
							Job:   jobName,
						},
						Status: prowapi.ProwJobStatus{
							State: prowapi.PendingState,
						},
					}); err != nil {
						t.Fatalf("failed to create prowJob: %v", err)
					}
				}
			}
			r := newReconciler(ctx, fakeProwJobClient, nil, config, nil, "")
			r.buildClients = buildClients
			for _, job := range test.PJs {
				request := reconcile.Request{NamespacedName: types.NamespacedName{
					Name:      job.Name,
					Namespace: job.Namespace,
				}}
				if _, err := r.Reconcile(ctx, request); err != nil {
					t.Fatalf("failed to reconcile job %s: %v", request.String(), err)
				}
			}

			podsAfterSync := &v1.PodList{}
			if err := buildClients[prowapi.DefaultClusterAlias].List(ctx, podsAfterSync); err != nil {
				t.Fatalf("could not list pods from the client: %v", err)
			}
			if len(podsAfterSync.Items) != test.ExpectedPods {
				t.Errorf("expected pods: %d, got: %d", test.ExpectedPods, len(podsAfterSync.Items))
			}
		})
	}
}

func TestMaxConcurrency(t *testing.T) {
	type pendingJob struct {
		Duplicates int
		JobQueue   string
	}

	type testCase struct {
		Name               string
		JobQueueCapacities map[string]int
		ProwJob            prowapi.ProwJob
		ExistingProwJobs   []prowapi.ProwJob
		PendingJobs        map[string]pendingJob

		ExpectedResult bool
	}
	testCases := []testCase{
		{
			Name:           "Max concurrency 0 always runs",
			ProwJob:        prowapi.ProwJob{Spec: prowapi.ProwJobSpec{MaxConcurrency: 0}},
			ExpectedResult: true,
		},
		{
			Name: "Num pending exceeds max concurrency",
			ProwJob: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.Now()},
				Spec: prowapi.ProwJobSpec{
					MaxConcurrency: 10,
					Job:            "my-pj",
				},
			},
			PendingJobs:    map[string]pendingJob{"my-pj": {Duplicates: 10}},
			ExpectedResult: false,
		},
		{
			Name: "Num pending plus older instances equals max concurrency",
			ProwJob: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.Now(),
				},
				Spec: prowapi.ProwJobSpec{
					MaxConcurrency: 10,
					Job:            "my-pj",
				},
			},
			ExistingProwJobs: []prowapi.ProwJob{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "prowjobs"},
					Spec:       prowapi.ProwJobSpec{Agent: prowapi.KubernetesAgent, Job: "my-pj"},
					Status: prowapi.ProwJobStatus{
						State: prowapi.TriggeredState,
					},
				},
			},
			PendingJobs:    map[string]pendingJob{"my-pj": {Duplicates: 9}},
			ExpectedResult: false,
		},
		{
			Name: "Num pending plus older instances exceeds max concurrency",
			ProwJob: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.Now(),
				},
				Spec: prowapi.ProwJobSpec{
					MaxConcurrency: 10,
					Job:            "my-pj",
				},
			},
			ExistingProwJobs: []prowapi.ProwJob{
				{
					Spec: prowapi.ProwJobSpec{Job: "my-pj"},
					Status: prowapi.ProwJobStatus{
						State: prowapi.TriggeredState,
					},
				},
			},
			PendingJobs:    map[string]pendingJob{"my-pj": {Duplicates: 10}},
			ExpectedResult: false,
		},
		{
			Name: "Have other jobs that are newer, can execute",
			ProwJob: prowapi.ProwJob{
				Spec: prowapi.ProwJobSpec{
					MaxConcurrency: 1,
					Job:            "my-pj",
				},
			},
			ExistingProwJobs: []prowapi.ProwJob{
				{
					ObjectMeta: metav1.ObjectMeta{
						CreationTimestamp: metav1.Now(),
					},
					Spec: prowapi.ProwJobSpec{Job: "my-pj"},
					Status: prowapi.ProwJobStatus{
						State: prowapi.TriggeredState,
					},
				},
			},
			ExpectedResult: true,
		},
		{
			Name: "Have older jobs that are not triggered, can execute",
			ProwJob: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.Now(),
				},
				Spec: prowapi.ProwJobSpec{
					MaxConcurrency: 2,
					Job:            "my-pj",
				},
			},
			ExistingProwJobs: []prowapi.ProwJob{
				{
					Spec: prowapi.ProwJobSpec{Job: "my-pj"},
					Status: prowapi.ProwJobStatus{
						CompletionTime: &[]metav1.Time{{}}[0],
					},
				},
			},
			PendingJobs:    map[string]pendingJob{"my-pj": {Duplicates: 1}},
			ExpectedResult: true,
		},
		{
			Name:               "Job queue capacity 0 never runs",
			ProwJob:            prowapi.ProwJob{Spec: prowapi.ProwJobSpec{JobQueueName: "queue"}},
			JobQueueCapacities: map[string]int{"queue": 0},
			ExpectedResult:     false,
		},
		{
			Name:               "Job queue capacity -1 always runs",
			ProwJob:            prowapi.ProwJob{Spec: prowapi.ProwJobSpec{JobQueueName: "queue"}},
			JobQueueCapacities: map[string]int{"queue": -1},
			ExpectedResult:     true,
		},
		{
			Name: "Num pending within max concurrency but exceeds job queue concurrency",
			ProwJob: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.Now()},
				Spec: prowapi.ProwJobSpec{
					MaxConcurrency: 100,
					Job:            "my-pj",
					JobQueueName:   "queue",
				},
			},
			JobQueueCapacities: map[string]int{"queue": 10},
			PendingJobs:        map[string]pendingJob{"my-pj": {Duplicates: 10, JobQueue: "queue"}},
			ExpectedResult:     false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			if tc.PendingJobs == nil {
				tc.PendingJobs = map[string]pendingJob{}
			}
			buildClients := map[string]buildClient{}
			logrus.SetLevel(logrus.DebugLevel)

			prowJobs := make([]runtime.Object, 0)
			for i := range tc.ExistingProwJobs {
				tc.ExistingProwJobs[i].Namespace = "prowjobs"
				prowJobs = append(prowJobs, &tc.ExistingProwJobs[i])
			}

			for jobName, jobsToCreateParams := range tc.PendingJobs {
				for i := 0; i < jobsToCreateParams.Duplicates; i++ {
					prowJobs = append(prowJobs, &prowapi.ProwJob{
						ObjectMeta: metav1.ObjectMeta{
							Name:      fmt.Sprintf("%s-%d", jobName, i),
							Namespace: "prowjobs",
						},
						Spec: prowapi.ProwJobSpec{
							Agent:        prowapi.KubernetesAgent,
							Job:          jobName,
							JobQueueName: jobsToCreateParams.JobQueue,
						},
						Status: prowapi.ProwJobStatus{
							State: prowapi.PendingState,
						},
					})
				}
			}

			ctx := context.Background()
			config := newFakeConfigAgent(t, 0, tc.JobQueueCapacities).Config

			fakeMgr, err := testutil.NewFakeManager(
				ctx,
				prowJobs,
				func(ctx context.Context, indexer ctrlruntimeclient.FieldIndexer) error {
					return setupIndexes(ctx, indexer, config)
				},
			)
			if err != nil {
				t.Fatalf("Failed to setup fake manager: %v", err)
			}

			r := &reconciler{
				pjClient:     fakeMgr.GetClient(),
				buildClients: buildClients,
				log:          logrus.NewEntry(logrus.StandardLogger()),
				config:       config,
				clock:        clock.RealClock{},
			}
			// We filter ourselves out via the UID, so make sure its not the empty string
			tc.ProwJob.UID = types.UID("under-test")
			result, err := r.canExecuteConcurrently(ctx, &tc.ProwJob)
			if err != nil {
				t.Fatalf("canExecuteConcurrently: %v", err)
			}

			if result != tc.ExpectedResult {
				t.Errorf("Expected max_concurrency to allow job: %t, result was %t", tc.ExpectedResult, result)
			}
		})
	}
}

type patchTrackingFakeClient struct {
	ctrlruntimeclient.Client
	patched sets.Set[string]
}

func (c *patchTrackingFakeClient) Patch(ctx context.Context, obj ctrlruntimeclient.Object, patch ctrlruntimeclient.Patch, opts ...ctrlruntimeclient.PatchOption) error {
	if c.patched == nil {
		c.patched = sets.New[string]()
	}
	c.patched.Insert(obj.GetName())
	return c.Client.Patch(ctx, obj, patch, opts...)
}

type deleteTrackingFakeClient struct {
	deleteError error
	ctrlruntimeclient.Client
	deleted sets.Set[string]
}

func (c *deleteTrackingFakeClient) Delete(ctx context.Context, obj ctrlruntimeclient.Object, opts ...ctrlruntimeclient.DeleteOption) error {
	if c.deleteError != nil {
		return c.deleteError
	}
	if c.deleted == nil {
		c.deleted = sets.Set[string]{}
	}
	if err := c.Client.Delete(ctx, obj, opts...); err != nil {
		return err
	}
	c.deleted.Insert(obj.GetName())
	return nil
}

type clientWrapper struct {
	ctrlruntimeclient.Client
	createError              error
	errOnDeleteWithFinalizer bool
}

func (c *clientWrapper) Create(ctx context.Context, obj ctrlruntimeclient.Object, opts ...ctrlruntimeclient.CreateOption) error {
	if c.createError != nil {
		return c.createError
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *clientWrapper) Delete(ctx context.Context, obj ctrlruntimeclient.Object, opts ...ctrlruntimeclient.DeleteOption) error {
	if len(obj.GetFinalizers()) > 0 {
		return fmt.Errorf("object still had finalizers when attempting to delete: %v", obj.GetFinalizers())
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func TestSyncAbortedJob(t *testing.T) {
	t.Parallel()

	type testCase struct {
		Name           string
		Pod            *v1.Pod
		DeleteError    error
		ExpectSyncFail bool
		ExpectDelete   bool
		ExpectComplete bool
	}

	testCases := []testCase{
		{
			Name:           "Pod is deleted",
			Pod:            &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "my-pj"}},
			ExpectDelete:   true,
			ExpectComplete: true,
		},
		{
			Name:           "No pod there",
			ExpectDelete:   false,
			ExpectComplete: true,
		},
		{
			Name:           "NotFound on delete is tolerated",
			Pod:            &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "my-pj"}},
			DeleteError:    kapierrors.NewNotFound(schema.GroupResource{}, "my-pj"),
			ExpectDelete:   false,
			ExpectComplete: true,
		},
		{
			Name:           "Failed delete does not set job to completed",
			Pod:            &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "my-pj"}},
			DeleteError:    errors.New("erroring as requested"),
			ExpectSyncFail: true,
			ExpectDelete:   false,
			ExpectComplete: false,
		},
	}

	const cluster = "cluster"
	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			pj := &prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-pj",
				},
				Spec: prowapi.ProwJobSpec{
					Cluster: cluster,
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.AbortedState,
				},
			}

			builder := fakectrlruntimeclient.NewClientBuilder()
			if tc.Pod != nil {
				builder.WithRuntimeObjects(tc.Pod)
			}
			podClient := &deleteTrackingFakeClient{
				deleteError: tc.DeleteError,
				Client:      builder.Build(),
			}

			ctx := context.Background()
			config := func() *config.Config { return &config.Config{} }

			fakeMgr, err := testutil.NewFakeManager(
				ctx,
				[]runtime.Object{pj},
				func(ctx context.Context, indexer ctrlruntimeclient.FieldIndexer) error {
					return setupIndexes(ctx, indexer, config)
				},
			)
			if err != nil {
				t.Fatalf("Failed to setup fake manager: %v", err)
			}

			pjClient := fakeMgr.GetClient()
			r := &reconciler{
				log:          logrus.NewEntry(logrus.New()),
				config:       config,
				pjClient:     fakeMgr.GetClient(),
				buildClients: map[string]buildClient{cluster: {Client: podClient}},
			}

			res, err := r.reconcile(ctx, pj)
			if (err != nil) != tc.ExpectSyncFail {
				t.Fatalf("sync failed: %v, expected it to fail: %t", err, tc.ExpectSyncFail)
			}
			if res != nil {
				t.Errorf("expected reconcile.Result to be nil, was %v", res)
			}

			if err := pjClient.Get(ctx, types.NamespacedName{Name: pj.Name}, pj); err != nil {
				t.Fatalf("failed to get job from client: %v", err)
			}
			if pj.Complete() != tc.ExpectComplete {
				t.Errorf("expected complete: %t, got complete: %t", tc.ExpectComplete, pj.Complete())
			}

			if tc.ExpectDelete != podClient.deleted.Has(pj.Name) {
				t.Errorf("expected delete: %t, got delete: %t", tc.ExpectDelete, podClient.deleted.Has(pj.Name))
			}
		})
	}
}

func TestProwJobPredicate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		obj        ctrlruntimeclient.Object
		wantResult bool
	}{
		{
			name:       "Accept PJ",
			obj:        &prowapi.ProwJob{Spec: prowapi.ProwJobSpec{Agent: prowapi.KubernetesAgent}},
			wantResult: true,
		},
		{
			name: "Filter scheduling",
			obj: &prowapi.ProwJob{
				Spec:   prowapi.ProwJobSpec{Agent: prowapi.KubernetesAgent},
				Status: prowapi.ProwJobStatus{State: prowapi.SchedulingState},
			},
		},
		{
			name: "Filter completed",
			obj: &prowapi.ProwJob{
				Spec:   prowapi.ProwJobSpec{Agent: prowapi.KubernetesAgent},
				Status: prowapi.ProwJobStatus{CompletionTime: &metav1.Time{}},
			},
		},
		{
			name: "Filter non k8s agent",
			obj:  &prowapi.ProwJob{Spec: prowapi.ProwJobSpec{Agent: prowapi.JenkinsAgent}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			predicate := prowJobPredicate(nil)

			actualResult := predicate.Create(event.CreateEvent{Object: tc.obj}) &&
				predicate.Update(event.UpdateEvent{ObjectNew: tc.obj}) &&
				predicate.Delete(event.DeleteEvent{Object: tc.obj}) &&
				predicate.Generic(event.GenericEvent{Object: tc.obj})

			if actualResult != tc.wantResult {
				t.Errorf("Expected %t but got %t", tc.wantResult, actualResult)
			}
		})
	}
}

func TestPodPredicate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		obj        *v1.Pod
		selector   string
		wantResult bool
	}{
		{
			name:       "Accept Pod if created by Prow",
			obj:        &v1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{kube.CreatedByProw: "true"}}},
			wantResult: true,
		},
		{
			name:       "Accept Pod if matches additional selector",
			obj:        &v1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{kube.CreatedByProw: "true", "foo": "bar"}}},
			selector:   "foo=bar",
			wantResult: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			predicate, err := podPredicate(tc.selector, nil)
			if err != nil {
				t.Fatalf("Failed to create pod predicate: %v", err)
			}

			actualResult := predicate.Create(event.TypedCreateEvent[*corev1.Pod]{Object: tc.obj}) &&
				predicate.Update(event.TypedUpdateEvent[*corev1.Pod]{ObjectNew: tc.obj}) &&
				predicate.Delete(event.TypedDeleteEvent[*corev1.Pod]{Object: tc.obj}) &&
				predicate.Generic(event.TypedGenericEvent[*corev1.Pod]{Object: tc.obj})

			if actualResult != tc.wantResult {
				t.Errorf("Expected %t but got %t", tc.wantResult, actualResult)
			}
		})
	}
}
