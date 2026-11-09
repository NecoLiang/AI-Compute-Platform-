package fleet

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// T-087/T-088 租约命令模板: 白名单、注入防护、语法与输出解析。

const testLeaseNo = "l-abcdefgh"

func TestLeaseCommandsRejectBadNames(t *testing.T) {
	for _, bad := range []string{"", "l-ABCDEFGH", "l-abc;reboot", "l-abcdefgh1", "x-abcdefgh"} {
		_, err := stopContainerCommand(bad)
		assert.Error(t, err, bad)
		_, err = killSessionsCommand(bad)
		assert.Error(t, err, bad)
		_, err = containerStateCommand(bad)
		assert.Error(t, err, bad)
		_, err = startContainerCommand(bad)
		assert.Error(t, err, bad)
		_, err = wallCommand(bad, 30)
		assert.Error(t, err, bad)
		_, err = miningScanCommand(testProbeImage, bad)
		assert.Error(t, err, bad)
	}
	for _, bad := range []string{"omnis-l-abcdefgh;rm -rf /", "omnis-x", "../omnis-l-abcdefgh", "omnis-l-abcdefgh-data2"} {
		_, err := purgeContainerCommand(bad)
		assert.Error(t, err, bad)
		_, err = purgeVolumeCommand(bad)
		assert.Error(t, err, bad)
		_, err = purgeNetworkCommand(bad)
		assert.Error(t, err, bad)
	}
	for _, bad := range []string{"ubuntu:latest", "omnis-commit/l-abcdefgh:1", "omnis-commit/l-abcdefgh:1700000000;x"} {
		_, err := removeCommitImageCommand(bad)
		assert.Error(t, err, bad)
	}
	_, err := commitContainerCommand(testLeaseNo, "omnis-commit/l-zzzzzzzz:1700000000")
	assert.Error(t, err, "提交镜像必须属于本租约")
	_, err = miningScanCommand("busybox:latest", testLeaseNo)
	assert.Error(t, err, "探针镜像必须 digest 固定")
	_, err = wallCommand(testLeaseNo, 0)
	assert.Error(t, err)
}

func TestLeaseCommandsSyntax(t *testing.T) {
	ref, err := commitRefOf(testLeaseNo, 1700000000)
	require.NoError(t, err)
	assert.Equal(t, "omnis-commit/l-abcdefgh:1700000000", ref)
	var cmds []string
	add := func(c string, err error) {
		require.NoError(t, err)
		cmds = append(cmds, c)
	}
	add(stopContainerCommand(testLeaseNo))
	add(containerStateCommand(testLeaseNo))
	add(killSessionsCommand(testLeaseNo))
	add(wallCommand(testLeaseNo, 30))
	add(purgeContainerCommand(containerNameOf(testLeaseNo)))
	add(purgeVolumeCommand(volumeNameOf(testLeaseNo)))
	add(purgeNetworkCommand(networkNameOf(testLeaseNo)))
	add(removeCommitImageCommand(ref))
	add(startContainerCommand(testLeaseNo))
	add(commitContainerCommand(testLeaseNo, ref))
	add(miningScanCommand(testProbeImage, testLeaseNo))
	cmds = append(cmds, cmdLeaseContainers, killSessionsScript)
	for _, c := range cmds {
		shSyntaxOK(t, c)
	}
	assert.NotContains(t, killSessionsScript, "'")
}

// fakeDocker 用状态目录模拟 docker inspect/stop/rm/volume/network/image, 实际执行命令模板验证分支与输出。
const fakeDocker = `#!/bin/sh
D="$FAKE_DOCKER_DIR"
echo "$*" >> "$D/log"
case "$1" in
  inspect)
    shift; [ "$1" = --type ] && shift 2
    fmt=""; [ "$1" = -f ] && { fmt=$2; shift 2; }
    [ -f "$D/c_$1" ] || exit 1
    case "$fmt" in
      *Id*) echo 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef ;;
      *DeviceRequests*) case "$fmt" in "{{.State.Running}}"*) printf '%s ' "$(cat "$D/c_$1")";; esac; echo "GPU-11111111-2222-3333-4444-555555555555,GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee," ;;
      *Running*) cat "$D/c_$1" ;;
    esac ;;
  stop) echo false > "$D/c_$4" ;;
  rm) rm -f "$D/c_$3" ;;
  volume|network|image)
    k=$1; op=$2; shift 2; [ "$1" = -f ] && shift
    case $op in inspect) [ -f "$D/${k}_$1" ] ;; rm|rmi) rm -f "$D/${k}_$1" ;; esac ;;
  rmi) rm -f "$D/image_$3" ;;
  *) exit 2 ;;
esac
`

func runWithFakeDocker(t *testing.T, dir, cmd string) (string, int) {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeDocker), 0o755))
	c := exec.Command("sh", "-c", cmd)
	c.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_DOCKER_DIR="+dir)
	out, err := c.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else {
		require.NoError(t, err)
	}
	return string(out), code
}

func TestStopAndPurgeCommandsAgainstFakeDocker(t *testing.T) {
	dir := t.TempDir()
	c, v, nw := containerNameOf(testLeaseNo), volumeNameOf(testLeaseNo), networkNameOf(testLeaseNo)

	stop, _ := stopContainerCommand(testLeaseNo)
	out, code := runWithFakeDocker(t, dir, stop)
	require.Equal(t, 0, code)
	exists, gpus, err := parseStopped(out)
	require.NoError(t, err)
	assert.False(t, exists)
	assert.Nil(t, gpus)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "c_"+c), []byte("true"), 0o644))
	state, _ := containerStateCommand(testLeaseNo)
	out, code = runWithFakeDocker(t, dir, state)
	require.Equal(t, 0, code)
	st, err := parseContainerState(out)
	require.NoError(t, err)
	assert.Equal(t, ContainerState{Exists: true, Running: true, GPUs: []string{
		"GPU-11111111-2222-3333-4444-555555555555", "GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}}, st)

	out, code = runWithFakeDocker(t, dir, stop)
	require.Equal(t, 0, code)
	exists, gpus, err = parseStopped(out)
	require.NoError(t, err)
	assert.True(t, exists)
	assert.Len(t, gpus, 2)
	raw, _ := os.ReadFile(filepath.Join(dir, "c_"+c))
	assert.Equal(t, "false\n", string(raw))

	ks, _ := killSessionsCommand(testLeaseNo)
	out, code = runWithFakeDocker(t, dir, ks)
	require.Equal(t, 0, code)
	assert.Equal(t, "@@notrunning\n", out, "已停止的容器没有会话可断")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "volume_"+v), nil, 0o644))
	pc, _ := purgeContainerCommand(c)
	out, code = runWithFakeDocker(t, dir, pc)
	require.Equal(t, 0, code)
	o, id := parsePurgeOut(out)
	assert.Equal(t, "removed", o)
	assert.Len(t, id, 64)
	out, _ = runWithFakeDocker(t, dir, pc)
	o, _ = parsePurgeOut(out)
	assert.Equal(t, "absent", o, "重复执行幂等")

	pv, _ := purgeVolumeCommand(v)
	out, _ = runWithFakeDocker(t, dir, pv)
	o, _ = parsePurgeOut(out)
	assert.Equal(t, "removed", o)
	pn, _ := purgeNetworkCommand(nw)
	out, _ = runWithFakeDocker(t, dir, pn)
	o, _ = parsePurgeOut(out)
	assert.Equal(t, "absent", o)
}

func TestParseLeaseOutputs(t *testing.T) {
	_, _, err := parseStopped("garbage")
	assert.Error(t, err)
	_, _, err = parseStopped("@@stopped GPU-x;reboot,")
	assert.Error(t, err, "卡标识须过白名单")
	_, err = parseContainerState("@@state maybe")
	assert.Error(t, err)
	o, id := parsePurgeOut("@@removed not-an-id")
	assert.Equal(t, "removed", o)
	assert.Empty(t, id)
	o, _ = parsePurgeOut("")
	assert.Equal(t, "error", o)

	m := parseLeaseContainers("omnis-l-abcdefgh running\nomnis-l-bbbbbbbb exited\nevil;name running\nomnis-l-cccccccc-data running\nomnis-l-dddddddd running x\n")
	assert.Equal(t, map[string]string{"omnis-l-abcdefgh": "running", "omnis-l-bbbbbbbb": "exited"}, m)
	assert.True(t, strings.HasPrefix(cmdLeaseContainers, "docker ps -a --filter label=omnis.lease"))
}
