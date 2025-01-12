/*
Copyright the Velero contributors.

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

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	testclocks "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/stretchr/testify/mock"

	corev1api "k8s.io/api/core/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vmware-tanzu/velero/internal/volume"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/builder"
	"github.com/vmware-tanzu/velero/pkg/metrics"
	persistencemocks "github.com/vmware-tanzu/velero/pkg/persistence/mocks"
	"github.com/vmware-tanzu/velero/pkg/plugin/clientmgmt"
	pluginmocks "github.com/vmware-tanzu/velero/pkg/plugin/mocks"
	velerotest "github.com/vmware-tanzu/velero/pkg/test"
	"github.com/vmware-tanzu/velero/pkg/util/results"
)

func TestRestoreFinalizerReconcile(t *testing.T) {
	defaultStorageLocation := builder.ForBackupStorageLocation("velero", "default").Provider("myCloud").Bucket("bucket").Result()
	now, err := time.Parse(time.RFC1123Z, time.RFC1123Z)
	require.NoError(t, err)
	now = now.Local()
	timestamp := metav1.NewTime(now)
	assert.NotNil(t, timestamp)

	rfrTests := []struct {
		name                  string
		restore               *velerov1api.Restore
		backup                *velerov1api.Backup
		location              *velerov1api.BackupStorageLocation
		expectError           bool
		expectPhase           velerov1api.RestorePhase
		expectWarningsCnt     int
		expectErrsCnt         int
		statusCompare         bool
		expectedCompletedTime *metav1.Time
	}{
		{
			name:          "Restore is not awaiting finalization, skip",
			restore:       builder.ForRestore(velerov1api.DefaultNamespace, "restore-1").Phase(velerov1api.RestorePhaseInProgress).Result(),
			expectError:   false,
			expectPhase:   velerov1api.RestorePhaseInProgress,
			statusCompare: false,
		},
		{
			name:                  "Upon completion of all finalization tasks in the 'FinalizingPartiallyFailed' phase, the restore process transit to the 'PartiallyFailed' phase.",
			restore:               builder.ForRestore(velerov1api.DefaultNamespace, "restore-1").Phase(velerov1api.RestorePhaseFinalizingPartiallyFailed).Backup("backup-1").Result(),
			backup:                defaultBackup().StorageLocation("default").Result(),
			location:              defaultStorageLocation,
			expectError:           false,
			expectPhase:           velerov1api.RestorePhasePartiallyFailed,
			statusCompare:         true,
			expectedCompletedTime: &timestamp,
			expectWarningsCnt:     0,
			expectErrsCnt:         0,
		},
		{
			name:                  "Upon completion of all finalization tasks in the 'Finalizing' phase, the restore process transit to the 'Completed' phase.",
			restore:               builder.ForRestore(velerov1api.DefaultNamespace, "restore-1").Phase(velerov1api.RestorePhaseFinalizing).Backup("backup-1").Result(),
			backup:                defaultBackup().StorageLocation("default").Result(),
			location:              defaultStorageLocation,
			expectError:           false,
			expectPhase:           velerov1api.RestorePhaseCompleted,
			statusCompare:         true,
			expectedCompletedTime: &timestamp,
			expectWarningsCnt:     0,
			expectErrsCnt:         0,
		},
		{
			name:        "Backup not exist",
			restore:     builder.ForRestore(velerov1api.DefaultNamespace, "restore-1").Phase(velerov1api.RestorePhaseFinalizing).Backup("backup-2").Result(),
			expectError: false,
		},
		{
			name:          "Restore not exist",
			restore:       builder.ForRestore("unknown", "restore-1").Phase(velerov1api.RestorePhaseFinalizing).Result(),
			expectError:   false,
			statusCompare: false,
		},
	}

	for _, test := range rfrTests {
		t.Run(test.name, func(t *testing.T) {
			if test.restore == nil {
				return
			}

			var (
				fakeClient    = velerotest.NewFakeControllerRuntimeClientBuilder(t).Build()
				logger        = velerotest.NewLogger()
				pluginManager = &pluginmocks.Manager{}
				backupStore   = &persistencemocks.BackupStore{}
			)

			defer func() {
				// reset defaultStorageLocation resourceVersion
				defaultStorageLocation.ObjectMeta.ResourceVersion = ""
			}()

			r := NewRestoreFinalizerReconciler(
				logger,
				velerov1api.DefaultNamespace,
				fakeClient,
				func(logrus.FieldLogger) clientmgmt.Manager { return pluginManager },
				NewFakeSingleObjectBackupStoreGetter(backupStore),
				metrics.NewServerMetrics(),
				fakeClient,
			)
			r.clock = testclocks.NewFakeClock(now)

			if test.restore != nil && test.restore.Namespace == velerov1api.DefaultNamespace {
				require.NoError(t, r.Client.Create(context.Background(), test.restore))
				backupStore.On("GetRestoredResourceList", test.restore.Name).Return(map[string][]string{}, nil)
			}
			if test.backup != nil {
				assert.NoError(t, r.Client.Create(context.Background(), test.backup))
				backupStore.On("GetBackupVolumeInfos", test.backup.Name).Return(nil, nil)
				pluginManager.On("GetRestoreItemActionsV2").Return(nil, nil)
				pluginManager.On("CleanupClients")
			}
			if test.location != nil {
				require.NoError(t, r.Client.Create(context.Background(), test.location))
			}

			_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: test.restore.Namespace,
				Name:      test.restore.Name,
			}})

			assert.Equal(t, test.expectError, err != nil)
			if test.expectError {
				return
			}

			if test.statusCompare {
				restoreAfter := velerov1api.Restore{}
				err = fakeClient.Get(context.TODO(), types.NamespacedName{
					Namespace: test.restore.Namespace,
					Name:      test.restore.Name,
				}, &restoreAfter)

				require.NoError(t, err)

				assert.Equal(t, test.expectPhase, restoreAfter.Status.Phase)
				assert.Equal(t, test.expectErrsCnt, restoreAfter.Status.Errors)
				assert.Equal(t, test.expectWarningsCnt, restoreAfter.Status.Warnings)
				require.True(t, test.expectedCompletedTime.Equal(restoreAfter.Status.CompletionTimestamp))
			}
		})
	}

}

func TestUpdateResult(t *testing.T) {
	var (
		fakeClient    = velerotest.NewFakeControllerRuntimeClientBuilder(t).Build()
		logger        = velerotest.NewLogger()
		pluginManager = &pluginmocks.Manager{}
		backupStore   = &persistencemocks.BackupStore{}
	)

	r := NewRestoreFinalizerReconciler(
		logger,
		velerov1api.DefaultNamespace,
		fakeClient,
		func(logrus.FieldLogger) clientmgmt.Manager { return pluginManager },
		NewFakeSingleObjectBackupStoreGetter(backupStore),
		metrics.NewServerMetrics(),
		fakeClient,
	)
	restore := builder.ForRestore(velerov1api.DefaultNamespace, "restore-1").Result()
	res := map[string]results.Result{"warnings": {}, "errors": {}}

	backupStore.On("GetRestoreResults", restore.Name).Return(res, nil)
	backupStore.On("PutRestoreResults", mock.Anything, mock.Anything, mock.Anything).Return(nil)

	err := r.updateResults(backupStore, restore, &results.Result{}, &results.Result{})
	require.NoError(t, err)
}

func TestPatchDynamicPVWithVolumeInfo(t *testing.T) {
	tests := []struct {
		name             string
		volumeInfo       []*volume.VolumeInfo
		restoredPVCNames map[string]struct{}
		restore          *velerov1api.Restore
		restoredPVC      []*corev1api.PersistentVolumeClaim
		restoredPV       []*corev1api.PersistentVolume
		expectedPatch    map[string]volume.PVInfo
		expectedErrNum   int
	}{
		{
			name:           "no applicable volumeInfo",
			volumeInfo:     []*volume.VolumeInfo{{BackupMethod: "VeleroNativeSnapshot", PVCName: "pvc1"}},
			restore:        builder.ForRestore(velerov1api.DefaultNamespace, "restore").Result(),
			expectedPatch:  nil,
			expectedErrNum: 0,
		},
		{
			name:           "no restored PVC",
			volumeInfo:     []*volume.VolumeInfo{{BackupMethod: "PodVolumeBackup", PVCName: "pvc1"}},
			restore:        builder.ForRestore(velerov1api.DefaultNamespace, "restore").Result(),
			expectedPatch:  nil,
			expectedErrNum: 0,
		},
		{
			name: "no applicable pv patch",
			volumeInfo: []*volume.VolumeInfo{{
				BackupMethod: "PodVolumeBackup",
				PVCName:      "pvc1",
				PVName:       "pv1",
				PVCNamespace: "ns1",
				PVInfo: &volume.PVInfo{
					ReclaimPolicy: string(corev1api.PersistentVolumeReclaimDelete),
					Labels:        map[string]string{"label1": "label1-val"},
				},
			}},
			restore:          builder.ForRestore(velerov1api.DefaultNamespace, "restore").Result(),
			restoredPVCNames: map[string]struct{}{"ns1/pvc1": {}},
			restoredPV: []*corev1api.PersistentVolume{
				builder.ForPersistentVolume("new-pv1").ObjectMeta(builder.WithLabels("label1", "label1-val")).ClaimRef("ns1", "pvc1").Phase(corev1api.VolumeBound).ReclaimPolicy(corev1api.PersistentVolumeReclaimDelete).Result()},
			restoredPVC: []*corev1api.PersistentVolumeClaim{
				builder.ForPersistentVolumeClaim("ns1", "pvc1").VolumeName("new-pv1").Phase(corev1api.ClaimBound).Result(),
			},
			expectedPatch:  nil,
			expectedErrNum: 0,
		},
		{
			name: "an applicable pv patch",
			volumeInfo: []*volume.VolumeInfo{{
				BackupMethod: "PodVolumeBackup",
				PVCName:      "pvc1",
				PVName:       "pv1",
				PVCNamespace: "ns1",
				PVInfo: &volume.PVInfo{
					ReclaimPolicy: string(corev1api.PersistentVolumeReclaimDelete),
					Labels:        map[string]string{"label1": "label1-val"},
				},
			}},
			restore:          builder.ForRestore(velerov1api.DefaultNamespace, "restore").Result(),
			restoredPVCNames: map[string]struct{}{"ns1/pvc1": {}},
			restoredPV: []*corev1api.PersistentVolume{
				builder.ForPersistentVolume("new-pv1").ClaimRef("ns1", "pvc1").Phase(corev1api.VolumeBound).ReclaimPolicy(corev1api.PersistentVolumeReclaimRetain).Result()},
			restoredPVC: []*corev1api.PersistentVolumeClaim{
				builder.ForPersistentVolumeClaim("ns1", "pvc1").VolumeName("new-pv1").Phase(corev1api.ClaimBound).Result(),
			},
			expectedPatch: map[string]volume.PVInfo{"new-pv1": {
				ReclaimPolicy: string(corev1api.PersistentVolumeReclaimDelete),
				Labels:        map[string]string{"label1": "label1-val"},
			}},
			expectedErrNum: 0,
		},
		{
			name: "a mapped namespace restore",
			volumeInfo: []*volume.VolumeInfo{{
				BackupMethod: "PodVolumeBackup",
				PVCName:      "pvc1",
				PVName:       "pv1",
				PVCNamespace: "ns2",
				PVInfo: &volume.PVInfo{
					ReclaimPolicy: string(corev1api.PersistentVolumeReclaimDelete),
					Labels:        map[string]string{"label1": "label1-val"},
				},
			}},
			restore:          builder.ForRestore(velerov1api.DefaultNamespace, "restore").NamespaceMappings("ns2", "ns1").Result(),
			restoredPVCNames: map[string]struct{}{"ns1/pvc1": {}},
			restoredPV: []*corev1api.PersistentVolume{
				builder.ForPersistentVolume("new-pv1").ClaimRef("ns1", "pvc1").Phase(corev1api.VolumeBound).ReclaimPolicy(corev1api.PersistentVolumeReclaimRetain).Result()},
			restoredPVC: []*corev1api.PersistentVolumeClaim{
				builder.ForPersistentVolumeClaim("ns1", "pvc1").VolumeName("new-pv1").Phase(corev1api.ClaimBound).Result(),
			},
			expectedPatch: map[string]volume.PVInfo{"new-pv1": {
				ReclaimPolicy: string(corev1api.PersistentVolumeReclaimDelete),
				Labels:        map[string]string{"label1": "label1-val"},
			}},
			expectedErrNum: 0,
		},
		{
			name: "two applicable pv patches",
			volumeInfo: []*volume.VolumeInfo{{
				BackupMethod: "PodVolumeBackup",
				PVCName:      "pvc1",
				PVName:       "pv1",
				PVCNamespace: "ns1",
				PVInfo: &volume.PVInfo{
					ReclaimPolicy: string(corev1api.PersistentVolumeReclaimDelete),
					Labels:        map[string]string{"label1": "label1-val"},
				},
			},
				{
					BackupMethod: "CSISnapshot",
					PVCName:      "pvc2",
					PVName:       "pv2",
					PVCNamespace: "ns2",
					PVInfo: &volume.PVInfo{
						ReclaimPolicy: string(corev1api.PersistentVolumeReclaimDelete),
						Labels:        map[string]string{"label2": "label2-val"},
					},
				},
			},
			restore: builder.ForRestore(velerov1api.DefaultNamespace, "restore").Result(),
			restoredPVCNames: map[string]struct{}{
				"ns1/pvc1": {},
				"ns2/pvc2": {},
			},
			restoredPV: []*corev1api.PersistentVolume{
				builder.ForPersistentVolume("new-pv1").ClaimRef("ns1", "pvc1").Phase(corev1api.VolumeBound).ReclaimPolicy(corev1api.PersistentVolumeReclaimRetain).Result(),
				builder.ForPersistentVolume("new-pv2").ClaimRef("ns2", "pvc2").Phase(corev1api.VolumeBound).ReclaimPolicy(corev1api.PersistentVolumeReclaimRetain).Result(),
			},
			restoredPVC: []*corev1api.PersistentVolumeClaim{
				builder.ForPersistentVolumeClaim("ns1", "pvc1").VolumeName("new-pv1").Phase(corev1api.ClaimBound).Result(),
				builder.ForPersistentVolumeClaim("ns2", "pvc2").VolumeName("new-pv2").Phase(corev1api.ClaimBound).Result(),
			},
			expectedPatch: map[string]volume.PVInfo{
				"new-pv1": {
					ReclaimPolicy: string(corev1api.PersistentVolumeReclaimDelete),
					Labels:        map[string]string{"label1": "label1-val"},
				},
				"new-pv2": {
					ReclaimPolicy: string(corev1api.PersistentVolumeReclaimDelete),
					Labels:        map[string]string{"label2": "label2-val"},
				},
			},
			expectedErrNum: 0,
		},
		{
			name: "an applicable pv patch with bound error",
			volumeInfo: []*volume.VolumeInfo{{
				BackupMethod: "PodVolumeBackup",
				PVCName:      "pvc1",
				PVName:       "pv1",
				PVCNamespace: "ns1",
				PVInfo: &volume.PVInfo{
					ReclaimPolicy: string(corev1api.PersistentVolumeReclaimDelete),
					Labels:        map[string]string{"label1": "label1-val"},
				},
			}},
			restore:          builder.ForRestore(velerov1api.DefaultNamespace, "restore").Result(),
			restoredPVCNames: map[string]struct{}{"ns1/pvc1": {}},
			restoredPV: []*corev1api.PersistentVolume{
				builder.ForPersistentVolume("new-pv1").ClaimRef("ns2", "pvc2").Phase(corev1api.VolumeBound).ReclaimPolicy(corev1api.PersistentVolumeReclaimRetain).Result()},
			restoredPVC: []*corev1api.PersistentVolumeClaim{
				builder.ForPersistentVolumeClaim("ns1", "pvc1").VolumeName("new-pv1").Phase(corev1api.ClaimBound).Result(),
			},
			expectedErrNum: 1,
		},
		{
			name: "two applicable pv patches with an error",
			volumeInfo: []*volume.VolumeInfo{{
				BackupMethod: "PodVolumeBackup",
				PVCName:      "pvc1",
				PVName:       "pv1",
				PVCNamespace: "ns1",
				PVInfo: &volume.PVInfo{
					ReclaimPolicy: string(corev1api.PersistentVolumeReclaimDelete),
					Labels:        map[string]string{"label1": "label1-val"},
				},
			},
				{
					BackupMethod: "CSISnapshot",
					PVCName:      "pvc2",
					PVName:       "pv2",
					PVCNamespace: "ns2",
					PVInfo: &volume.PVInfo{
						ReclaimPolicy: string(corev1api.PersistentVolumeReclaimDelete),
						Labels:        map[string]string{"label2": "label2-val"},
					},
				},
			},
			restore: builder.ForRestore(velerov1api.DefaultNamespace, "restore").Result(),
			restoredPVCNames: map[string]struct{}{
				"ns1/pvc1": {},
				"ns2/pvc2": {},
			},
			restoredPV: []*corev1api.PersistentVolume{
				builder.ForPersistentVolume("new-pv1").ClaimRef("ns1", "pvc1").Phase(corev1api.VolumeBound).ReclaimPolicy(corev1api.PersistentVolumeReclaimRetain).Result(),
				builder.ForPersistentVolume("new-pv2").ClaimRef("ns3", "pvc3").Phase(corev1api.VolumeBound).ReclaimPolicy(corev1api.PersistentVolumeReclaimRetain).Result(),
			},
			restoredPVC: []*corev1api.PersistentVolumeClaim{
				builder.ForPersistentVolumeClaim("ns1", "pvc1").VolumeName("new-pv1").Phase(corev1api.ClaimBound).Result(),
				builder.ForPersistentVolumeClaim("ns2", "pvc2").VolumeName("new-pv2").Phase(corev1api.ClaimBound).Result(),
			},
			expectedPatch: map[string]volume.PVInfo{
				"new-pv1": {
					ReclaimPolicy: string(corev1api.PersistentVolumeReclaimDelete),
					Labels:        map[string]string{"label1": "label1-val"},
				},
			},
			expectedErrNum: 1,
		},
	}

	for _, tc := range tests {
		var (
			fakeClient = velerotest.NewFakeControllerRuntimeClientBuilder(t).Build()
			logger     = velerotest.NewLogger()
		)
		ctx := &finalizerContext{
			logger:          logger,
			crClient:        fakeClient,
			restore:         tc.restore,
			restoredPVCList: tc.restoredPVCNames,
			volumeInfo:      tc.volumeInfo,
		}

		for _, pv := range tc.restoredPV {
			require.NoError(t, ctx.crClient.Create(context.Background(), pv))
		}
		for _, pvc := range tc.restoredPVC {
			require.NoError(t, ctx.crClient.Create(context.Background(), pvc))
		}

		errs := ctx.patchDynamicPVWithVolumeInfo()
		if tc.expectedErrNum > 0 {
			assert.Equal(t, tc.expectedErrNum, len(errs.Namespaces))
		}

		for pvName, expectedPVInfo := range tc.expectedPatch {
			pv := &corev1api.PersistentVolume{}
			err := ctx.crClient.Get(context.Background(), crclient.ObjectKey{Name: pvName}, pv)
			assert.NoError(t, err)

			assert.Equal(t, expectedPVInfo.ReclaimPolicy, string(pv.Spec.PersistentVolumeReclaimPolicy))
			assert.Equal(t, expectedPVInfo.Labels, pv.Labels)
		}

	}
}

func TestGetRestoredPVCFromRestoredResourceList(t *testing.T) {
	// test empty list
	restoredResourceList := map[string][]string{}
	actual := getRestoredPVCFromRestoredResourceList(restoredResourceList)
	assert.Empty(t, actual)

	// test no match
	restoredResourceList = map[string][]string{
		"v1/PersistentVolumeClaim": {
			"namespace1/pvc1(updated)",
		},
		"v1/PersistentVolume": {
			"namespace1/pv(created)",
		},
	}
	actual = getRestoredPVCFromRestoredResourceList(restoredResourceList)
	assert.Empty(t, actual)

	// test matches
	restoredResourceList = map[string][]string{
		"v1/PersistentVolumeClaim": {
			"namespace1/pvc1(created)",
			"namespace2/pvc2(updated)",
			"namespace3/pvc(3)(created)",
		},
	}
	expected := map[string]struct{}{
		"namespace1/pvc1":   {},
		"namespace3/pvc(3)": {},
	}
	actual = getRestoredPVCFromRestoredResourceList(restoredResourceList)
	assert.Equal(t, expected, actual)

}
