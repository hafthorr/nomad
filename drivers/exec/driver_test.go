package exec

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	ctestutils "github.com/hashicorp/nomad/client/testutil"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/helper/testtask"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
	dtestutil "github.com/hashicorp/nomad/plugins/drivers/testutils"
	"github.com/hashicorp/nomad/testutil"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	if !testtask.Run() {
		os.Exit(m.Run())
	}
}

var testResources = &drivers.Resources{
	NomadResources: &structs.AllocatedTaskResources{
		Memory: structs.AllocatedMemoryResources{
			MemoryMB: 128,
		},
		Cpu: structs.AllocatedCpuResources{
			CpuShares: 100,
		},
	},
	LinuxResources: &drivers.LinuxResources{
		MemoryLimitBytes: 134217728,
		CPUShares:        100,
	},
}

func TestExecDriver_Fingerprint_NonLinux(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	require := require.New(t)
	if runtime.GOOS == "linux" {
		t.Skip("Test only available not on Linux")
	}

	d := NewExecDriver(testlog.HCLogger(t))
	harness := dtestutil.NewDriverHarness(t, d)

	fingerCh, err := harness.Fingerprint(context.Background())
	require.NoError(err)
	select {
	case finger := <-fingerCh:
		require.Equal(drivers.HealthStateUndetected, finger.Health)
	case <-time.After(time.Duration(testutil.TestMultiplier()*5) * time.Second):
		require.Fail("timeout receiving fingerprint")
	}
}

func TestExecDriver_Fingerprint(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	ctestutils.ExecCompatible(t)

	d := NewExecDriver(testlog.HCLogger(t))
	harness := dtestutil.NewDriverHarness(t, d)

	fingerCh, err := harness.Fingerprint(context.Background())
	require.NoError(err)
	select {
	case finger := <-fingerCh:
		require.Equal(drivers.HealthStateHealthy, finger.Health)
		require.True(finger.Attributes["driver.exec"].GetBool())
	case <-time.After(time.Duration(testutil.TestMultiplier()*5) * time.Second):
		require.Fail("timeout receiving fingerprint")
	}
}

func TestExecDriver_StartWait(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	ctestutils.ExecCompatible(t)

	d := NewExecDriver(testlog.HCLogger(t))
	harness := dtestutil.NewDriverHarness(t, d)
	task := &drivers.TaskConfig{
		ID:        uuid.Generate(),
		Name:      "test",
		Resources: testResources,
	}

	tc := &TaskConfig{
		Command: "cat",
		Args:    []string{"/proc/self/cgroup"},
	}
	require.NoError(task.EncodeConcreteDriverConfig(&tc))

	cleanup := harness.MkAllocDir(task, false)
	defer cleanup()

	handle, _, err := harness.StartTask(task)
	require.NoError(err)

	ch, err := harness.WaitTask(context.Background(), handle.Config.ID)
	require.NoError(err)
	result := <-ch
	require.Zero(result.ExitCode)
	require.NoError(harness.DestroyTask(task.ID, true))
}

func TestExecDriver_StartWaitStopKill(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	ctestutils.ExecCompatible(t)

	d := NewExecDriver(testlog.HCLogger(t))
	harness := dtestutil.NewDriverHarness(t, d)
	task := &drivers.TaskConfig{
		ID:        uuid.Generate(),
		Name:      "test",
		Resources: testResources,
	}

	tc := &TaskConfig{
		Command: "/bin/bash",
		Args:    []string{"-c", "echo hi; sleep 600"},
	}
	require.NoError(task.EncodeConcreteDriverConfig(&tc))

	cleanup := harness.MkAllocDir(task, false)
	defer cleanup()

	handle, _, err := harness.StartTask(task)
	require.NoError(err)
	defer harness.DestroyTask(task.ID, true)

	ch, err := harness.WaitTask(context.Background(), handle.Config.ID)
	require.NoError(err)

	require.NoError(harness.WaitUntilStarted(task.ID, 1*time.Second))

	go func() {
		harness.StopTask(task.ID, 2*time.Second, "SIGINT")
	}()

	select {
	case result := <-ch:
		require.False(result.Successful())
	case <-time.After(10 * time.Second):
		require.Fail("timeout waiting for task to shutdown")
	}

	// Ensure that the task is marked as dead, but account
	// for WaitTask() closing channel before internal state is updated
	testutil.WaitForResult(func() (bool, error) {
		status, err := harness.InspectTask(task.ID)
		if err != nil {
			return false, fmt.Errorf("inspecting task failed: %v", err)
		}
		if status.State != drivers.TaskStateExited {
			return false, fmt.Errorf("task hasn't exited yet; status: %v", status.State)
		}

		return true, nil
	}, func(err error) {
		require.NoError(err)
	})

	require.NoError(harness.DestroyTask(task.ID, true))
}

func TestExecDriver_StartWaitRecover(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	ctestutils.ExecCompatible(t)

	d := NewExecDriver(testlog.HCLogger(t))
	harness := dtestutil.NewDriverHarness(t, d)
	task := &drivers.TaskConfig{
		ID:        uuid.Generate(),
		Name:      "test",
		Resources: testResources,
	}

	tc := &TaskConfig{
		Command: "/bin/sleep",
		Args:    []string{"5"},
	}
	require.NoError(task.EncodeConcreteDriverConfig(&tc))

	cleanup := harness.MkAllocDir(task, false)
	defer cleanup()

	handle, _, err := harness.StartTask(task)
	require.NoError(err)

	ctx, cancel := context.WithCancel(context.Background())

	ch, err := harness.WaitTask(ctx, handle.Config.ID)
	require.NoError(err)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		result := <-ch
		require.Error(result.Err)
	}()

	require.NoError(harness.WaitUntilStarted(task.ID, 1*time.Second))
	cancel()

	waitCh := make(chan struct{})
	go func() {
		defer close(waitCh)
		wg.Wait()
	}()

	select {
	case <-waitCh:
		status, err := harness.InspectTask(task.ID)
		require.NoError(err)
		require.Equal(drivers.TaskStateRunning, status.State)
	case <-time.After(1 * time.Second):
		require.Fail("timeout waiting for task wait to cancel")
	}

	// Loose task
	d.(*Driver).tasks.Delete(task.ID)
	_, err = harness.InspectTask(task.ID)
	require.Error(err)

	require.NoError(harness.RecoverTask(handle))
	status, err := harness.InspectTask(task.ID)
	require.NoError(err)
	require.Equal(drivers.TaskStateRunning, status.State)

	require.NoError(harness.StopTask(task.ID, 0, ""))
	require.NoError(harness.DestroyTask(task.ID, true))
}

func TestExecDriver_Stats(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	ctestutils.ExecCompatible(t)

	d := NewExecDriver(testlog.HCLogger(t))
	harness := dtestutil.NewDriverHarness(t, d)
	task := &drivers.TaskConfig{
		ID:        uuid.Generate(),
		Name:      "test",
		Resources: testResources,
	}

	tc := &TaskConfig{
		Command: "/bin/sleep",
		Args:    []string{"5"},
	}
	require.NoError(task.EncodeConcreteDriverConfig(&tc))

	cleanup := harness.MkAllocDir(task, false)
	defer cleanup()

	handle, _, err := harness.StartTask(task)
	require.NoError(err)
	require.NotNil(handle)

	require.NoError(harness.WaitUntilStarted(task.ID, 1*time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	statsCh, err := harness.TaskStats(ctx, task.ID, time.Second*10)
	require.NoError(err)
	select {
	case stats := <-statsCh:
		require.NotZero(stats.ResourceUsage.MemoryStats.RSS)
		require.NotZero(stats.Timestamp)
		require.WithinDuration(time.Now(), time.Unix(0, stats.Timestamp), time.Second)
	case <-time.After(time.Second):
		require.Fail("timeout receiving from channel")
	}

	require.NoError(harness.DestroyTask(task.ID, true))
}

func TestExecDriver_Start_Wait_AllocDir(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	ctestutils.ExecCompatible(t)

	d := NewExecDriver(testlog.HCLogger(t))
	harness := dtestutil.NewDriverHarness(t, d)
	task := &drivers.TaskConfig{
		ID:        uuid.Generate(),
		Name:      "sleep",
		Resources: testResources,
	}
	cleanup := harness.MkAllocDir(task, false)
	defer cleanup()

	exp := []byte{'w', 'i', 'n'}
	file := "output.txt"
	tc := &TaskConfig{
		Command: "/bin/bash",
		Args: []string{
			"-c",
			fmt.Sprintf(`sleep 1; echo -n %s > /alloc/%s`, string(exp), file),
		},
	}
	require.NoError(task.EncodeConcreteDriverConfig(&tc))

	handle, _, err := harness.StartTask(task)
	require.NoError(err)
	require.NotNil(handle)

	// Task should terminate quickly
	waitCh, err := harness.WaitTask(context.Background(), task.ID)
	require.NoError(err)
	select {
	case res := <-waitCh:
		require.True(res.Successful(), "task should have exited successfully: %v", res)
	case <-time.After(time.Duration(testutil.TestMultiplier()*5) * time.Second):
		require.Fail("timeout waiting for task")
	}

	// Check that data was written to the shared alloc directory.
	outputFile := filepath.Join(task.TaskDir().SharedAllocDir, file)
	act, err := ioutil.ReadFile(outputFile)
	require.NoError(err)
	require.Exactly(exp, act)

	require.NoError(harness.DestroyTask(task.ID, true))
}

func TestExecDriver_User(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	ctestutils.ExecCompatible(t)

	d := NewExecDriver(testlog.HCLogger(t))
	harness := dtestutil.NewDriverHarness(t, d)
	task := &drivers.TaskConfig{
		ID:        uuid.Generate(),
		Name:      "sleep",
		User:      "alice",
		Resources: testResources,
	}
	cleanup := harness.MkAllocDir(task, false)
	defer cleanup()

	tc := &TaskConfig{
		Command: "/bin/sleep",
		Args:    []string{"100"},
	}
	require.NoError(task.EncodeConcreteDriverConfig(&tc))

	handle, _, err := harness.StartTask(task)
	require.Error(err)
	require.Nil(handle)

	msg := "user alice"
	if !strings.Contains(err.Error(), msg) {
		t.Fatalf("Expecting '%v' in '%v'", msg, err)
	}
}

// TestExecDriver_HandlerExec ensures the exec driver's handle properly
// executes commands inside the container.
func TestExecDriver_HandlerExec(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	ctestutils.ExecCompatible(t)

	d := NewExecDriver(testlog.HCLogger(t))
	harness := dtestutil.NewDriverHarness(t, d)
	task := &drivers.TaskConfig{
		ID:        uuid.Generate(),
		Name:      "sleep",
		Resources: testResources,
	}
	cleanup := harness.MkAllocDir(task, false)
	defer cleanup()

	tc := &TaskConfig{
		Command: "/bin/sleep",
		Args:    []string{"9000"},
	}
	require.NoError(task.EncodeConcreteDriverConfig(&tc))

	handle, _, err := harness.StartTask(task)
	require.NoError(err)
	require.NotNil(handle)

	// Exec a command that should work and dump the environment
	// TODO: enable section when exec env is fully loaded
	/*res, err := harness.ExecTask(task.ID, []string{"/bin/sh", "-c", "env | grep ^NOMAD"}, time.Second)
	require.NoError(err)
	require.True(res.ExitResult.Successful())

	// Assert exec'd commands are run in a task-like environment
	scriptEnv := make(map[string]string)
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(string(line), "=", 2)
		if len(parts) != 2 {
			t.Fatalf("Invalid env var: %q", line)
		}
		scriptEnv[parts[0]] = parts[1]
	}
	if v, ok := scriptEnv["NOMAD_SECRETS_DIR"]; !ok || v != "/secrets" {
		t.Errorf("Expected NOMAD_SECRETS_DIR=/secrets but found=%t value=%q", ok, v)
	}*/

	// Assert cgroup membership
	res, err := harness.ExecTask(task.ID, []string{"/bin/cat", "/proc/self/cgroup"}, time.Second)
	require.NoError(err)
	require.True(res.ExitResult.Successful())
	found := false
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		// Every cgroup entry should be /nomad/$ALLOC_ID
		if line == "" {
			continue
		}
		// Skip systemd and rdma cgroups; rdma was added in most recent kernels and libcontainer/docker
		// don't isolate them by default.
		if strings.HasPrefix(line, "1:name=systemd") || strings.Contains(line, ":rdma:") {
			continue
		}
		if !strings.Contains(line, ":/nomad/") {
			t.Errorf("Not a member of the alloc's cgroup: expected=...:/nomad/... -- found=%q", line)
			continue
		}
		found = true
	}
	require.True(found, "exec'd command isn't in the task's cgroup")

	// Exec a command that should fail
	res, err = harness.ExecTask(task.ID, []string{"/usr/bin/stat", "lkjhdsaflkjshowaisxmcvnlia"}, time.Second)
	require.NoError(err)
	require.False(res.ExitResult.Successful())
	if expected := "No such file or directory"; !bytes.Contains(res.Stdout, []byte(expected)) {
		t.Fatalf("expected output to contain %q but found: %q", expected, res.Stdout)
	}

	require.NoError(harness.DestroyTask(task.ID, true))
}

func TestExecDriver_DevicesAndMounts(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	ctestutils.ExecCompatible(t)

	tmpDir, err := ioutil.TempDir("", "exec_binds_mounts")
	require.NoError(err)
	defer os.RemoveAll(tmpDir)

	err = ioutil.WriteFile(filepath.Join(tmpDir, "testfile"), []byte("from-host"), 600)
	require.NoError(err)

	d := NewExecDriver(testlog.HCLogger(t))
	harness := dtestutil.NewDriverHarness(t, d)
	task := &drivers.TaskConfig{
		ID:         uuid.Generate(),
		Name:       "test",
		Resources:  testResources,
		StdoutPath: filepath.Join(tmpDir, "task-stdout"),
		StderrPath: filepath.Join(tmpDir, "task-stderr"),
		Devices: []*drivers.DeviceConfig{
			{
				TaskPath:    "/dev/inserted-random",
				HostPath:    "/dev/random",
				Permissions: "rw",
			},
		},
		Mounts: []*drivers.MountConfig{
			{
				TaskPath: "/tmp/task-path-rw",
				HostPath: tmpDir,
				Readonly: false,
			},
			{
				TaskPath: "/tmp/task-path-ro",
				HostPath: tmpDir,
				Readonly: true,
			},
		},
	}

	require.NoError(ioutil.WriteFile(task.StdoutPath, []byte{}, 660))
	require.NoError(ioutil.WriteFile(task.StderrPath, []byte{}, 660))

	tc := &TaskConfig{
		Command: "/bin/bash",
		Args: []string{"-c", `
export LANG=en.UTF-8
echo "mounted device /inserted-random: $(stat -c '%t:%T' /dev/inserted-random)"
echo "reading from ro path: $(cat /tmp/task-path-ro/testfile)"
echo "reading from rw path: $(cat /tmp/task-path-rw/testfile)"
touch /tmp/task-path-rw/testfile && echo 'overwriting file in rw succeeded'
touch /tmp/task-path-rw/testfile-from-rw && echo from-exec >  /tmp/task-path-rw/testfile-from-rw && echo 'writing new file in rw succeeded'
touch /tmp/task-path-ro/testfile && echo 'overwriting file in ro succeeded'
touch /tmp/task-path-ro/testfile-from-ro && echo from-exec >  /tmp/task-path-ro/testfile-from-ro && echo 'writing new file in ro succeeded'
exit 0
`},
	}
	require.NoError(task.EncodeConcreteDriverConfig(&tc))

	cleanup := harness.MkAllocDir(task, false)
	defer cleanup()

	handle, _, err := harness.StartTask(task)
	require.NoError(err)

	ch, err := harness.WaitTask(context.Background(), handle.Config.ID)
	require.NoError(err)
	result := <-ch
	require.NoError(harness.DestroyTask(task.ID, true))

	stdout, err := ioutil.ReadFile(task.StdoutPath)
	require.NoError(err)
	require.Equal(`mounted device /inserted-random: 1:8
reading from ro path: from-host
reading from rw path: from-host
overwriting file in rw succeeded
writing new file in rw succeeded`, strings.TrimSpace(string(stdout)))

	stderr, err := ioutil.ReadFile(task.StderrPath)
	require.NoError(err)
	require.Equal(`touch: cannot touch '/tmp/task-path-ro/testfile': Read-only file system
touch: cannot touch '/tmp/task-path-ro/testfile-from-ro': Read-only file system`, strings.TrimSpace(string(stderr)))

	// testing exit code last so we can inspect output first
	require.Zero(result.ExitCode)

	fromRWContent, err := ioutil.ReadFile(filepath.Join(tmpDir, "testfile-from-rw"))
	require.NoError(err)
	require.Equal("from-exec", strings.TrimSpace(string(fromRWContent)))
}
