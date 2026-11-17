package modeldeploy

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 权重拉取镜像冒烟(deploy/images/model-pull, .github/workflows/fleet-images.yml): 用生产的命令模板
// (写校验清单 → 拉取容器 modelscope download 固定 commit → 逐文件 sha256 → 完成标记 → 原子改名) 在本机 Docker
// 真实拉取一个 35MB 的 ModelScope 公开模型。SMOKE_PULL_IMAGE = 拉取镜像 ref@sha256; 需要访问 modelscope.cn。
func TestModelPullImageSmoke(t *testing.T) {
	img := os.Getenv("SMOKE_PULL_IMAGE")
	if img == "" {
		t.Skip("set SMOKE_PULL_IMAGE to run the model pull image smoke test")
	}
	files := []ManifestFile{ // iic/nlp_corom_sentence-embedding_chinese-tiny @ f339ead3(ModelScope repo/files 接口的 Sha256)
		{Path: ".gitattributes", Size: 1414, SHA256: "f3d04437c926962476701a7939d22f21400b4eb0bdd31a15f0012576f2d00932"},
		{Path: "README.md", Size: 11869, SHA256: "e92c2fa20dd149a23453bbca8f983bc7ca69ccf858e3a2f5446c35c28af88a97"},
		{Path: "config.json", Size: 526, SHA256: "900016217cf9e2ffa2214fd6d1babae0b2e7e50444c792e66ca8a50f3ea1fdda"},
		{Path: "configuration.json", Size: 2886, SHA256: "134939e8722f60b30bcb15a05899c982cb0619d5eadfb8836011a9ce62f6797f"},
		{Path: "pytorch_model.bin", Size: 34830355, SHA256: "9664fd32c03dfae15de5b9dff8ea5fba92487a21cb33b4f57fb60b1f064c4eb0"},
		{Path: "requirements.txt", Size: 45, SHA256: "2ddb3d5fa4491265df10726006250b4a3fee3058925fcb15781bcc828f921489"},
		{Path: "resources/dual-encoder.png", Size: 62195, SHA256: "d2503b7ce242cfc2dd544b6e7c6d9b2532fc8e8c8c046f75c21184e1dae6cccb"},
		{Path: "special_tokens_map.json", Size: 113, SHA256: "88bbdf754dd64c44fff9e61b2c7d4380ded1bdf5c6d386be827ee28d79596cb9"},
		{Path: "tokenizer_config.json", Size: 63, SHA256: "195da2ce0682b35c16d3ebc85416255715d9c69c032bd4e08d28e5b77d6cfc96"},
		{Path: "vocab.txt", Size: 69998, SHA256: "fc4b55153cc8633a75d9142e721f05e26c833b186b356b1b9251f3f789ed75e4"},
	}
	manifest, err := json.Marshal(files)
	require.NoError(t, err)
	e := &ModelEntry{ID: 9001, ModelKey: "smoke-corom-tiny", Repo: "iic/nlp_corom_sentence-embedding_chinese-tiny",
		Revision: "f339ead3be6324443f44a41c56f071d894cc4430", Manifest: manifest}

	cache, err := os.MkdirTemp("", "omnis-pull-smoke-")
	require.NoError(t, err)
	cache, err = filepath.EvalSymlinks(cache)
	require.NoError(t, err)
	sh := func(cmd string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, "sh", "-c", cmd).CombinedOutput()
		require.NoError(t, err, string(out))
		return string(out)
	}
	t.Cleanup(func() { // 拉取容器以 root 写入, 由容器删除
		_ = exec.Command("sh", "-c", "docker rm -f "+pullContainerName(e.ID)+" >/dev/null 2>&1; docker run --rm -v "+cache+":"+cache+
			" --entrypoint rm "+img+" -rf "+cache+"/.omnis-pull "+cache+"/"+e.ModelKey+"@"+e.Revision+" "+cache+"/"+e.ModelKey+"@"+e.Revision+".partial").Run()
		_ = os.RemoveAll(cache)
	})

	checklist, sum, err := manifestChecklist(e)
	require.NoError(t, err)
	cmds, err := writeChecklistCommands(cache, e, checklist)
	require.NoError(t, err)
	var last string
	for _, c := range cmds {
		last = sh(c)
	}
	require.Equal(t, sum, strings.TrimSpace(last))

	run, err := pullRunCommand(cache, img, 1, e)
	require.NoError(t, err)
	assert.Contains(t, sh(run), "@@started")
	status, err := pullStatusCommand(cache, e)
	require.NoError(t, err)
	var st string
	require.Eventually(t, func() bool {
		st = sh(status)
		return strings.Contains(st, "@@pull exited")
	}, 10*time.Minute, 2*time.Second)
	logs := sh(pullFinishCommand(e.ID))
	require.Contains(t, st, "@@pull exited 0", logs)
	assert.Contains(t, logs, "@@done")

	dir, err := modelDir(cache, e)
	require.NoError(t, err)
	assert.Equal(t, sum, strings.TrimSpace(sh(markerCheckCommand(dir))), "completion marker must match the manifest checklist")
	assert.Contains(t, sh("ls "+dir+"/resources"), "dual-encoder.png")

	// 篡改清单中的一个摘要: sha256sum -c 必须让拉取失败, 不写完成标记、不改名。
	bad := append([]ManifestFile(nil), files...)
	bad[2].SHA256 = strings.Repeat("0", 64)
	badManifest, err := json.Marshal(bad)
	require.NoError(t, err)
	e2 := *e
	e2.ID, e2.ModelKey, e2.Manifest = 9002, "smoke-corom-bad", badManifest
	t.Cleanup(func() {
		_ = exec.Command("sh", "-c", "docker rm -f "+pullContainerName(e2.ID)+" >/dev/null 2>&1; docker run --rm -v "+cache+":"+cache+
			" --entrypoint rm "+img+" -rf "+cache+"/"+e2.ModelKey+"@"+e2.Revision+" "+cache+"/"+e2.ModelKey+"@"+e2.Revision+".partial").Run()
	})
	checklist2, _, err := manifestChecklist(&e2)
	require.NoError(t, err)
	cmds, err = writeChecklistCommands(cache, &e2, checklist2)
	require.NoError(t, err)
	for _, c := range cmds {
		sh(c)
	}
	run, err = pullRunCommand(cache, img, 1, &e2)
	require.NoError(t, err)
	sh(run)
	status, err = pullStatusCommand(cache, &e2)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		st = sh(status)
		return strings.Contains(st, "@@pull exited")
	}, 10*time.Minute, 2*time.Second)
	logs = sh(pullFinishCommand(e2.ID))
	assert.NotContains(t, st, "@@pull exited 0", logs)
	assert.NotContains(t, logs, "@@done")
	dir2, err := modelDir(cache, &e2)
	require.NoError(t, err)
	assert.Contains(t, sh(markerCheckCommand(dir2)), "@@nomarker")
}
