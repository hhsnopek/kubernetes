/*
Copyright 2014 The Kubernetes Authors.

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

package volume

import (
	"context"
	"fmt"
	"math/rand"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	ref "k8s.io/client-go/tools/reference"
	fakecloud "k8s.io/cloud-provider/fake"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	persistentvolumecontroller "k8s.io/kubernetes/pkg/controller/volume/persistentvolume"
	"k8s.io/kubernetes/pkg/volume"
	volumetest "k8s.io/kubernetes/pkg/volume/testing"
	"k8s.io/kubernetes/test/integration/framework"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
)

// Several tests in this file are configurable by environment variables:
// KUBE_INTEGRATION_PV_OBJECTS - nr. of PVs/PVCs to be created
//      (100 by default)
// KUBE_INTEGRATION_PV_SYNC_PERIOD - volume controller sync period
//      (1s by default)
// KUBE_INTEGRATION_PV_END_SLEEP - for how long should
//      TestPersistentVolumeMultiPVsPVCs sleep when it's finished (0s by
//      default). This is useful to test how long does it take for periodic sync
//      to process bound PVs/PVCs.
//
const defaultObjectCount = 100
const defaultSyncPeriod = 1 * time.Second

const provisionerPluginName = "kubernetes.io/mock-provisioner"

func getObjectCount() int {
	objectCount := defaultObjectCount
	if s := os.Getenv("KUBE_INTEGRATION_PV_OBJECTS"); s != "" {
		var err error
		objectCount, err = strconv.Atoi(s)
		if err != nil {
			klog.Fatalf("cannot parse value of KUBE_INTEGRATION_PV_OBJECTS: %v", err)
		}
	}
	klog.V(2).Infof("using KUBE_INTEGRATION_PV_OBJECTS=%d", objectCount)
	return objectCount
}

func getSyncPeriod(syncPeriod time.Duration) time.Duration {
	period := syncPeriod
	if s := os.Getenv("KUBE_INTEGRATION_PV_SYNC_PERIOD"); s != "" {
		var err error
		period, err = time.ParseDuration(s)
		if err != nil {
			klog.Fatalf("cannot parse value of KUBE_INTEGRATION_PV_SYNC_PERIOD: %v", err)
		}
	}
	klog.V(2).Infof("using KUBE_INTEGRATION_PV_SYNC_PERIOD=%v", period)
	return period
}

func testSleep() {
	var period time.Duration
	if s := os.Getenv("KUBE_INTEGRATION_PV_END_SLEEP"); s != "" {
		var err error
		period, err = time.ParseDuration(s)
		if err != nil {
			klog.Fatalf("cannot parse value of KUBE_INTEGRATION_PV_END_SLEEP: %v", err)
		}
	}
	klog.V(2).Infof("using KUBE_INTEGRATION_PV_END_SLEEP=%v", period)
	if period != 0 {
		time.Sleep(period)
		klog.V(2).Infof("sleep finished")
	}
}

func TestPersistentVolumeRecycler(t *testing.T) {
	klog.V(2).Infof("TestPersistentVolumeRecycler started")
	_, s, closeFn := framework.RunAnAPIServer(nil)
	defer closeFn()

	ns := framework.CreateTestingNamespace("pv-recycler", t)
	defer framework.DeleteTestingNamespace(ns, t)

	testClient, ctrl, informers, watchPV, watchPVC := createClients(ns, t, s, defaultSyncPeriod)
	defer watchPV.Stop()
	defer watchPVC.Stop()

	// NOTE: This test cannot run in parallel, because it is creating and deleting
	// non-namespaced objects (PersistenceVolumes).
	defer testClient.CoreV1().PersistentVolumes().DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})

	ctx, cancel := context.WithCancel(context.TODO())
	informers.Start(ctx.Done())
	go ctrl.Run(ctx)
	defer cancel()

	// This PV will be claimed, released, and recycled.
	pv := createPV("fake-pv-recycler", "/tmp/foo", "10G", []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}, v1.PersistentVolumeReclaimRecycle)
	pvc := createPVC("fake-pvc-recycler", ns.Name, "5G", []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}, "")

	_, err := testClient.CoreV1().PersistentVolumes().Create(context.TODO(), pv, metav1.CreateOptions{})
	if err != nil {
		t.Errorf("Failed to create PersistentVolume: %v", err)
	}
	klog.V(2).Infof("TestPersistentVolumeRecycler pvc created")

	_, err = testClient.CoreV1().PersistentVolumeClaims(ns.Name).Create(context.TODO(), pvc, metav1.CreateOptions{})
	if err != nil {
		t.Errorf("Failed to create PersistentVolumeClaim: %v", err)
	}
	klog.V(2).Infof("TestPersistentVolumeRecycler pvc created")

	// wait until the controller pairs the volume and claim
	waitForPersistentVolumePhase(testClient, pv.Name, watchPV, v1.VolumeBound)
	klog.V(2).Infof("TestPersistentVolumeRecycler pv bound")
	waitForPersistentVolumeClaimPhase(testClient, pvc.Name, ns.Name, watchPVC, v1.ClaimBound)
	klog.V(2).Infof("TestPersistentVolumeRecycler pvc bound")

	// deleting a claim releases the volume, after which it can be recycled
	if err := testClient.CoreV1().PersistentVolumeClaims(ns.Name).Delete(context.TODO(), pvc.Name, metav1.DeleteOptions{}); err != nil {
		t.Errorf("error deleting claim %s", pvc.Name)
	}
	klog.V(2).Infof("TestPersistentVolumeRecycler pvc deleted")

	waitForPersistentVolumePhase(testClient, pv.Name, watchPV, v1.VolumeReleased)
	klog.V(2).Infof("TestPersistentVolumeRecycler pv released")
	waitForPersistentVolumePhase(testClient, pv.Name, watchPV, v1.VolumeAvailable)
	klog.V(2).Infof("TestPersistentVolumeRecycler pv available")
}

func TestPersistentVolumeDeleter(t *testing.T) {
	klog.V(2).Infof("TestPersistentVolumeDeleter started")
	_, s, closeFn := framework.RunAnAPIServer(nil)
	defer closeFn()

	ns := framework.CreateTestingNamespace("pv-deleter", t)
	defer framework.DeleteTestingNamespace(ns, t)

	testClient, ctrl, informers, watchPV, watchPVC := createClients(ns, t, s, defaultSyncPeriod)
	defer watchPV.Stop()
	defer watchPVC.Stop()

	// NOTE: This test cannot run in parallel, because it is creating and deleting
	// non-namespaced objects (PersistenceVolumes).
	defer testClient.CoreV1().PersistentVolumes().DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})

	ctx, cancel := context.WithCancel(context.TODO())
	informers.Start(ctx.Done())
	go ctrl.Run(ctx)
	defer cancel()

	// This PV will be claimed, released, and deleted.
	pv := createPV("fake-pv-deleter", "/tmp/foo", "10G", []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}, v1.PersistentVolumeReclaimDelete)
	pvc := createPVC("fake-pvc-deleter", ns.Name, "5G", []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}, "")

	_, err := testClient.CoreV1().PersistentVolumes().Create(context.TODO(), pv, metav1.CreateOptions{})
	if err != nil {
		t.Errorf("Failed to create PersistentVolume: %v", err)
	}
	klog.V(2).Infof("TestPersistentVolumeDeleter pv created")
	_, err = testClient.CoreV1().PersistentVolumeClaims(ns.Name).Create(context.TODO(), pvc, metav1.CreateOptions{})
	if err != nil {
		t.Errorf("Failed to create PersistentVolumeClaim: %v", err)
	}
	klog.V(2).Infof("TestPersistentVolumeDeleter pvc created")
	waitForPersistentVolumePhase(testClient, pv.Name, watchPV, v1.VolumeBound)
	klog.V(2).Infof("TestPersistentVolumeDeleter pv bound")
	waitForPersistentVolumeClaimPhase(testClient, pvc.Name, ns.Name, watchPVC, v1.ClaimBound)
	klog.V(2).Infof("TestPersistentVolumeDeleter pvc bound")

	// deleting a claim releases the volume, after which it can be recycled
	if err := testClient.CoreV1().PersistentVolumeClaims(ns.Name).Delete(context.TODO(), pvc.Name, metav1.DeleteOptions{}); err != nil {
		t.Errorf("error deleting claim %s", pvc.Name)
	}
	klog.V(2).Infof("TestPersistentVolumeDeleter pvc deleted")

	waitForPersistentVolumePhase(testClient, pv.Name, watchPV, v1.VolumeReleased)
	klog.V(2).Infof("TestPersistentVolumeDeleter pv released")

	for {
		event := <-watchPV.ResultChan()
		if event.Type == watch.Deleted {
			break
		}
	}
	klog.V(2).Infof("TestPersistentVolumeDeleter pv deleted")
}

func TestPersistentVolumeBindRace(t *testing.T) {
	// Test a race binding many claims to a PV that is pre-bound to a specific
	// PVC. Only this specific PVC should get bound.
	klog.V(2).Infof("TestPersistentVolumeBindRace started")
	_, s, closeFn := framework.RunAnAPIServer(nil)
	defer closeFn()

	ns := framework.CreateTestingNamespace("pv-bind-race", t)
	defer framework.DeleteTestingNamespace(ns, t)

	testClient, ctrl, informers, watchPV, watchPVC := createClients(ns, t, s, defaultSyncPeriod)
	defer watchPV.Stop()
	defer watchPVC.Stop()

	// NOTE: This test cannot run in parallel, because it is creating and deleting
	// non-namespaced objects (PersistenceVolumes).
	defer testClient.CoreV1().PersistentVolumes().DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})

	ctx, cancel := context.WithCancel(context.TODO())
	informers.Start(ctx.Done())
	go ctrl.Run(ctx)
	defer cancel()

	pv := createPV("fake-pv-race", "/tmp/foo", "10G", []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}, v1.PersistentVolumeReclaimRetain)
	pvc := createPVC("fake-pvc-race", ns.Name, "5G", []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}, "")
	counter := 0
	maxClaims := 100
	claims := []*v1.PersistentVolumeClaim{}
	for counter <= maxClaims {
		counter++
		newPvc := pvc.DeepCopy()
		newPvc.ObjectMeta = metav1.ObjectMeta{Name: fmt.Sprintf("fake-pvc-race-%d", counter)}
		claim, err := testClient.CoreV1().PersistentVolumeClaims(ns.Name).Create(context.TODO(), newPvc, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Error creating newPvc: %v", err)
		}
		claims = append(claims, claim)
	}
	klog.V(2).Infof("TestPersistentVolumeBindRace claims created")

	// putting a bind manually on a pv should only match the claim it is bound to
	claim := claims[rand.Intn(maxClaims-1)]
	claimRef, err := ref.GetReference(legacyscheme.Scheme, claim)
	if err != nil {
		t.Fatalf("Unexpected error getting claimRef: %v", err)
	}
	pv.Spec.ClaimRef = claimRef
	pv.Spec.ClaimRef.UID = ""

	pv, err = testClient.CoreV1().PersistentVolumes().Create(context.TODO(), pv, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Unexpected error creating pv: %v", err)
	}
	klog.V(2).Infof("TestPersistentVolumeBindRace pv created, pre-bound to %s", claim.Name)

	waitForPersistentVolumePhase(testClient, pv.Name, watchPV, v1.VolumeBound)
	klog.V(2).Infof("TestPersistentVolumeBindRace pv bound")
	waitForAnyPersistentVolumeClaimPhase(watchPVC, v1.ClaimBound)
	klog.V(2).Infof("TestPersistentVolumeBindRace pvc bound")

	pv, err = testClient.CoreV1().PersistentVolumes().Get(context.TODO(), pv.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error getting pv: %v", err)
	}
	if pv.Spec.ClaimRef == nil {
		t.Fatalf("Unexpected nil claimRef")
	}
	if pv.Spec.ClaimRef.Namespace != claimRef.Namespace || pv.Spec.ClaimRef.Name != claimRef.Name {
		t.Fatalf("Bind mismatch! Expected %s/%s but got %s/%s", claimRef.Namespace, claimRef.Name, pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name)
	}
}

// TestPersistentVolumeClaimLabelSelector test binding using label selectors
func TestPersistentVolumeClaimLabelSelector(t *testing.T) {
	_, s, closeFn := framework.RunAnAPIServer(nil)
	defer closeFn()

	ns := framework.CreateTestingNamespace("pvc-label-selector", t)
	defer framework.DeleteTestingNamespace(ns, t)

	testClient, controller, informers, watchPV, watchPVC := createClients(ns, t, s, defaultSyncPeriod)
	defer watchPV.Stop()
	defer watchPVC.Stop()

	// NOTE: This test cannot run in parallel, because it is creating and deleting
	// non-namespaced objects (PersistenceVolumes).
	defer testClient.CoreV1().PersistentVolumes().DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})

	ctx, cancel := context.WithCancel(context.TODO())
	informers.Start(ctx.Done())
	go controller.Run(ctx)
	defer cancel()

	var (
		err     error
		modes   = []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}
		reclaim = v1.PersistentVolumeReclaimRetain

		pvTrue  = createPV("pv-true", "/tmp/foo-label", "1G", modes, reclaim)
		pvFalse = createPV("pv-false", "/tmp/foo-label", "1G", modes, reclaim)
		pvc     = createPVC("pvc-ls-1", ns.Name, "1G", modes, "")
	)

	pvTrue.ObjectMeta.SetLabels(map[string]string{"foo": "true"})
	pvFalse.ObjectMeta.SetLabels(map[string]string{"foo": "false"})

	_, err = testClient.CoreV1().PersistentVolumes().Create(context.TODO(), pvTrue, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create PersistentVolume: %v", err)
	}
	_, err = testClient.CoreV1().PersistentVolumes().Create(context.TODO(), pvFalse, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create PersistentVolume: %v", err)
	}
	t.Log("volumes created")

	pvc.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"foo": "true",
		},
	}

	_, err = testClient.CoreV1().PersistentVolumeClaims(ns.Name).Create(context.TODO(), pvc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create PersistentVolumeClaim: %v", err)
	}
	t.Log("claim created")

	waitForAnyPersistentVolumePhase(watchPV, v1.VolumeBound)
	t.Log("volume bound")
	waitForPersistentVolumeClaimPhase(testClient, pvc.Name, ns.Name, watchPVC, v1.ClaimBound)
	t.Log("claim bound")

	pv, err := testClient.CoreV1().PersistentVolumes().Get(context.TODO(), "pv-false", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error getting pv: %v", err)
	}
	if pv.Spec.ClaimRef != nil {
		t.Fatalf("False PV shouldn't be bound")
	}
	pv, err = testClient.CoreV1().PersistentVolumes().Get(context.TODO(), "pv-true", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error getting pv: %v", err)
	}
	if pv.Spec.ClaimRef == nil {
		t.Fatalf("True PV should be bound")
	}
	if pv.Spec.ClaimRef.Namespace != pvc.Namespace || pv.Spec.ClaimRef.Name != pvc.Name {
		t.Fatalf("Bind mismatch! Expected %s/%s but got %s/%s", pvc.Namespace, pvc.Name, pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name)
	}
}

// TestPersistentVolumeClaimLabelSelectorMatchExpressions test binding using
// MatchExpressions label selectors
func TestPersistentVolumeClaimLabelSelectorMatchExpressions(t *testing.T) {
	_, s, closeFn := framework.RunAnAPIServer(nil)
	defer closeFn()

	ns := framework.CreateTestingNamespace("pvc-match-expressions", t)
	defer framework.DeleteTestingNamespace(ns, t)

	testClient, controller, informers, watchPV, watchPVC := createClients(ns, t, s, defaultSyncPeriod)
	defer watchPV.Stop()
	defer watchPVC.Stop()

	// NOTE: This test cannot run in parallel, because it is creating and deleting
	// non-namespaced objects (PersistenceVolumes).
	defer testClient.CoreV1().PersistentVolumes().DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})

	ctx, cancel := context.WithCancel(context.TODO())
	informers.Start(ctx.Done())
	go controller.Run(ctx)
	defer cancel()

	var (
		err     error
		modes   = []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}
		reclaim = v1.PersistentVolumeReclaimRetain

		pvTrue  = createPV("pv-true", "/tmp/foo-label", "1G", modes, reclaim)
		pvFalse = createPV("pv-false", "/tmp/foo-label", "1G", modes, reclaim)
		pvc     = createPVC("pvc-ls-1", ns.Name, "1G", modes, "")
	)

	pvTrue.ObjectMeta.SetLabels(map[string]string{"foo": "valA", "bar": ""})
	pvFalse.ObjectMeta.SetLabels(map[string]string{"foo": "valB", "baz": ""})

	_, err = testClient.CoreV1().PersistentVolumes().Create(context.TODO(), pvTrue, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create PersistentVolume: %v", err)
	}
	_, err = testClient.CoreV1().PersistentVolumes().Create(context.TODO(), pvFalse, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create PersistentVolume: %v", err)
	}
	t.Log("volumes created")

	pvc.Spec.Selector = &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      "foo",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{"valA"},
			},
			{
				Key:      "foo",
				Operator: metav1.LabelSelectorOpNotIn,
				Values:   []string{"valB"},
			},
			{
				Key:      "bar",
				Operator: metav1.LabelSelectorOpExists,
				Values:   []string{},
			},
			{
				Key:      "baz",
				Operator: metav1.LabelSelectorOpDoesNotExist,
				Values:   []string{},
			},
		},
	}

	_, err = testClient.CoreV1().PersistentVolumeClaims(ns.Name).Create(context.TODO(), pvc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create PersistentVolumeClaim: %v", err)
	}
	t.Log("claim created")

	waitForAnyPersistentVolumePhase(watchPV, v1.VolumeBound)
	t.Log("volume bound")
	waitForPersistentVolumeClaimPhase(testClient, pvc.Name, ns.Name, watchPVC, v1.ClaimBound)
	t.Log("claim bound")

	pv, err := testClient.CoreV1().PersistentVolumes().Get(context.TODO(), "pv-false", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error getting pv: %v", err)
	}
	if pv.Spec.ClaimRef != nil {
		t.Fatalf("False PV shouldn't be bound")
	}
	pv, err = testClient.CoreV1().PersistentVolumes().Get(context.TODO(), "pv-true", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error getting pv: %v", err)
	}
	if pv.Spec.ClaimRef == nil {
		t.Fatalf("True PV should be bound")
	}
	if pv.Spec.ClaimRef.Namespace != pvc.Namespace || pv.Spec.ClaimRef.Name != pvc.Name {
		t.Fatalf("Bind mismatch! Expected %s/%s but got %s/%s", pvc.Namespace, pvc.Name, pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name)
	}
}

// TestPersistentVolumeMultiPVs tests binding of one PVC to 100 PVs with
// different size.
func TestPersistentVolumeMultiPVs(t *testing.T) {
	_, s, closeFn := framework.RunAnAPIServer(nil)
	defer closeFn()

	ns := framework.CreateTestingNamespace("multi-pvs", t)
	defer framework.DeleteTestingNamespace(ns, t)

	testClient, controller, informers, watchPV, watchPVC := createClients(ns, t, s, defaultSyncPeriod)
	defer watchPV.Stop()
	defer watchPVC.Stop()

	// NOTE: This test cannot run in parallel, because it is creating and deleting
	// non-namespaced objects (PersistenceVolumes).
	defer testClient.CoreV1().PersistentVolumes().DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})

	ctx, cancel := context.WithCancel(context.TODO())
	informers.Start(ctx.Done())
	go controller.Run(ctx)
	defer cancel()

	maxPVs := getObjectCount()
	pvs := make([]*v1.PersistentVolume, maxPVs)
	for i := 0; i < maxPVs; i++ {
		// This PV will be claimed, released, and deleted
		pvs[i] = createPV("pv-"+strconv.Itoa(i), "/tmp/foo"+strconv.Itoa(i), strconv.Itoa(i+1)+"G",
			[]v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}, v1.PersistentVolumeReclaimRetain)
	}

	pvc := createPVC("pvc-2", ns.Name, strconv.Itoa(maxPVs/2)+"G", []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}, "")

	for i := 0; i < maxPVs; i++ {
		_, err := testClient.CoreV1().PersistentVolumes().Create(context.TODO(), pvs[i], metav1.CreateOptions{})
		if err != nil {
			t.Errorf("Failed to create PersistentVolume %d: %v", i, err)
		}
		waitForPersistentVolumePhase(testClient, pvs[i].Name, watchPV, v1.VolumeAvailable)
	}
	t.Log("volumes created")

	_, err := testClient.CoreV1().PersistentVolumeClaims(ns.Name).Create(context.TODO(), pvc, metav1.CreateOptions{})
	if err != nil {
		t.Errorf("Failed to create PersistentVolumeClaim: %v", err)
	}
	t.Log("claim created")

	// wait until the binder pairs the claim with a volume
	waitForAnyPersistentVolumePhase(watchPV, v1.VolumeBound)
	t.Log("volume bound")
	waitForPersistentVolumeClaimPhase(testClient, pvc.Name, ns.Name, watchPVC, v1.ClaimBound)
	t.Log("claim bound")

	// only one PV is bound
	bound := 0
	for i := 0; i < maxPVs; i++ {
		pv, err := testClient.CoreV1().PersistentVolumes().Get(context.TODO(), pvs[i].Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Unexpected error getting pv: %v", err)
		}
		if pv.Spec.ClaimRef == nil {
			continue
		}
		// found a bounded PV
		p := pv.Spec.Capacity[v1.ResourceStorage]
		pvCap := p.Value()
		expectedCap := resource.MustParse(strconv.Itoa(maxPVs/2) + "G")
		expectedCapVal := expectedCap.Value()
		if pv.Spec.ClaimRef.Name != pvc.Name || pvCap != expectedCapVal {
			t.Fatalf("Bind mismatch! Expected %s capacity %d but got %s capacity %d", pvc.Name, expectedCapVal, pv.Spec.ClaimRef.Name, pvCap)
		}
		t.Logf("claim bounded to %s capacity %v", pv.Name, pv.Spec.Capacity[v1.ResourceStorage])
		bound++
	}
	t.Log("volumes checked")

	if bound != 1 {
		t.Fatalf("Only 1 PV should be bound but got %d", bound)
	}

	// deleting a claim releases the volume
	if err := testClient.CoreV1().PersistentVolumeClaims(ns.Name).Delete(context.TODO(), pvc.Name, metav1.DeleteOptions{}); err != nil {
		t.Errorf("error deleting claim %s", pvc.Name)
	}
	t.Log("claim deleted")

	waitForAnyPersistentVolumePhase(watchPV, v1.VolumeReleased)
	t.Log("volumes released")
}

// TestPersistentVolumeMultiPVsPVCs tests binding of 100 PVC to 100 PVs.
// This test is configurable by KUBE_INTEGRATION_PV_* variables.
func TestPersistentVolumeMultiPVsPVCs(t *testing.T) {
	_, s, closeFn := framework.RunAnAPIServer(nil)
	defer closeFn()

	ns := framework.CreateTestingNamespace("multi-pvs-pvcs", t)
	defer framework.DeleteTestingNamespace(ns, t)

	testClient, binder, informers, watchPV, watchPVC := createClients(ns, t, s, defaultSyncPeriod)
	defer watchPV.Stop()
	defer watchPVC.Stop()

	// NOTE: This test cannot run in parallel, because it is creating and deleting
	// non-namespaced objects (PersistenceVolumes).
	defer testClient.CoreV1().PersistentVolumes().DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})

	ctx, cancel := context.WithCancel(context.TODO())
	informers.Start(ctx.Done())
	go binder.Run(ctx)
	defer cancel()

	objCount := getObjectCount()
	pvs := make([]*v1.PersistentVolume, objCount)
	pvcs := make([]*v1.PersistentVolumeClaim, objCount)
	for i := 0; i < objCount; i++ {
		// This PV will be claimed, released, and deleted
		pvs[i] = createPV("pv-"+strconv.Itoa(i), "/tmp/foo"+strconv.Itoa(i), "1G",
			[]v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}, v1.PersistentVolumeReclaimRetain)
		pvcs[i] = createPVC("pvc-"+strconv.Itoa(i), ns.Name, "1G", []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}, "")
	}

	// Create PVs first
	klog.V(2).Infof("TestPersistentVolumeMultiPVsPVCs: start")

	// Create the volumes in a separate goroutine to pop events from
	// watchPV early - it seems it has limited capacity and it gets stuck
	// with >3000 volumes.
	go func() {
		for i := 0; i < objCount; i++ {
			_, _ = testClient.CoreV1().PersistentVolumes().Create(context.TODO(), pvs[i], metav1.CreateOptions{})
		}
	}()
	// Wait for them to get Available
	for i := 0; i < objCount; i++ {
		waitForAnyPersistentVolumePhase(watchPV, v1.VolumeAvailable)
		klog.V(1).Infof("%d volumes available", i+1)
	}
	klog.V(2).Infof("TestPersistentVolumeMultiPVsPVCs: volumes are Available")

	// Start a separate goroutine that randomly modifies PVs and PVCs while the
	// binder is working. We test that the binder can bind volumes despite
	// users modifying objects underneath.
	stopCh := make(chan struct{}, 0)
	go func() {
		for {
			// Roll a dice and decide a PV or PVC to modify
			if rand.Intn(2) == 0 {
				// Modify PV
				i := rand.Intn(objCount)
				name := "pv-" + strconv.Itoa(i)
				pv, err := testClient.CoreV1().PersistentVolumes().Get(context.TODO(), name, metav1.GetOptions{})
				if err != nil {
					// Silently ignore error, the PV may have be already deleted
					// or not exists yet.
					klog.V(4).Infof("Failed to read PV %s: %v", name, err)
					continue
				}
				if pv.Annotations == nil {
					pv.Annotations = map[string]string{"TestAnnotation": fmt.Sprint(rand.Int())}
				} else {
					pv.Annotations["TestAnnotation"] = fmt.Sprint(rand.Int())
				}
				_, err = testClient.CoreV1().PersistentVolumes().Update(context.TODO(), pv, metav1.UpdateOptions{})
				if err != nil {
					// Silently ignore error, the PV may have been updated by
					// the controller.
					klog.V(4).Infof("Failed to update PV %s: %v", pv.Name, err)
					continue
				}
				klog.V(4).Infof("Updated PV %s", pv.Name)
			} else {
				// Modify PVC
				i := rand.Intn(objCount)
				name := "pvc-" + strconv.Itoa(i)
				pvc, err := testClient.CoreV1().PersistentVolumeClaims(metav1.NamespaceDefault).Get(context.TODO(), name, metav1.GetOptions{})
				if err != nil {
					// Silently ignore error, the PVC may have be already
					// deleted or not exists yet.
					klog.V(4).Infof("Failed to read PVC %s: %v", name, err)
					continue
				}
				if pvc.Annotations == nil {
					pvc.Annotations = map[string]string{"TestAnnotation": fmt.Sprint(rand.Int())}
				} else {
					pvc.Annotations["TestAnnotation"] = fmt.Sprint(rand.Int())
				}
				_, err = testClient.CoreV1().PersistentVolumeClaims(metav1.NamespaceDefault).Update(context.TODO(), pvc, metav1.UpdateOptions{})
				if err != nil {
					// Silently ignore error, the PVC may have been updated by
					// the controller.
					klog.V(4).Infof("Failed to update PVC %s: %v", pvc.Name, err)
					continue
				}
				klog.V(4).Infof("Updated PVC %s", pvc.Name)
			}

			select {
			case <-stopCh:
				return
			default:
				continue
			}
		}
	}()

	// Create the claims, again in a separate goroutine.
	go func() {
		for i := 0; i < objCount; i++ {
			_, _ = testClient.CoreV1().PersistentVolumeClaims(ns.Name).Create(context.TODO(), pvcs[i], metav1.CreateOptions{})
		}
	}()

	// wait until the binder pairs all claims
	for i := 0; i < objCount; i++ {
		waitForAnyPersistentVolumeClaimPhase(watchPVC, v1.ClaimBound)
		klog.V(1).Infof("%d claims bound", i+1)
	}
	// wait until the binder pairs all volumes
	for i := 0; i < objCount; i++ {
		waitForPersistentVolumePhase(testClient, pvs[i].Name, watchPV, v1.VolumeBound)
		klog.V(1).Infof("%d claims bound", i+1)
	}

	klog.V(2).Infof("TestPersistentVolumeMultiPVsPVCs: claims are bound")
	stopCh <- struct{}{}

	// check that everything is bound to something
	for i := 0; i < objCount; i++ {
		pv, err := testClient.CoreV1().PersistentVolumes().Get(context.TODO(), pvs[i].Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Unexpected error getting pv: %v", err)
		}
		if pv.Spec.ClaimRef == nil {
			t.Fatalf("PV %q is not bound", pv.Name)
		}
		klog.V(2).Infof("PV %q is bound to PVC %q", pv.Name, pv.Spec.ClaimRef.Name)

		pvc, err := testClient.CoreV1().PersistentVolumeClaims(ns.Name).Get(context.TODO(), pvcs[i].Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Unexpected error getting pvc: %v", err)
		}
		if pvc.Spec.VolumeName == "" {
			t.Fatalf("PVC %q is not bound", pvc.Name)
		}
		klog.V(2).Infof("PVC %q is bound to PV %q", pvc.Name, pvc.Spec.VolumeName)
	}
	testSleep()
}

// TestPersistentVolumeControllerStartup tests startup of the controller.
// The controller should not unbind any volumes when it starts.
func TestPersistentVolumeControllerStartup(t *testing.T) {
	_, s, closeFn := framework.RunAnAPIServer(nil)
	defer closeFn()

	ns := framework.CreateTestingNamespace("controller-startup", t)
	defer framework.DeleteTestingNamespace(ns, t)

	objCount := getObjectCount()

	const shortSyncPeriod = 2 * time.Second
	syncPeriod := getSyncPeriod(shortSyncPeriod)

	testClient, binder, informers, watchPV, watchPVC := createClients(ns, t, s, shortSyncPeriod)
	defer watchPV.Stop()
	defer watchPVC.Stop()

	// Create *bound* volumes and PVCs
	pvs := make([]*v1.PersistentVolume, objCount)
	pvcs := make([]*v1.PersistentVolumeClaim, objCount)
	for i := 0; i < objCount; i++ {
		pvName := "pv-startup-" + strconv.Itoa(i)
		pvcName := "pvc-startup-" + strconv.Itoa(i)

		pvc := createPVC(pvcName, ns.Name, "1G", []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}, "")
		pvc.Annotations = map[string]string{"annBindCompleted": ""}
		pvc.Spec.VolumeName = pvName
		newPVC, err := testClient.CoreV1().PersistentVolumeClaims(ns.Name).Create(context.TODO(), pvc, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Cannot create claim %q: %v", pvc.Name, err)
		}
		// Save Bound status as a separate transaction
		newPVC.Status.Phase = v1.ClaimBound
		newPVC, err = testClient.CoreV1().PersistentVolumeClaims(ns.Name).UpdateStatus(context.TODO(), newPVC, metav1.UpdateOptions{})
		if err != nil {
			t.Fatalf("Cannot update claim status %q: %v", pvc.Name, err)
		}
		pvcs[i] = newPVC
		// Drain watchPVC with all events generated by the PVC until it's bound
		// We don't want to catch "PVC created with Status.Phase == Pending"
		// later in this test.
		waitForAnyPersistentVolumeClaimPhase(watchPVC, v1.ClaimBound)

		pv := createPV(pvName, "/tmp/foo"+strconv.Itoa(i), "1G",
			[]v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}, v1.PersistentVolumeReclaimRetain)
		claimRef, err := ref.GetReference(legacyscheme.Scheme, newPVC)
		if err != nil {
			klog.V(3).Infof("unexpected error getting claim reference: %v", err)
			return
		}
		pv.Spec.ClaimRef = claimRef
		newPV, err := testClient.CoreV1().PersistentVolumes().Create(context.TODO(), pv, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Cannot create volume %q: %v", pv.Name, err)
		}
		// Save Bound status as a separate transaction
		newPV.Status.Phase = v1.VolumeBound
		newPV, err = testClient.CoreV1().PersistentVolumes().UpdateStatus(context.TODO(), newPV, metav1.UpdateOptions{})
		if err != nil {
			t.Fatalf("Cannot update volume status %q: %v", pv.Name, err)
		}
		pvs[i] = newPV
		// Drain watchPV with all events generated by the PV until it's bound
		// We don't want to catch "PV created with Status.Phase == Pending"
		// later in this test.
		waitForAnyPersistentVolumePhase(watchPV, v1.VolumeBound)
	}

	// Start the controller when all PVs and PVCs are already saved in etcd
	ctx, cancel := context.WithCancel(context.TODO())
	informers.Start(ctx.Done())
	go binder.Run(ctx)
	defer cancel()

	// wait for at least two sync periods for changes. No volume should be
	// Released and no claim should be Lost during this time.
	timer := time.NewTimer(2 * syncPeriod)
	defer timer.Stop()
	finished := false
	for !finished {
		select {
		case volumeEvent := <-watchPV.ResultChan():
			volume, ok := volumeEvent.Object.(*v1.PersistentVolume)
			if !ok {
				continue
			}
			if volume.Status.Phase != v1.VolumeBound {
				t.Errorf("volume %s unexpectedly changed state to %s", volume.Name, volume.Status.Phase)
			}

		case claimEvent := <-watchPVC.ResultChan():
			claim, ok := claimEvent.Object.(*v1.PersistentVolumeClaim)
			if !ok {
				continue
			}
			if claim.Status.Phase != v1.ClaimBound {
				t.Errorf("claim %s unexpectedly changed state to %s", claim.Name, claim.Status.Phase)
			}

		case <-timer.C:
			// Wait finished
			klog.V(2).Infof("Wait finished")
			finished = true
		}
	}

	// check that everything is bound to something
	for i := 0; i < objCount; i++ {
		pv, err := testClient.CoreV1().PersistentVolumes().Get(context.TODO(), pvs[i].Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Unexpected error getting pv: %v", err)
		}
		if pv.Spec.ClaimRef == nil {
			t.Fatalf("PV %q is not bound", pv.Name)
		}
		klog.V(2).Infof("PV %q is bound to PVC %q", pv.Name, pv.Spec.ClaimRef.Name)

		pvc, err := testClient.CoreV1().PersistentVolumeClaims(ns.Name).Get(context.TODO(), pvcs[i].Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Unexpected error getting pvc: %v", err)
		}
		if pvc.Spec.VolumeName == "" {
			t.Fatalf("PVC %q is not bound", pvc.Name)
		}
		klog.V(2).Infof("PVC %q is bound to PV %q", pvc.Name, pvc.Spec.VolumeName)
	}
}

// TestPersistentVolumeProvisionMultiPVCs tests provisioning of many PVCs.
// This test is configurable by KUBE_INTEGRATION_PV_* variables.
func TestPersistentVolumeProvisionMultiPVCs(t *testing.T) {
	_, s, closeFn := framework.RunAnAPIServer(nil)
	defer closeFn()

	ns := framework.CreateTestingNamespace("provision-multi-pvs", t)
	defer framework.DeleteTestingNamespace(ns, t)

	testClient, binder, informers, watchPV, watchPVC := createClients(ns, t, s, defaultSyncPeriod)
	defer watchPV.Stop()
	defer watchPVC.Stop()

	// NOTE: This test cannot run in parallel, because it is creating and deleting
	// non-namespaced objects (PersistenceVolumes and StorageClasses).
	defer testClient.CoreV1().PersistentVolumes().DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})
	defer testClient.StorageV1().StorageClasses().DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})

	storageClass := storage.StorageClass{
		TypeMeta: metav1.TypeMeta{
			Kind: "StorageClass",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "gold",
		},
		Provisioner: provisionerPluginName,
	}
	testClient.StorageV1().StorageClasses().Create(context.TODO(), &storageClass, metav1.CreateOptions{})

	ctx, cancel := context.WithCancel(context.TODO())
	informers.Start(ctx.Done())
	go binder.Run(ctx)
	defer cancel()

	objCount := getObjectCount()
	pvcs := make([]*v1.PersistentVolumeClaim, objCount)
	for i := 0; i < objCount; i++ {
		pvc := createPVC("pvc-provision-"+strconv.Itoa(i), ns.Name, "1G", []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}, "gold")
		pvcs[i] = pvc
	}

	klog.V(2).Infof("TestPersistentVolumeProvisionMultiPVCs: start")
	// Create the claims in a separate goroutine to pop events from watchPVC
	// early. It gets stuck with >3000 claims.
	go func() {
		for i := 0; i < objCount; i++ {
			_, _ = testClient.CoreV1().PersistentVolumeClaims(ns.Name).Create(context.TODO(), pvcs[i], metav1.CreateOptions{})
		}
	}()

	// Wait until the controller provisions and binds all of them
	for i := 0; i < objCount; i++ {
		waitForAnyPersistentVolumeClaimPhase(watchPVC, v1.ClaimBound)
		klog.V(1).Infof("%d claims bound", i+1)
	}
	klog.V(2).Infof("TestPersistentVolumeProvisionMultiPVCs: claims are bound")

	// check that we have enough bound PVs
	pvList, err := testClient.CoreV1().PersistentVolumes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list volumes: %s", err)
	}
	if len(pvList.Items) != objCount {
		t.Fatalf("Expected to get %d volumes, got %d", objCount, len(pvList.Items))
	}
	for i := 0; i < objCount; i++ {
		pv := &pvList.Items[i]
		if pv.Status.Phase != v1.VolumeBound {
			t.Fatalf("Expected volume %s to be bound, is %s instead", pv.Name, pv.Status.Phase)
		}
		klog.V(2).Infof("PV %q is bound to PVC %q", pv.Name, pv.Spec.ClaimRef.Name)
	}

	// Delete the claims
	for i := 0; i < objCount; i++ {
		_ = testClient.CoreV1().PersistentVolumeClaims(ns.Name).Delete(context.TODO(), pvcs[i].Name, metav1.DeleteOptions{})
	}

	// Wait for the PVs to get deleted by listing remaining volumes
	// (delete events were unreliable)
	for {
		volumes, err := testClient.CoreV1().PersistentVolumes().List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			t.Fatalf("Failed to list volumes: %v", err)
		}

		klog.V(1).Infof("%d volumes remaining", len(volumes.Items))
		if len(volumes.Items) == 0 {
			break
		}
		time.Sleep(time.Second)
	}
	klog.V(2).Infof("TestPersistentVolumeProvisionMultiPVCs: volumes are deleted")
}

// TestPersistentVolumeMultiPVsDiffAccessModes tests binding of one PVC to two
// PVs with different access modes.
func TestPersistentVolumeMultiPVsDiffAccessModes(t *testing.T) {
	_, s, closeFn := framework.RunAnAPIServer(nil)
	defer closeFn()

	ns := framework.CreateTestingNamespace("multi-pvs-diff-access", t)
	defer framework.DeleteTestingNamespace(ns, t)

	testClient, controller, informers, watchPV, watchPVC := createClients(ns, t, s, defaultSyncPeriod)
	defer watchPV.Stop()
	defer watchPVC.Stop()

	// NOTE: This test cannot run in parallel, because it is creating and deleting
	// non-namespaced objects (PersistenceVolumes).
	defer testClient.CoreV1().PersistentVolumes().DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})

	ctx, cancel := context.WithCancel(context.TODO())
	informers.Start(ctx.Done())
	go controller.Run(ctx)
	defer cancel()

	// This PV will be claimed, released, and deleted
	pvRwo := createPV("pv-rwo", "/tmp/foo", "10G",
		[]v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}, v1.PersistentVolumeReclaimRetain)
	pvRwm := createPV("pv-rwm", "/tmp/bar", "10G",
		[]v1.PersistentVolumeAccessMode{v1.ReadWriteMany}, v1.PersistentVolumeReclaimRetain)

	pvc := createPVC("pvc-rwm", ns.Name, "5G", []v1.PersistentVolumeAccessMode{v1.ReadWriteMany}, "")

	_, err := testClient.CoreV1().PersistentVolumes().Create(context.TODO(), pvRwm, metav1.CreateOptions{})
	if err != nil {
		t.Errorf("Failed to create PersistentVolume: %v", err)
	}
	_, err = testClient.CoreV1().PersistentVolumes().Create(context.TODO(), pvRwo, metav1.CreateOptions{})
	if err != nil {
		t.Errorf("Failed to create PersistentVolume: %v", err)
	}
	t.Log("volumes created")

	_, err = testClient.CoreV1().PersistentVolumeClaims(ns.Name).Create(context.TODO(), pvc, metav1.CreateOptions{})
	if err != nil {
		t.Errorf("Failed to create PersistentVolumeClaim: %v", err)
	}
	t.Log("claim created")

	// wait until the controller pairs the volume and claim
	waitForAnyPersistentVolumePhase(watchPV, v1.VolumeBound)
	t.Log("volume bound")
	waitForPersistentVolumeClaimPhase(testClient, pvc.Name, ns.Name, watchPVC, v1.ClaimBound)
	t.Log("claim bound")

	// only RWM PV is bound
	pv, err := testClient.CoreV1().PersistentVolumes().Get(context.TODO(), "pv-rwo", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error getting pv: %v", err)
	}
	if pv.Spec.ClaimRef != nil {
		t.Fatalf("ReadWriteOnce PV shouldn't be bound")
	}
	pv, err = testClient.CoreV1().PersistentVolumes().Get(context.TODO(), "pv-rwm", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error getting pv: %v", err)
	}
	if pv.Spec.ClaimRef == nil {
		t.Fatalf("ReadWriteMany PV should be bound")
	}
	if pv.Spec.ClaimRef.Name != pvc.Name {
		t.Fatalf("Bind mismatch! Expected %s but got %s", pvc.Name, pv.Spec.ClaimRef.Name)
	}

	// deleting a claim releases the volume
	if err := testClient.CoreV1().PersistentVolumeClaims(ns.Name).Delete(context.TODO(), pvc.Name, metav1.DeleteOptions{}); err != nil {
		t.Errorf("error deleting claim %s", pvc.Name)
	}
	t.Log("claim deleted")

	waitForAnyPersistentVolumePhase(watchPV, v1.VolumeReleased)
	t.Log("volume released")
}

func waitForPersistentVolumePhase(client *clientset.Clientset, pvName string, w watch.Interface, phase v1.PersistentVolumePhase) {
	// Check if the volume is already in requested phase
	volume, err := client.CoreV1().PersistentVolumes().Get(context.TODO(), pvName, metav1.GetOptions{})
	if err == nil && volume.Status.Phase == phase {
		return
	}

	// Wait for the phase
	for {
		event := <-w.ResultChan()
		volume, ok := event.Object.(*v1.PersistentVolume)
		if !ok {
			continue
		}
		if volume.Status.Phase == phase && volume.Name == pvName {
			klog.V(2).Infof("volume %q is %s", volume.Name, phase)
			break
		}
	}
}

func waitForPersistentVolumeClaimPhase(client *clientset.Clientset, claimName, namespace string, w watch.Interface, phase v1.PersistentVolumeClaimPhase) {
	// Check if the claim is already in requested phase
	claim, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(context.TODO(), claimName, metav1.GetOptions{})
	if err == nil && claim.Status.Phase == phase {
		return
	}

	// Wait for the phase
	for {
		event := <-w.ResultChan()
		claim, ok := event.Object.(*v1.PersistentVolumeClaim)
		if !ok {
			continue
		}
		if claim.Status.Phase == phase && claim.Name == claimName {
			klog.V(2).Infof("claim %q is %s", claim.Name, phase)
			break
		}
	}
}

func waitForAnyPersistentVolumePhase(w watch.Interface, phase v1.PersistentVolumePhase) {
	for {
		event := <-w.ResultChan()
		volume, ok := event.Object.(*v1.PersistentVolume)
		if !ok {
			continue
		}
		if volume.Status.Phase == phase {
			klog.V(2).Infof("volume %q is %s", volume.Name, phase)
			break
		}
	}
}

func waitForAnyPersistentVolumeClaimPhase(w watch.Interface, phase v1.PersistentVolumeClaimPhase) {
	for {
		event := <-w.ResultChan()
		claim, ok := event.Object.(*v1.PersistentVolumeClaim)
		if !ok {
			continue
		}
		if claim.Status.Phase == phase {
			klog.V(2).Infof("claim %q is %s", claim.Name, phase)
			break
		}
	}
}

func createClients(ns *v1.Namespace, t *testing.T, s *httptest.Server, syncPeriod time.Duration) (*clientset.Clientset, *persistentvolumecontroller.PersistentVolumeController, informers.SharedInformerFactory, watch.Interface, watch.Interface) {
	// Use higher QPS and Burst, there is a test for race conditions which
	// creates many objects and default values were too low.
	binderClient := clientset.NewForConfigOrDie(&restclient.Config{
		Host:          s.URL,
		ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}},
		QPS:           1000000,
		Burst:         1000000,
	})
	testClient := clientset.NewForConfigOrDie(&restclient.Config{
		Host:          s.URL,
		ContentConfig: restclient.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}},
		QPS:           1000000,
		Burst:         1000000,
	})

	host := volumetest.NewFakeVolumeHost(t, "/tmp/fake", nil, nil)
	plugin := &volumetest.FakeVolumePlugin{
		PluginName:             provisionerPluginName,
		Host:                   host,
		Config:                 volume.VolumeConfig{},
		LastProvisionerOptions: volume.VolumeOptions{},
		NewAttacherCallCount:   0,
		NewDetacherCallCount:   0,
		Mounters:               nil,
		Unmounters:             nil,
		Attachers:              nil,
		Detachers:              nil,
	}
	plugins := []volume.VolumePlugin{plugin}
	cloud := &fakecloud.Cloud{}
	informers := informers.NewSharedInformerFactory(testClient, getSyncPeriod(syncPeriod))
	ctrl, err := persistentvolumecontroller.NewController(
		persistentvolumecontroller.ControllerParameters{
			KubeClient:                binderClient,
			SyncPeriod:                getSyncPeriod(syncPeriod),
			VolumePlugins:             plugins,
			Cloud:                     cloud,
			VolumeInformer:            informers.Core().V1().PersistentVolumes(),
			ClaimInformer:             informers.Core().V1().PersistentVolumeClaims(),
			ClassInformer:             informers.Storage().V1().StorageClasses(),
			PodInformer:               informers.Core().V1().Pods(),
			NodeInformer:              informers.Core().V1().Nodes(),
			EnableDynamicProvisioning: true,
		})
	if err != nil {
		t.Fatalf("Failed to construct PersistentVolumes: %v", err)
	}

	watchPV, err := testClient.CoreV1().PersistentVolumes().Watch(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to watch PersistentVolumes: %v", err)
	}
	watchPVC, err := testClient.CoreV1().PersistentVolumeClaims(ns.Name).Watch(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to watch PersistentVolumeClaims: %v", err)
	}

	return testClient, ctrl, informers, watchPV, watchPVC
}

func createPV(name, path, cap string, mode []v1.PersistentVolumeAccessMode, reclaim v1.PersistentVolumeReclaimPolicy) *v1.PersistentVolume {
	return &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource:        v1.PersistentVolumeSource{HostPath: &v1.HostPathVolumeSource{Path: path}},
			Capacity:                      v1.ResourceList{v1.ResourceName(v1.ResourceStorage): resource.MustParse(cap)},
			AccessModes:                   mode,
			PersistentVolumeReclaimPolicy: reclaim,
		},
	}
}

func createPVC(name, namespace, cap string, mode []v1.PersistentVolumeAccessMode, class string) *v1.PersistentVolumeClaim {
	return &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			Resources:        v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceName(v1.ResourceStorage): resource.MustParse(cap)}},
			AccessModes:      mode,
			StorageClassName: &class,
		},
	}
}
