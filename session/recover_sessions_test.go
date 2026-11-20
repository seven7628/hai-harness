package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestRecoverAllSessions 把磁盘上所有幸存的 <sid>.record.json + .blocks 还原成可读 JSONL。
// 用法：go test ./session/ -run TestRecoverAllSessions -v -args -root <sessions根> -out <输出目录>
func TestRecoverAllSessions(t *testing.T) {
	root := os.Getenv("RECOVER_ROOT")
	out := os.Getenv("RECOVER_OUT")
	if root == "" || out == "" {
		t.Skip("设 RECOVER_ROOT / RECOVER_OUT 环境变量后运行")
	}
	wsDirs, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, ws := range wsDirs {
		if !ws.IsDir() {
			continue
		}
		wsDir := filepath.Join(root, ws.Name())
		files, err := os.ReadDir(wsDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			name := f.Name()
			if !strings.HasSuffix(name, ".record.json") {
				continue
			}
			store := filepath.Join(wsDir, name+".blocks")
			if _, err := os.Stat(store); err != nil {
				continue
			}
			n, err := dumpStore(store, filepath.Join(out, ws.Name(), strings.TrimSuffix(name, ".record.json")+".jsonl"))
			if err != nil {
				t.Errorf("dump %s/%s: %v", ws.Name(), name, err)
				continue
			}
			t.Logf("✅ %s/%s → %d 条", ws.Name(), name, n)
			total += n
		}
	}
	t.Logf("共导出 %d 条记录", total)
}

func dumpStore(storePath, dst string) (int, error) {
	bs, err := openBlockStore(storePath)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	f, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	n := 0
	for _, idNum := range bs.sortedIDs {
		id := strconv.FormatUint(idNum, 10)
		raw, ok := bs.read(id)
		if !ok {
			continue
		}
		var probe map[string]any
		if err := json.Unmarshal(raw, &probe); err != nil {
			continue
		}
		w.Write(raw)
		w.WriteByte('\n')
		n++
	}
	return n, nil
}
