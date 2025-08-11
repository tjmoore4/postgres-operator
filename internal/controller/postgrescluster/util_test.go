// Copyright 2021 - 2025 Crunchy Data Solutions, Inc.
//
// SPDX-License-Identifier: Apache-2.0

package postgrescluster

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/go-logr/logr/funcr"
	"gotest.tools/v3/assert"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/crunchydata/postgres-operator/internal/controller/runtime"
	"github.com/crunchydata/postgres-operator/internal/feature"
	"github.com/crunchydata/postgres-operator/internal/initialize"
	"github.com/crunchydata/postgres-operator/internal/logging"
	"github.com/crunchydata/postgres-operator/internal/naming"
	"github.com/crunchydata/postgres-operator/internal/testing/cmp"
	"github.com/crunchydata/postgres-operator/internal/testing/events"
	"github.com/crunchydata/postgres-operator/pkg/apis/postgres-operator.crunchydata.com/v1beta1"
)

func TestSafeHash32(t *testing.T) {
	expected := errors.New("whomp")

	_, err := safeHash32(func(io.Writer) error { return expected })
	assert.Equal(t, err, expected)

	stuff, err := safeHash32(func(w io.Writer) error {
		_, _ = w.Write([]byte(`some stuff`))
		return nil
	})
	assert.NilError(t, err)
	assert.Equal(t, stuff, "574b4c7d87", "expected alphanumeric")

	same, err := safeHash32(func(w io.Writer) error {
		_, _ = w.Write([]byte(`some stuff`))
		return nil
	})
	assert.NilError(t, err)
	assert.Equal(t, same, stuff, "expected deterministic hash")
}

func TestAddDevSHM(t *testing.T) {

	testCases := []struct {
		tcName      string
		podTemplate *corev1.PodTemplateSpec
		expected    bool
	}{{
		tcName: "database and pgbackrest containers",
		podTemplate: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "database"}, {Name: "pgbackrest"}, {Name: "dontmodify"},
			}}},
		expected: true,
	}, {
		tcName: "database container only",
		podTemplate: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "database"}, {Name: "dontmodify"}}}},
		expected: true,
	}, {
		tcName: "pgbackest container only",
		podTemplate: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "dontmodify"}, {Name: "pgbackrest"}}}},
	}, {
		tcName: "other containers",
		podTemplate: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "dontmodify1"}, {Name: "dontmodify2"}}}},
	}}

	for _, tc := range testCases {
		t.Run(tc.tcName, func(t *testing.T) {

			template := tc.podTemplate

			addDevSHM(template)

			found := false

			// check there is an empty dir mounted under the dshm volume
			for _, v := range template.Spec.Volumes {
				if v.Name == "dshm" && v.EmptyDir != nil && v.EmptyDir.Medium == corev1.StorageMediumMemory {
					found = true
					break
				}
			}
			assert.Assert(t, found)

			// check that the database container contains a mount to the shared volume
			// directory
			found = false

		loop:
			for _, c := range template.Spec.Containers {
				if c.Name == naming.ContainerDatabase {
					for _, vm := range c.VolumeMounts {
						if vm.Name == "dshm" && vm.MountPath == "/dev/shm" {
							found = true
							break loop
						}
					}
				}
			}

			assert.Equal(t, tc.expected, found)
		})
	}
}

func TestAddNSSWrapper(t *testing.T) {

	image := "test-image"
	imagePullPolicy := corev1.PullAlways

	expectedResources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("200m"),
		}}

	expectedEnv := []corev1.EnvVar{
		{Name: "LD_PRELOAD", Value: "/usr/lib64/libnss_wrapper.so"},
		{Name: "NSS_WRAPPER_PASSWD", Value: "/tmp/nss_wrapper/postgres/passwd"},
		{Name: "NSS_WRAPPER_GROUP", Value: "/tmp/nss_wrapper/postgres/group"},
	}

	expectedPGAdminEnv := []corev1.EnvVar{
		{Name: "LD_PRELOAD", Value: "/usr/lib64/libnss_wrapper.so"},
		{Name: "NSS_WRAPPER_PASSWD", Value: "/tmp/nss_wrapper/pgadmin/passwd"},
		{Name: "NSS_WRAPPER_GROUP", Value: "/tmp/nss_wrapper/pgadmin/group"},
	}

	testCases := []struct {
		tcName                        string
		podTemplate                   *corev1.PodTemplateSpec
		pgadmin                       bool
		resourceProvider              string
		expectedUpdatedContainerCount int
	}{{
		tcName: "database container with pgbackrest sidecar",
		podTemplate: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: naming.ContainerDatabase, Resources: expectedResources},
				{Name: naming.PGBackRestRepoContainerName, Resources: expectedResources},
				{Name: "dontmodify"},
			}}},
		expectedUpdatedContainerCount: 2,
	}, {
		tcName: "database container only",
		podTemplate: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: naming.ContainerDatabase, Resources: expectedResources},
				{Name: "dontmodify"}}}},
		expectedUpdatedContainerCount: 1,
	}, {
		tcName: "pgbackest container only",
		podTemplate: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: naming.PGBackRestRepoContainerName, Resources: expectedResources},
				{Name: "dontmodify"},
			}}},
		expectedUpdatedContainerCount: 1,
	}, {
		tcName: "pgadmin container only",
		podTemplate: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "dontmodify"}, {Name: "pgadmin"}}}},
		pgadmin:                       true,
		expectedUpdatedContainerCount: 1,
	}, {
		tcName: "restore container only",
		podTemplate: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: naming.PGBackRestRestoreContainerName, Resources: expectedResources},
				{Name: "dontmodify"},
			}}},
		expectedUpdatedContainerCount: 1,
	}, {
		tcName: "custom database container resources",
		podTemplate: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "database",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("200m"),
						}}}}}},
		resourceProvider:              "database",
		expectedUpdatedContainerCount: 1,
	}, {
		tcName: "custom pgbackrest container resources",
		podTemplate: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "pgbackrest",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("300m"),
						}}}}}},
		resourceProvider:              "pgbackrest",
		expectedUpdatedContainerCount: 1,
	}, {
		tcName: "custom pgadmin container resources",
		podTemplate: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "pgadmin",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("400m"),
						}}}}}},
		pgadmin:                       true,
		resourceProvider:              "pgadmin",
		expectedUpdatedContainerCount: 1,
	}}

	for _, tc := range testCases {
		t.Run(tc.tcName, func(t *testing.T) {

			template := tc.podTemplate
			beforeAddNSS := template.DeepCopy().Spec.Containers

			addNSSWrapper(image, imagePullPolicy, template)

			t.Run("container-updated", func(t *testing.T) {
				// Each container that requires the nss_wrapper envs should be updated
				var actualUpdatedContainerCount int
				for i, c := range template.Spec.Containers {
					switch c.Name {
					case naming.ContainerDatabase, naming.PGBackRestRepoContainerName, naming.PGBackRestRestoreContainerName:
						assert.DeepEqual(t, expectedEnv, c.Env)
						actualUpdatedContainerCount++
					case "pgadmin":
						assert.DeepEqual(t, expectedPGAdminEnv, c.Env)
						actualUpdatedContainerCount++
					default:
						assert.DeepEqual(t, beforeAddNSS[i], c)
					}
				}
				// verify database and/or pgbackrest containers updated
				assert.Equal(t, actualUpdatedContainerCount,
					tc.expectedUpdatedContainerCount)
			})

			t.Run("init-container-added", func(t *testing.T) {
				var foundInitContainer bool
				// verify init container command, image & name
				for _, ic := range template.Spec.InitContainers {
					if ic.Name == naming.ContainerNSSWrapperInit {
						if tc.pgadmin {
							assert.Equal(t, pgAdminNSSWrapperPrefix+nssWrapperScript, ic.Command[2]) // ignore "bash -c"
						} else {
							assert.Equal(t, postgresNSSWrapperPrefix+nssWrapperScript, ic.Command[2]) // ignore "bash -c"
						}
						assert.Assert(t, ic.Image == image)
						assert.Assert(t, ic.ImagePullPolicy == imagePullPolicy)
						assert.Assert(t, !cmp.DeepEqual(ic.SecurityContext,
							&corev1.SecurityContext{})().Success())

						if tc.resourceProvider != "" {
							for _, c := range template.Spec.Containers {
								if c.Name == tc.resourceProvider {
									assert.DeepEqual(t, ic.Resources.Requests,
										c.Resources.Requests)
								}
							}
						}
						foundInitContainer = true
						break
					}
				}
				// verify init container is present
				assert.Assert(t, foundInitContainer)
			})
		})
	}
}

func TestJobCompleted(t *testing.T) {

	testCases := []struct {
		job              *batchv1.Job
		expectSuccessful bool
		testDesc         string
	}{{
		job: &batchv1.Job{
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{{
					Type:   batchv1.JobComplete,
					Status: corev1.ConditionTrue,
				}},
			},
		},
		expectSuccessful: true,
		testDesc:         "condition present and true",
	}, {
		job: &batchv1.Job{
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{{
					Type:   batchv1.JobComplete,
					Status: corev1.ConditionFalse,
				}},
			},
		},
		expectSuccessful: false,
		testDesc:         "condition present but false",
	}, {
		job: &batchv1.Job{
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{{
					Type:   batchv1.JobComplete,
					Status: corev1.ConditionUnknown,
				}},
			},
		},
		expectSuccessful: false,
		testDesc:         "condition present but unknown",
	}, {
		job:              &batchv1.Job{},
		expectSuccessful: false,
		testDesc:         "empty conditions",
	}}

	for _, tc := range testCases {
		t.Run(tc.testDesc, func(t *testing.T) {
			// first ensure jobCompleted gives the expected result
			isCompleted := jobCompleted(tc.job)
			assert.Assert(t, isCompleted == tc.expectSuccessful)
		})
	}
}

func TestJobFailed(t *testing.T) {

	testCases := []struct {
		job          *batchv1.Job
		expectFailed bool
		testDesc     string
	}{{
		job: &batchv1.Job{
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{{
					Type:   batchv1.JobFailed,
					Status: corev1.ConditionTrue,
				}},
			},
		},
		expectFailed: true,
		testDesc:     "condition present and true",
	}, {
		job: &batchv1.Job{
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{{
					Type:   batchv1.JobFailed,
					Status: corev1.ConditionFalse,
				}},
			},
		},
		expectFailed: false,
		testDesc:     "condition present but false",
	}, {
		job: &batchv1.Job{
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{{
					Type:   batchv1.JobFailed,
					Status: corev1.ConditionUnknown,
				}},
			},
		},
		expectFailed: false,
		testDesc:     "condition present but unknown",
	}, {
		job:          &batchv1.Job{},
		expectFailed: false,
		testDesc:     "empty conditions",
	}}

	for _, tc := range testCases {
		t.Run(tc.testDesc, func(t *testing.T) {
			// first ensure jobCompleted gives the expected result
			isCompleted := jobFailed(tc.job)
			assert.Assert(t, isCompleted == tc.expectFailed)
		})
	}
}

func TestStoreDesiredRequest(t *testing.T) {
	ctx := context.Background()

	setupLogCapture := func(ctx context.Context) (context.Context, *[]string) {
		calls := []string{}
		testlog := funcr.NewJSON(func(object string) {
			calls = append(calls, object)
		}, funcr.Options{
			Verbosity: 1,
		})
		return logging.NewContext(ctx, testlog), &calls
	}

	cluster := v1beta1.PostgresCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rhino",
			Namespace: "test-namespace",
		},
		Spec: v1beta1.PostgresClusterSpec{
			InstanceSets: []v1beta1.PostgresInstanceSetSpec{{
				Name:     "red",
				Replicas: initialize.Int32(1),
				DataVolumeClaimSpec: v1beta1.VolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Limits: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceStorage: resource.MustParse("1Gi"),
						}}},
			}, {
				Name:     "blue",
				Replicas: initialize.Int32(1),
			}}}}

	t.Run("BadRequestNoBackup", func(t *testing.T) {
		recorder := events.NewRecorder(t, runtime.Scheme)
		reconciler := &Reconciler{Recorder: recorder}
		ctx, logs := setupLogCapture(ctx)

		value := reconciler.storeDesiredRequest(ctx, &cluster, "pgData", "red", "woot", "")

		assert.Equal(t, value, "")
		assert.Equal(t, len(recorder.Events), 0)
		assert.Equal(t, len(*logs), 1)
		assert.Assert(t, cmp.Contains((*logs)[0], "Unable to parse pgData volume request from status"))
	})

	t.Run("BadRequestWithBackup", func(t *testing.T) {
		recorder := events.NewRecorder(t, runtime.Scheme)
		reconciler := &Reconciler{Recorder: recorder}
		ctx, logs := setupLogCapture(ctx)

		value := reconciler.storeDesiredRequest(ctx, &cluster, "pgData", "red", "foo", "1Gi")

		assert.Equal(t, value, "1Gi")
		assert.Equal(t, len(recorder.Events), 0)
		assert.Equal(t, len(*logs), 1)
		assert.Assert(t, cmp.Contains((*logs)[0], "Unable to parse pgData volume request from status (foo) for rhino/red"))
	})

	t.Run("NoLimitNoEvent", func(t *testing.T) {
		recorder := events.NewRecorder(t, runtime.Scheme)
		reconciler := &Reconciler{Recorder: recorder}
		ctx, logs := setupLogCapture(ctx)

		value := reconciler.storeDesiredRequest(ctx, &cluster, "pgData", "blue", "1Gi", "")

		assert.Equal(t, value, "1Gi")
		assert.Equal(t, len(*logs), 0)
		assert.Equal(t, len(recorder.Events), 0)
	})

	t.Run("BadBackupRequest", func(t *testing.T) {
		recorder := events.NewRecorder(t, runtime.Scheme)
		reconciler := &Reconciler{Recorder: recorder}
		ctx, logs := setupLogCapture(ctx)

		value := reconciler.storeDesiredRequest(ctx, &cluster, "pgData", "red", "2Gi", "bar")

		assert.Equal(t, value, "2Gi")
		assert.Equal(t, len(*logs), 1)
		assert.Assert(t, cmp.Contains((*logs)[0], "Unable to parse pgData volume request from status backup (bar) for rhino/red"))
		assert.Equal(t, len(recorder.Events), 1)
		assert.Equal(t, recorder.Events[0].Regarding.Name, cluster.Name)
		assert.Equal(t, recorder.Events[0].Reason, "VolumeAutoGrow")
		assert.Equal(t, recorder.Events[0].Note, "pgData volume expansion to 2Gi requested for rhino/red.")
	})

	t.Run("ValueUpdateWithEvent", func(t *testing.T) {
		recorder := events.NewRecorder(t, runtime.Scheme)
		reconciler := &Reconciler{Recorder: recorder}
		ctx, logs := setupLogCapture(ctx)

		value := reconciler.storeDesiredRequest(ctx, &cluster, "pgData", "red", "1Gi", "")

		assert.Equal(t, value, "1Gi")
		assert.Equal(t, len(*logs), 0)
		assert.Equal(t, len(recorder.Events), 1)
		assert.Equal(t, recorder.Events[0].Regarding.Name, cluster.Name)
		assert.Equal(t, recorder.Events[0].Reason, "VolumeAutoGrow")
		assert.Equal(t, recorder.Events[0].Note, "pgData volume expansion to 1Gi requested for rhino/red.")
	})

	t.Run("NoLimitNoEvent", func(t *testing.T) {
		recorder := events.NewRecorder(t, runtime.Scheme)
		reconciler := &Reconciler{Recorder: recorder}
		ctx, logs := setupLogCapture(ctx)

		value := reconciler.storeDesiredRequest(ctx, &cluster, "pgData", "blue", "1Gi", "")

		assert.Equal(t, value, "1Gi")
		assert.Equal(t, len(*logs), 0)
		assert.Equal(t, len(recorder.Events), 0)
	})
}

func TestLimitIsSet(t *testing.T) {

	cluster := v1beta1.PostgresCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rhino",
			Namespace: "test-namespace",
		},
		Spec: v1beta1.PostgresClusterSpec{
			InstanceSets: []v1beta1.PostgresInstanceSetSpec{{
				Name:     "red",
				Replicas: initialize.Int32(1),
				DataVolumeClaimSpec: v1beta1.VolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Limits: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceStorage: resource.MustParse("1Gi"),
						}}},
			}, {
				Name:     "blue",
				Replicas: initialize.Int32(1),
			}, {
				Name:     "green",
				Replicas: initialize.Int32(1),
				WALVolumeClaimSpec: &v1beta1.VolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Limits: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceStorage: resource.MustParse("2Gi"),
						}}},
			}},
			Backups: v1beta1.Backups{
				PGBackRest: v1beta1.PGBackRestArchive{
					Repos: []v1beta1.PGBackRestRepo{{
						Name: "repo1",
						Volume: &v1beta1.RepoPVC{
							VolumeClaimSpec: v1beta1.VolumeClaimSpec{
								AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
								Resources: corev1.VolumeResourceRequirements{
									Limits: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceStorage: resource.MustParse("1Gi"),
									},
								},
							}}},
						{
							Name: "repo2",
						}}}},
		}}

	testCases := []struct {
		tcName       string
		Voltype      string
		instanceName string
		expected     bool
	}{{
		tcName:       "Limit is set for instance PGDATA volume",
		Voltype:      "pgData",
		instanceName: "red",
		expected:     true,
	}, {
		tcName:       "Limit is not set for instance PGDATA volume",
		Voltype:      "pgData",
		instanceName: "blue",
		expected:     false,
	}, {
		tcName:       "Check PGDATA volume for non-existent instance",
		Voltype:      "pgData",
		instanceName: "orange",
		expected:     false,
	}, {
		tcName:       "Limit is set for instance WAL volume",
		Voltype:      "pgWAL",
		instanceName: "green",
		expected:     true,
	}, {
		tcName:       "Limit is not set for instance WAL volume",
		Voltype:      "pgWAL",
		instanceName: "red",
		expected:     false,
	}, {
		tcName:       "Check WAL volume for non-existent instance",
		Voltype:      "pgWAL",
		instanceName: "orange",
		expected:     false,
	}, {
		tcName:       "Limit is not set for repo1 volume",
		Voltype:      "repo1",
		instanceName: "",
		expected:     true,
	}, {
		tcName:       "Limit is not set for repo2 volume",
		Voltype:      "repo2",
		instanceName: "",
		expected:     false,
	}, {
		tcName:       "Check non-existent repo volume",
		Voltype:      "repo3",
		instanceName: "",
		expected:     false,
	}}

	for _, tc := range testCases {
		t.Run(tc.tcName, func(t *testing.T) {

			limitSet := limitIsSet(&cluster, tc.Voltype, tc.instanceName)
			assert.Check(t, limitSet == tc.expected)
		})
	}
}

func TestSetVolumeSize(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cluster := v1beta1.PostgresCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "elephant",
			Namespace: "test-namespace",
		},
		Spec: v1beta1.PostgresClusterSpec{
			InstanceSets: []v1beta1.PostgresInstanceSetSpec{{
				Name:     "some-instance",
				Replicas: initialize.Int32(1),
			}},
		},
	}

	instance := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "elephant-some-instance-wxyz-0",
			Namespace: cluster.Namespace,
		}}

	setupLogCapture := func(ctx context.Context) (context.Context, *[]string) {
		calls := []string{}
		testlog := funcr.NewJSON(func(object string) {
			calls = append(calls, object)
		}, funcr.Options{
			Verbosity: 1,
		})
		return logging.NewContext(ctx, testlog), &calls
	}

	// helper functions
	instanceSetSpec := func(request, limit string) *v1beta1.PostgresInstanceSetSpec {
		return &v1beta1.PostgresInstanceSetSpec{
			Name: "some-instance",
			DataVolumeClaimSpec: v1beta1.VolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceStorage: resource.MustParse(request),
					},
					Limits: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceStorage: resource.MustParse(limit),
					}}}}
	}

	desiredStatus := func(request string) v1beta1.PostgresClusterStatus {
		desiredMap := make(map[string]string)
		desiredMap["elephant-some-instance-wxyz-0"] = request
		return v1beta1.PostgresClusterStatus{
			InstanceSets: []v1beta1.PostgresInstanceSetStatus{{
				Name:                "some-instance",
				DesiredPGDataVolume: desiredMap,
			}}}
	}

	t.Run("RequestAboveLimit", func(t *testing.T) {
		recorder := events.NewRecorder(t, runtime.Scheme)
		reconciler := &Reconciler{Recorder: recorder}
		ctx, logs := setupLogCapture(ctx)

		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: naming.InstancePostgresDataVolume(instance)}
		spec := instanceSetSpec("4Gi", "3Gi")
		pvc.Spec = spec.DataVolumeClaimSpec.AsPersistentVolumeClaimSpec()

		reconciler.setVolumeSize(ctx, &cluster, pvc, "pgData", spec.Name)

		assert.Assert(t, cmp.MarshalMatches(pvc.Spec, `
accessModes:
- ReadWriteOnce
resources:
  limits:
    storage: 3Gi
  requests:
    storage: 3Gi
`))
		assert.Equal(t, len(*logs), 0)
		assert.Equal(t, len(recorder.Events), 1)
		assert.Equal(t, recorder.Events[0].Regarding.Name, cluster.Name)
		assert.Equal(t, recorder.Events[0].Reason, "VolumeRequestOverLimit")
		assert.Equal(t, recorder.Events[0].Note, "pgData volume request (4Gi) for elephant/some-instance is greater than set limit (3Gi). Limit value will be used.")
	})

	t.Run("NoFeatureGate", func(t *testing.T) {
		recorder := events.NewRecorder(t, runtime.Scheme)
		reconciler := &Reconciler{Recorder: recorder}
		ctx, logs := setupLogCapture(ctx)

		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: naming.InstancePostgresDataVolume(instance)}
		spec := instanceSetSpec("1Gi", "3Gi")

		desiredMap := make(map[string]string)
		desiredMap["elephant-some-instance-wxyz-0"] = "2Gi"
		cluster.Status = v1beta1.PostgresClusterStatus{
			InstanceSets: []v1beta1.PostgresInstanceSetStatus{{
				Name:                "some-instance",
				DesiredPGDataVolume: desiredMap,
			}},
		}

		pvc.Spec = spec.DataVolumeClaimSpec.AsPersistentVolumeClaimSpec()

		reconciler.setVolumeSize(ctx, &cluster, pvc, "pgData", spec.Name)

		assert.Assert(t, cmp.MarshalMatches(pvc.Spec, `
accessModes:
- ReadWriteOnce
resources:
  limits:
    storage: 3Gi
  requests:
    storage: 1Gi
	`))

		assert.Equal(t, len(recorder.Events), 0)
		assert.Equal(t, len(*logs), 0)

		// clear status for other tests
		cluster.Status = v1beta1.PostgresClusterStatus{}
	})

	t.Run("FeatureEnabled", func(t *testing.T) {
		gate := feature.NewGate()
		assert.NilError(t, gate.SetFromMap(map[string]bool{
			feature.AutoGrowVolumes: true,
		}))
		ctx := feature.NewContext(ctx, gate)

		t.Run("StatusNoLimit", func(t *testing.T) {
			recorder := events.NewRecorder(t, runtime.Scheme)
			reconciler := &Reconciler{Recorder: recorder}
			ctx, logs := setupLogCapture(ctx)

			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: naming.InstancePostgresDataVolume(instance)}
			spec := &v1beta1.PostgresInstanceSetSpec{
				Name: "some-instance",
				DataVolumeClaimSpec: v1beta1.VolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceStorage: resource.MustParse("1Gi"),
						}}}}
			cluster.Status = desiredStatus("2Gi")
			pvc.Spec = spec.DataVolumeClaimSpec.AsPersistentVolumeClaimSpec()

			reconciler.setVolumeSize(ctx, &cluster, pvc, "pgData", spec.Name)

			assert.Assert(t, cmp.MarshalMatches(pvc.Spec, `
accessModes:
- ReadWriteOnce
resources:
  requests:
    storage: 1Gi
`))
			assert.Equal(t, len(recorder.Events), 0)
			assert.Equal(t, len(*logs), 0)

			// clear status for other tests
			cluster.Status = v1beta1.PostgresClusterStatus{}
		})

		t.Run("LimitNoStatus", func(t *testing.T) {
			recorder := events.NewRecorder(t, runtime.Scheme)
			reconciler := &Reconciler{Recorder: recorder}
			ctx, logs := setupLogCapture(ctx)

			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: naming.InstancePostgresDataVolume(instance)}
			spec := instanceSetSpec("1Gi", "2Gi")
			pvc.Spec = spec.DataVolumeClaimSpec.AsPersistentVolumeClaimSpec()

			reconciler.setVolumeSize(ctx, &cluster, pvc, "pgData", spec.Name)

			assert.Assert(t, cmp.MarshalMatches(pvc.Spec, `
accessModes:
- ReadWriteOnce
resources:
  limits:
    storage: 2Gi
  requests:
    storage: 1Gi
`))
			assert.Equal(t, len(recorder.Events), 0)
			assert.Equal(t, len(*logs), 0)
		})

		t.Run("BadStatusWithLimit", func(t *testing.T) {
			recorder := events.NewRecorder(t, runtime.Scheme)
			reconciler := &Reconciler{Recorder: recorder}
			ctx, logs := setupLogCapture(ctx)

			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: naming.InstancePostgresDataVolume(instance)}
			spec := instanceSetSpec("1Gi", "3Gi")
			cluster.Status = desiredStatus("NotAValidValue")
			pvc.Spec = spec.DataVolumeClaimSpec.AsPersistentVolumeClaimSpec()

			reconciler.setVolumeSize(ctx, &cluster, pvc, "pgData", spec.Name)

			assert.Assert(t, cmp.MarshalMatches(pvc.Spec, `
accessModes:
- ReadWriteOnce
resources:
  limits:
    storage: 3Gi
  requests:
    storage: 1Gi
`))

			assert.Equal(t, len(recorder.Events), 0)
			assert.Equal(t, len(*logs), 1)
			assert.Assert(t, cmp.Contains((*logs)[0],
				"For elephant/some-instance: Unable to parse pgData volume request: NotAValidValue"))
		})

		t.Run("StatusWithLimit", func(t *testing.T) {
			recorder := events.NewRecorder(t, runtime.Scheme)
			reconciler := &Reconciler{Recorder: recorder}
			ctx, logs := setupLogCapture(ctx)

			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: naming.InstancePostgresDataVolume(instance)}
			spec := instanceSetSpec("1Gi", "3Gi")
			cluster.Status = desiredStatus("2Gi")
			pvc.Spec = spec.DataVolumeClaimSpec.AsPersistentVolumeClaimSpec()

			reconciler.setVolumeSize(ctx, &cluster, pvc, "pgData", spec.Name)

			assert.Assert(t, cmp.MarshalMatches(pvc.Spec, `
accessModes:
- ReadWriteOnce
resources:
  limits:
    storage: 3Gi
  requests:
    storage: 2Gi
`))
			assert.Equal(t, len(recorder.Events), 0)
			assert.Equal(t, len(*logs), 0)
		})

		t.Run("StatusWithLimitGrowToLimit", func(t *testing.T) {
			recorder := events.NewRecorder(t, runtime.Scheme)
			reconciler := &Reconciler{Recorder: recorder}
			ctx, logs := setupLogCapture(ctx)

			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: naming.InstancePostgresDataVolume(instance)}
			spec := instanceSetSpec("1Gi", "2Gi")
			cluster.Status = desiredStatus("2Gi")
			pvc.Spec = spec.DataVolumeClaimSpec.AsPersistentVolumeClaimSpec()

			reconciler.setVolumeSize(ctx, &cluster, pvc, "pgData", spec.Name)

			assert.Assert(t, cmp.MarshalMatches(pvc.Spec, `
accessModes:
- ReadWriteOnce
resources:
  limits:
    storage: 2Gi
  requests:
    storage: 2Gi
`))

			assert.Equal(t, len(*logs), 0)
			assert.Equal(t, len(recorder.Events), 1)
			assert.Equal(t, recorder.Events[0].Regarding.Name, cluster.Name)
			assert.Equal(t, recorder.Events[0].Reason, "VolumeLimitReached")
			assert.Equal(t, recorder.Events[0].Note, "pgData volume(s) for elephant/some-instance are at size limit (2Gi).")
		})

		t.Run("DesiredStatusOverLimit", func(t *testing.T) {
			recorder := events.NewRecorder(t, runtime.Scheme)
			reconciler := &Reconciler{Recorder: recorder}
			ctx, logs := setupLogCapture(ctx)

			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: naming.InstancePostgresDataVolume(instance)}
			spec := instanceSetSpec("4Gi", "5Gi")
			cluster.Status = desiredStatus("10Gi")
			pvc.Spec = spec.DataVolumeClaimSpec.AsPersistentVolumeClaimSpec()

			reconciler.setVolumeSize(ctx, &cluster, pvc, "pgData", spec.Name)

			assert.Assert(t, cmp.MarshalMatches(pvc.Spec, `
accessModes:
- ReadWriteOnce
resources:
  limits:
    storage: 5Gi
  requests:
    storage: 5Gi
`))

			assert.Equal(t, len(*logs), 0)
			assert.Equal(t, len(recorder.Events), 2)
			var found1, found2 bool
			for _, event := range recorder.Events {
				if event.Reason == "VolumeLimitReached" {
					found1 = true
					assert.Equal(t, event.Regarding.Name, cluster.Name)
					assert.Equal(t, event.Note, "pgData volume(s) for elephant/some-instance are at size limit (5Gi).")
				}
				if event.Reason == "DesiredVolumeAboveLimit" {
					found2 = true
					assert.Equal(t, event.Regarding.Name, cluster.Name)
					assert.Equal(t, event.Note,
						"The desired size (10Gi) for the elephant/some-instance pgData volume(s) is greater than the size limit (5Gi).")
				}
			}
			assert.Assert(t, found1 && found2)
		})

	})
}

func TestDetermineDesiredVolumeRequest(t *testing.T) {
	t.Parallel()

	cluster := v1beta1.PostgresCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "elephant",
			Namespace: "test-namespace",
		},
		Spec: v1beta1.PostgresClusterSpec{
			InstanceSets: []v1beta1.PostgresInstanceSetSpec{{
				Name:     "some-instance",
				Replicas: initialize.Int32(1),
			}},
		},
	}

	pgDataStatus := func(request string) v1beta1.PostgresClusterStatus {
		desiredMap := make(map[string]string)
		desiredMap["elephant-some-instance-wxyz-0"] = request
		return v1beta1.PostgresClusterStatus{
			InstanceSets: []v1beta1.PostgresInstanceSetStatus{{
				Name:                "some-instance",
				DesiredPGDataVolume: desiredMap,
			}}}
	}

	testCases := []struct {
		tcName         string
		sizeFromStatus string
		pvcRequestSize string
		volType        string
		instanceName   string
		expected       string
	}{{
		tcName:         "Larger size requested",
		sizeFromStatus: "3Gi",
		pvcRequestSize: "2Gi",
		volType:        "pgData",
		instanceName:   "some-instance",
		expected:       "3Gi",
	}, {
		tcName:         "PVC is desired size",
		sizeFromStatus: "2Gi",
		pvcRequestSize: "2Gi",
		volType:        "pgData",
		instanceName:   "some-instance",
		expected:       "2Gi",
	}, {
		tcName:         "Original larger than status request",
		sizeFromStatus: "1Gi",
		pvcRequestSize: "2Gi",
		volType:        "pgData",
		instanceName:   "some-instance",
		expected:       "2Gi",
	}, {
		tcName:         "Instance doesn't exist",
		sizeFromStatus: "2Gi",
		pvcRequestSize: "1Gi",
		volType:        "pgData",
		instanceName:   "not-an-instance",
		expected:       "1Gi",
	}, {
		tcName:         "Bad Value",
		sizeFromStatus: "batman",
		pvcRequestSize: "1Gi",
		volType:        "pgData",
		instanceName:   "some-instance",
		expected:       "1Gi",
	}}

	for _, tc := range testCases {
		t.Run(tc.tcName, func(t *testing.T) {

			cluster.Status = pgDataStatus(tc.sizeFromStatus)
			request, err := resource.ParseQuantity(tc.pvcRequestSize)
			assert.NilError(t, err)

			dpv, err := getDesiredVolumeSize(&cluster, tc.volType, tc.instanceName, &request)
			assert.Equal(t, request.String(), tc.expected)

			if tc.tcName != "Bad Value" {
				assert.NilError(t, err)
				assert.Assert(t, dpv == "")
			} else {
				assert.ErrorContains(t, err, "quantities must match the regular expression")
				assert.Assert(t, dpv == "batman")
			}
		})
	}

}
