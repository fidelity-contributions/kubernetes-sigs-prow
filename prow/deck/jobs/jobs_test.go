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

package jobs

import (
	"fmt"
	"testing"

	coreapi "k8s.io/api/core/v1"
	"k8s.io/test-infra/prow/kube"
)

type fkc []kube.ProwJob

func (f fkc) ListProwJobs(s string) ([]kube.ProwJob, error) {
	return f, nil
}

type fpkc string

func (f fpkc) GetLogs(name string, opts *coreapi.PodLogOptions) ([]byte, error) {
	if opts.Container != kube.TestContainerName {
		return nil, fmt.Errorf("wrong container: %s", opts.Container)
	}
	if name == "wowowow" || name == "powowow" {
		return []byte(f), nil
	}
	return nil, fmt.Errorf("pod not found: %s", name)
}

func TestGetLog(t *testing.T) {
	kc := fkc{
		kube.ProwJob{
			Spec: kube.ProwJobSpec{
				Agent: kube.KubernetesAgent,
				Job:   "job",
			},
			Status: kube.ProwJobStatus{
				PodName: "wowowow",
				BuildID: "123",
			},
		},
		kube.ProwJob{
			Spec: kube.ProwJobSpec{
				Agent:   kube.KubernetesAgent,
				Job:     "jib",
				Cluster: "trusted",
			},
			Status: kube.ProwJobStatus{
				PodName: "powowow",
				BuildID: "123",
			},
		},
	}
	ja := &JobAgent{
		kc:   kc,
		pkcs: map[string]PodLogClient{kube.DefaultClusterAlias: fpkc("clusterA"), "trusted": fpkc("clusterB")},
	}
	if err := ja.update(); err != nil {
		t.Fatalf("Updating: %v", err)
	}
	if res, err := ja.GetJobLog("job", "123"); err != nil {
		t.Fatalf("Failed to get log: %v", err)
	} else if got, expect := string(res), "clusterA"; got != expect {
		t.Errorf("Unexpected result getting logs for job 'job'. Expected %q, but got %q.", expect, got)
	}

	if res, err := ja.GetJobLog("jib", "123"); err != nil {
		t.Fatalf("Failed to get log: %v", err)
	} else if got, expect := string(res), "clusterB"; got != expect {
		t.Errorf("Unexpected result getting logs for job 'job'. Expected %q, but got %q.", expect, got)
	}
}

func TestProwJobs(t *testing.T) {
	kc := fkc{
		kube.ProwJob{
			Spec: kube.ProwJobSpec{
				Agent: kube.KubernetesAgent,
				Job:   "job",
				Refs: &kube.Refs{
					Org:  "kubernetes",
					Repo: "test-infra",
				},
			},
			Status: kube.ProwJobStatus{
				PodName: "wowowow",
				BuildID: "123",
			},
		},
	}
	ja := &JobAgent{
		kc:   kc,
		pkcs: map[string]PodLogClient{kube.DefaultClusterAlias: fpkc("")},
	}
	if err := ja.update(); err != nil {
		t.Fatalf("Updating: %v", err)
	}
	pjs := ja.ProwJobs()
	if expect, got := 1, len(pjs); expect != got {
		t.Fatalf("Expected %d prowjobs, but got %d.", expect, got)
	}
	if expect, got := "kubernetes", pjs[0].Spec.Refs.Org; expect != got {
		t.Errorf("Expected prowjob to have org %q, but got %q.", expect, got)
	}
}
