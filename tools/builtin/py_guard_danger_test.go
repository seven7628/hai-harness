package builtin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPyGuard(t *testing.T) {
	blocked := []string{
		// 事故形态：具体资产路径
		`import shutil; shutil.rmtree("/Users/admin/workspace/go-code")`,
		`import shutil; shutil.rmtree("~/Documents")`,
		`import shutil; shutil.rmtree("$HOME/workspace/go-code")`,
		`import shutil, os; shutil.rmtree(os.path.expanduser("~/Desktop"))`,
		`import shutil; shutil.rmtree(os.path.join(os.environ["HOME"], "workspace"))`,
		// 家目录本身（原有能力）
		`import shutil, pathlib; shutil.rmtree(pathlib.Path.home())`,
		`import shutil, os; shutil.rmtree(os.getcwd())`,
		// shell 旁路
		`import os; os.system("rm -rf /Users/admin/workspace/go-code")`,
	}
	for _, s := range blocked {
		if r := checkPythonHighRisk(s); r == "" {
			t.Errorf("❌ 应拦截但放行: %s", s)
		} else {
			t.Logf("✅ 拦截: %s\n     → %s", s, r)
		}
	}

	allowed := []string{
		// 正常用法：临时目录 / 工作区内的可重建产物 / 缓存
		`import shutil, tempfile, os; shutil.rmtree(os.path.join(tempfile.gettempdir(), "x"))`,
		`import shutil; shutil.rmtree("/tmp/build-cache")`,
		`import shutil; shutil.rmtree("~/.cache/pip")`,
		`import shutil; shutil.rmtree("./dist")`,
		`import os; os.system("ls -la")`,
		`print("hello")`,
	}
	for _, s := range allowed {
		if r := checkPythonHighRisk(s); r != "" {
			t.Errorf("❌ 应放行但拦截: %s → %s", s, r)
		} else {
			t.Logf("✅ 放行: %s", s)
		}
	}
}

func TestPyGuardRepoRoot(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "myrepo")
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	s := `import shutil; shutil.rmtree("` + repo + `")`
	if r := checkPythonHighRisk(s); r == "" {
		t.Errorf("❌ 仓库根应被拦截: %s", repo)
	} else {
		t.Logf("✅ 仓库根被拦截: %s", r)
	}
}
