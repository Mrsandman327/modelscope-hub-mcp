package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const downloadTestToken = "test-token-not-real"

// downloadTestZipEntry 描述测试压缩包中的一个条目。
type downloadTestZipEntry struct {
	name string
	body string
	mode os.FileMode
}

// downloadTestZip 在内存中构造测试压缩包，不访问网络，也不写入真实技能。
func downloadTestZip(t *testing.T, entries ...downloadTestZipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		header.SetMode(mode)
		writer, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatalf("创建测试压缩包条目 %q 失败：%v", entry.name, err)
		}
		if _, err := writer.Write([]byte(entry.body)); err != nil {
			t.Fatalf("写入测试压缩包条目 %q 失败：%v", entry.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭测试压缩包失败：%v", err)
	}
	return buf.Bytes()
}

// downloadTestServerClient 启动本地测试服务并返回使用该服务的 hubClient。
func downloadTestServerClient(t *testing.T, handler http.Handler, skillsDir string) *hubClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &hubClient{
		baseURL:   server.URL,
		apiKey:    downloadTestToken,
		skillsDir: skillsDir,
		http:      server.Client(),
	}
}

// downloadTestAuthHandler 统一检查请求方法、鉴权头和调用次数，避免真实网络请求。
func downloadTestAuthHandler(t *testing.T, calls *atomic.Int32, serve func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("期望 GET 请求，实际为 %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+downloadTestToken {
			t.Errorf("鉴权头不正确，实际为 %q", got)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("未授权"))
			return
		}
		serve(w, r)
	}
}

// downloadTestZipHandler 返回固定的测试压缩包，并校验请求路径。
func downloadTestZipHandler(t *testing.T, data []byte, wantPath string, calls *atomic.Int32) http.HandlerFunc {
	t.Helper()
	return downloadTestAuthHandler(t, calls, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wantPath {
			t.Errorf("请求路径不正确，期望 %q，实际 %q", wantPath, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(data)
	})
}

// downloadAssertNoTempDirs 确认技能根目录下没有残留本实现创建的临时目录。
func downloadAssertNoTempDirs(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录 %q 失败：%v", dir, err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), downloadTempPrefix) {
			t.Errorf("发现未清理的临时目录：%s", entry.Name())
		}
	}
}

// downloadAssertTargetAbsent 确认目标技能目录不存在。
func downloadAssertTargetAbsent(t *testing.T, skillsDir, name string) {
	t.Helper()
	target := filepath.Join(skillsDir, name)
	if _, err := os.Lstat(target); err == nil {
		t.Errorf("目标目录不应存在：%s", target)
	} else if !os.IsNotExist(err) {
		t.Errorf("检查目标目录失败：%v", err)
	}
}

// downloadAssertFileContent 校验解压后的文件内容。
func downloadAssertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取文件 %q 失败：%v", path, err)
	}
	if string(got) != want {
		t.Errorf("文件 %q 内容不正确，期望 %q，实际 %q", path, want, string(got))
	}
}

// 合法的单根目录技能包应被去掉顶层目录，保留嵌套资源，并返回完整结果。
func TestDownloadSkillSuccess(t *testing.T) {
	skillsDir := t.TempDir()
	entries := []downloadTestZipEntry{
		{name: "demo-skill-master-abc/", mode: os.ModeDir | 0o755},
		{name: "demo-skill-master-abc/SKILL.md", body: "# 示例技能\n"},
		{name: "demo-skill-master-abc/scripts/run.py", body: "print('ok')\n"},
		{name: "demo-skill-master-abc/references/guide.txt", body: "参考文档\n"},
		{name: "demo-skill-master-abc/references/nested/data.bin", body: "\x00\x01\x02"},
	}
	data := downloadTestZip(t, entries...)
	var calls atomic.Int32
	client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)

	result, err := client.downloadSkill(context.Background(), "demo/skill", "")
	if err != nil {
		t.Fatalf("下载失败：%v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("期望请求一次，实际 %d 次", calls.Load())
	}
	if result.ID != "demo/skill" {
		t.Errorf("结果 ID 不正确：%q", result.ID)
	}
	if result.Revision != downloadDefaultRevision {
		t.Errorf("默认版本应为 %q，实际 %q", downloadDefaultRevision, result.Revision)
	}
	if result.SkillDir != filepath.Join(skillsDir, "demo--skill") {
		t.Errorf("技能目录不正确：%q", result.SkillDir)
	}
	if result.Files != 4 {
		t.Errorf("文件数不正确，期望 4，实际 %d", result.Files)
	}
	wantBytes := int64(len("# 示例技能\n") + len("print('ok')\n") + len("参考文档\n") + len("\x00\x01\x02"))
	if result.Bytes != wantBytes {
		t.Errorf("字节数不正确，期望 %d，实际 %d", wantBytes, result.Bytes)
	}
	sum := sha256.Sum256(data)
	if result.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("压缩包 SHA256 不正确：%q", result.SHA256)
	}
	if !strings.HasSuffix(result.DownloadURL, "/skills/demo/skill/archive/zip/master") {
		t.Errorf("下载地址不正确：%q", result.DownloadURL)
	}

	downloadAssertFileContent(t, filepath.Join(result.SkillDir, "SKILL.md"), "# 示例技能\n")
	downloadAssertFileContent(t, filepath.Join(result.SkillDir, "scripts", "run.py"), "print('ok')\n")
	downloadAssertFileContent(t, filepath.Join(result.SkillDir, "references", "guide.txt"), "参考文档\n")
	downloadAssertFileContent(t, filepath.Join(result.SkillDir, "references", "nested", "data.bin"), "\x00\x01\x02")
	if _, err := os.Lstat(filepath.Join(result.SkillDir, "demo-skill-master-abc")); err == nil {
		t.Error("顶层目录没有被去掉")
	}
	downloadAssertNoTempDirs(t, skillsDir)
}

// 平铺结构（根目录直接有 SKILL.md）也应被接受。
func TestDownloadSkillFlatRoot(t *testing.T) {
	skillsDir := t.TempDir()
	data := downloadTestZip(t,
		downloadTestZipEntry{name: "SKILL.md", body: "# 平铺技能\n"},
		downloadTestZipEntry{name: "scripts/run.sh", body: "echo ok\n"},
	)
	var calls atomic.Int32
	client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)

	result, err := client.downloadSkill(context.Background(), "demo/skill", "")
	if err != nil {
		t.Fatalf("下载失败：%v", err)
	}
	downloadAssertFileContent(t, filepath.Join(result.SkillDir, "SKILL.md"), "# 平铺技能\n")
	downloadAssertFileContent(t, filepath.Join(result.SkillDir, "scripts", "run.sh"), "echo ok\n")
	downloadAssertNoTempDirs(t, skillsDir)
}

// 显式版本名应出现在请求路径和结果中。
func TestDownloadSkillExplicitRevision(t *testing.T) {
	skillsDir := t.TempDir()
	data := downloadTestZip(t,
		downloadTestZipEntry{name: "demo-skill-v1.2.3/SKILL.md", body: "# 版本技能\n"},
	)
	var calls atomic.Int32
	client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/v1.2.3", &calls), skillsDir)

	result, err := client.downloadSkill(context.Background(), "demo/skill", "v1.2.3")
	if err != nil {
		t.Fatalf("下载失败：%v", err)
	}
	if result.Revision != "v1.2.3" {
		t.Errorf("版本不正确：%q", result.Revision)
	}
	if !strings.HasSuffix(result.DownloadURL, "/skills/demo/skill/archive/zip/v1.2.3") {
		t.Errorf("下载地址不正确：%q", result.DownloadURL)
	}
}

// 目标目录已存在时必须在发起网络请求前拒绝，且不覆盖任何既有数据。
func TestDownloadSkillRefusesOverwrite(t *testing.T) {
	skillsDir := t.TempDir()
	target := filepath.Join(skillsDir, "demo--skill")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("创建既有目标失败：%v", err)
	}
	marker := filepath.Join(target, "SKILL.md")
	if err := os.WriteFile(marker, []byte("既有内容\n"), 0o644); err != nil {
		t.Fatalf("写入既有文件失败：%v", err)
	}

	data := downloadTestZip(t, downloadTestZipEntry{name: "demo-skill-master/SKILL.md", body: "新内容\n"})
	var calls atomic.Int32
	client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)

	_, err := client.downloadSkill(context.Background(), "demo/skill", "")
	if err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("期望目标已存在错误，实际：%v", err)
	}
	if calls.Load() != 0 {
		t.Errorf("目标已存在时不应发起请求，实际 %d 次", calls.Load())
	}
	downloadAssertFileContent(t, marker, "既有内容\n")
	downloadAssertNoTempDirs(t, skillsDir)
}

// 目标位置是普通文件时同样不能覆盖。
func TestDownloadSkillRefusesOverwriteFileTarget(t *testing.T) {
	skillsDir := t.TempDir()
	target := filepath.Join(skillsDir, "demo--skill")
	if err := os.WriteFile(target, []byte("既有文件\n"), 0o644); err != nil {
		t.Fatalf("创建既有文件失败：%v", err)
	}

	data := downloadTestZip(t, downloadTestZipEntry{name: "demo-skill-master/SKILL.md", body: "新内容\n"})
	var calls atomic.Int32
	client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)

	_, err := client.downloadSkill(context.Background(), "demo/skill", "")
	if err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("期望目标已存在错误，实际：%v", err)
	}
	if calls.Load() != 0 {
		t.Errorf("目标已存在时不应发起请求，实际 %d 次", calls.Load())
	}
	downloadAssertFileContent(t, target, "既有文件\n")
	downloadAssertNoTempDirs(t, skillsDir)
}

// 下载或解压失败后必须清理临时目录，且不能留下不完整的技能目录。
func TestDownloadSkillFailureCleanup(t *testing.T) {
	t.Run("服务错误", func(t *testing.T) {
		skillsDir := t.TempDir()
		var calls atomic.Int32
		handler := downloadTestAuthHandler(t, &calls, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("模拟服务错误"))
		})
		client := downloadTestServerClient(t, handler, skillsDir)

		_, err := client.downloadSkill(context.Background(), "demo/skill", "")
		if err == nil {
			t.Fatal("期望下载失败")
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("错误信息未包含 HTTP 状态：%v", err)
		}
		if strings.Contains(err.Error(), downloadTestToken) {
			t.Errorf("错误信息泄露了访问令牌：%v", err)
		}
		downloadAssertTargetAbsent(t, skillsDir, "demo--skill")
		downloadAssertNoTempDirs(t, skillsDir)
	})

	t.Run("损坏压缩包", func(t *testing.T) {
		skillsDir := t.TempDir()
		var calls atomic.Int32
		handler := downloadTestAuthHandler(t, &calls, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write([]byte("这不是一个压缩包"))
		})
		client := downloadTestServerClient(t, handler, skillsDir)

		_, err := client.downloadSkill(context.Background(), "demo/skill", "")
		if err == nil {
			t.Fatal("期望损坏压缩包被拒绝")
		}
		downloadAssertTargetAbsent(t, skillsDir, "demo--skill")
		downloadAssertNoTempDirs(t, skillsDir)
	})
}

// 各种路径穿越、绝对路径和反斜杠条目必须在写盘前被拒绝。
func TestDownloadSkillRejectsPathTraversal(t *testing.T) {
	cases := []struct {
		name  string
		entry string
	}{
		{name: "上级目录", entry: "demo-skill-master-abc/../escape.txt"},
		{name: "多级穿越", entry: "demo-skill-master-abc/nested/../../escape.txt"},
		{name: "反斜杠穿越", entry: `demo-skill-master-abc\..\escape.txt`},
		{name: "绝对路径", entry: "/escape.txt"},
		{name: "盘符路径", entry: "C:/escape.txt"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			skillsDir := t.TempDir()
			data := downloadTestZip(t,
				downloadTestZipEntry{name: "demo-skill-master-abc/SKILL.md", body: "# 技能\n"},
				downloadTestZipEntry{name: testCase.entry, body: "恶意内容"},
			)
			var calls atomic.Int32
			client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)

			_, err := client.downloadSkill(context.Background(), "demo/skill", "")
			if err == nil {
				t.Fatal("期望拒绝危险路径条目")
			}
			downloadAssertTargetAbsent(t, skillsDir, "demo--skill")
			downloadAssertNoTempDirs(t, skillsDir)
			for _, path := range []string{
				filepath.Join(skillsDir, "escape.txt"),
				filepath.Join(filepath.Dir(skillsDir), "escape.txt"),
			} {
				if _, statErr := os.Lstat(path); statErr == nil {
					t.Errorf("危险条目逃逸到了根目录之外：%s", path)
				}
			}
		})
	}
}

// 尾部点、尾部空格、控制字符、冒号、通配符和空路径段都应被拒绝。
func TestDownloadSkillRejectsUnsafeEntryNames(t *testing.T) {
	cases := []struct {
		name  string
		entry string
	}{
		{name: "尾点", entry: "demo-skill-master-abc/bad."},
		{name: "尾空格", entry: "demo-skill-master-abc/bad "},
		{name: "控制字符", entry: "demo-skill-master-abc/bad\x01name"},
		{name: "冒号", entry: "demo-skill-master-abc/bad:name"},
		{name: "问号", entry: "demo-skill-master-abc/bad?name"},
		{name: "空路径段", entry: "demo-skill-master-abc//bad"},
		{name: "单点路径段", entry: "demo-skill-master-abc/./bad"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			skillsDir := t.TempDir()
			data := downloadTestZip(t,
				downloadTestZipEntry{name: "demo-skill-master-abc/SKILL.md", body: "# 技能\n"},
				downloadTestZipEntry{name: testCase.entry, body: "内容"},
			)
			var calls atomic.Int32
			client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)

			_, err := client.downloadSkill(context.Background(), "demo/skill", "")
			if err == nil {
				t.Fatal("期望拒绝不安全条目名")
			}
			downloadAssertTargetAbsent(t, skillsDir, "demo--skill")
			downloadAssertNoTempDirs(t, skillsDir)
		})
	}
}

// Windows 保留设备名（包含带扩展名和不同大小写的形式）必须被拒绝。
func TestDownloadSkillRejectsWindowsReservedNames(t *testing.T) {
	names := []string{
		"CON", "con.txt", "PRN", "AUX.log", "NUL", "COM1", "COM9.txt", "LPT1", "lpt9.log",
		"COM¹", "com².txt", "COM³.log", "LPT¹", "lpt².txt", "LPT³.log",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			skillsDir := t.TempDir()
			data := downloadTestZip(t,
				downloadTestZipEntry{name: "demo-skill-master-abc/SKILL.md", body: "# 技能\n"},
				downloadTestZipEntry{name: "demo-skill-master-abc/scripts/" + name, body: "内容"},
			)
			var calls atomic.Int32
			client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)

			_, err := client.downloadSkill(context.Background(), "demo/skill", "")
			if err == nil {
				t.Fatalf("期望拒绝 Windows 保留名 %q", name)
			}
			downloadAssertTargetAbsent(t, skillsDir, "demo--skill")
			downloadAssertNoTempDirs(t, skillsDir)
		})
	}
}

// 符号链接、设备文件和命名管道等特殊条目必须被拒绝。
func TestDownloadSkillRejectsSymlinkAndSpecialFiles(t *testing.T) {
	cases := []struct {
		name string
		mode os.FileMode
		body string
	}{
		{name: "符号链接", mode: os.ModeSymlink | 0o777, body: "SKILL.md"},
		{name: "设备文件", mode: os.ModeDevice | 0o600},
		{name: "命名管道", mode: os.ModeNamedPipe | 0o600},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			skillsDir := t.TempDir()
			data := downloadTestZip(t,
				downloadTestZipEntry{name: "demo-skill-master-abc/SKILL.md", body: "# 技能\n"},
				downloadTestZipEntry{name: "demo-skill-master-abc/link", body: testCase.body, mode: testCase.mode},
			)
			var calls atomic.Int32
			client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)

			_, err := client.downloadSkill(context.Background(), "demo/skill", "")
			if err == nil {
				t.Fatal("期望拒绝特殊文件条目")
			}
			downloadAssertTargetAbsent(t, skillsDir, "demo--skill")
			downloadAssertNoTempDirs(t, skillsDir)
		})
	}
}

// 带 / 后缀的特殊条目不能因为名称像目录而绕过类型检查。
func TestDownloadSkillRejectsSpecialFilesWithSlashSuffix(t *testing.T) {
	cases := []struct {
		name string
		mode os.FileMode
	}{
		{name: "符号链接", mode: os.ModeSymlink | 0o777},
		{name: "设备文件", mode: os.ModeDevice | 0o600},
		{name: "字符设备", mode: os.ModeDevice | os.ModeCharDevice | 0o600},
		{name: "命名管道", mode: os.ModeNamedPipe | 0o600},
		{name: "套接字", mode: os.ModeSocket | 0o600},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			skillsDir := t.TempDir()
			data := downloadTestZip(t,
				downloadTestZipEntry{name: "demo-skill-master-abc/SKILL.md", body: "# 技能\n"},
				downloadTestZipEntry{name: "demo-skill-master-abc/link/", body: "", mode: testCase.mode},
			)
			var calls atomic.Int32
			client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)

			_, err := client.downloadSkill(context.Background(), "demo/skill", "")
			if err == nil {
				t.Fatal("期望拒绝带斜杠后缀的特殊文件条目")
			}
			if !strings.Contains(err.Error(), "特殊文件") && !strings.Contains(err.Error(), "符号链接") {
				t.Errorf("错误信息未说明特殊文件类型：%v", err)
			}
			downloadAssertTargetAbsent(t, skillsDir, "demo--skill")
			downloadAssertNoTempDirs(t, skillsDir)
		})
	}
}

// 重复条目、仅大小写不同的条目以及文件与目录路径冲突都必须被拒绝。
func TestDownloadSkillRejectsDuplicateAndCaseConflict(t *testing.T) {
	t.Run("重复条目", func(t *testing.T) {
		skillsDir := t.TempDir()
		data := downloadTestZip(t,
			downloadTestZipEntry{name: "demo-skill-master-abc/SKILL.md", body: "# 第一份\n"},
			downloadTestZipEntry{name: "demo-skill-master-abc/SKILL.md", body: "# 第二份\n"},
		)
		var calls atomic.Int32
		client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)

		_, err := client.downloadSkill(context.Background(), "demo/skill", "")
		if err == nil || !strings.Contains(err.Error(), "重复") {
			t.Fatalf("期望重复条目错误，实际：%v", err)
		}
		downloadAssertTargetAbsent(t, skillsDir, "demo--skill")
		downloadAssertNoTempDirs(t, skillsDir)
	})

	t.Run("大小写冲突", func(t *testing.T) {
		skillsDir := t.TempDir()
		data := downloadTestZip(t,
			downloadTestZipEntry{name: "demo-skill-master-abc/SKILL.md", body: "# 技能\n"},
			downloadTestZipEntry{name: "demo-skill-master-abc/README.md", body: "a"},
			downloadTestZipEntry{name: "demo-skill-master-abc/readme.md", body: "b"},
		)
		var calls atomic.Int32
		client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)

		_, err := client.downloadSkill(context.Background(), "demo/skill", "")
		if err == nil || !strings.Contains(err.Error(), "大小写") {
			t.Fatalf("期望大小写冲突错误，实际：%v", err)
		}
		downloadAssertTargetAbsent(t, skillsDir, "demo--skill")
		downloadAssertNoTempDirs(t, skillsDir)
	})

	t.Run("文件与目录冲突", func(t *testing.T) {
		skillsDir := t.TempDir()
		data := downloadTestZip(t,
			downloadTestZipEntry{name: "demo-skill-master-abc/SKILL.md", body: "# 技能\n"},
			downloadTestZipEntry{name: "demo-skill-master-abc/resources", body: "文件"},
			downloadTestZipEntry{name: "demo-skill-master-abc/resources/child.txt", body: "子文件"},
		)
		var calls atomic.Int32
		client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)

		_, err := client.downloadSkill(context.Background(), "demo/skill", "")
		if err == nil || !strings.Contains(err.Error(), "冲突") {
			t.Fatalf("期望路径类型冲突错误，实际：%v", err)
		}
		downloadAssertTargetAbsent(t, skillsDir, "demo--skill")
		downloadAssertNoTempDirs(t, skillsDir)
	})
}

// 最终技能根目录必须存在普通文件 SKILL.md。
func TestDownloadSkillRequiresSkillMarkdown(t *testing.T) {
	skillsDir := t.TempDir()
	data := downloadTestZip(t,
		downloadTestZipEntry{name: "demo-skill-master-abc/README.md", body: "# 没有技能说明\n"},
		downloadTestZipEntry{name: "demo-skill-master-abc/scripts/run.sh", body: "echo ok\n"},
	)
	var calls atomic.Int32
	client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)

	_, err := client.downloadSkill(context.Background(), "demo/skill", "")
	if err == nil || !strings.Contains(err.Error(), "SKILL.md") {
		t.Fatalf("期望缺少 SKILL.md 错误，实际：%v", err)
	}
	downloadAssertTargetAbsent(t, skillsDir, "demo--skill")
	downloadAssertNoTempDirs(t, skillsDir)
}

// 使用参数化的小上限验证压缩包体积、展开体积和条目数限制。
func TestDownloadSkillLimits(t *testing.T) {
	t.Run("压缩包体积", func(t *testing.T) {
		skillsDir := t.TempDir()
		var calls atomic.Int32
		handler := downloadTestAuthHandler(t, &calls, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(bytes.Repeat([]byte("x"), 128))
			// 强制使用分块传输，确保验证的是流式读取上限而不是 Content-Length 预检查。
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		})
		client := downloadTestServerClient(t, handler, skillsDir)
		limits := defaultDownloadLimits
		limits.maxArchiveBytes = 64

		_, err := client.downloadSkillWithLimits(context.Background(), "demo/skill", "", limits)
		if err == nil || !strings.Contains(err.Error(), "上限") {
			t.Fatalf("期望压缩包体积上限错误，实际：%v", err)
		}
		downloadAssertTargetAbsent(t, skillsDir, "demo--skill")
		downloadAssertNoTempDirs(t, skillsDir)
	})

	t.Run("展开体积", func(t *testing.T) {
		skillsDir := t.TempDir()
		data := downloadTestZip(t,
			downloadTestZipEntry{name: "demo-skill-master-abc/SKILL.md", body: "#"},
			downloadTestZipEntry{name: "demo-skill-master-abc/big.bin", body: strings.Repeat("a", 64)},
		)
		var calls atomic.Int32
		client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)
		limits := defaultDownloadLimits
		limits.maxExtractBytes = 32

		_, err := client.downloadSkillWithLimits(context.Background(), "demo/skill", "", limits)
		if err == nil || !strings.Contains(err.Error(), "展开大小") {
			t.Fatalf("期望展开体积上限错误，实际：%v", err)
		}
		downloadAssertTargetAbsent(t, skillsDir, "demo--skill")
		downloadAssertNoTempDirs(t, skillsDir)
	})

	t.Run("单文件展开体积", func(t *testing.T) {
		skillsDir := t.TempDir()
		data := downloadTestZip(t,
			downloadTestZipEntry{name: "demo-skill-master-abc/SKILL.md", body: "#"},
			downloadTestZipEntry{name: "demo-skill-master-abc/big.bin", body: strings.Repeat("a", 16)},
		)
		var calls atomic.Int32
		client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)
		limits := defaultDownloadLimits
		limits.maxExtractBytes = 8

		_, err := client.downloadSkillWithLimits(context.Background(), "demo/skill", "", limits)
		if err == nil || !strings.Contains(err.Error(), "展开大小") {
			t.Fatalf("期望单个文件展开体积上限错误，实际：%v", err)
		}
		downloadAssertTargetAbsent(t, skillsDir, "demo--skill")
		downloadAssertNoTempDirs(t, skillsDir)
	})

	t.Run("条目数量", func(t *testing.T) {
		skillsDir := t.TempDir()
		data := downloadTestZip(t,
			downloadTestZipEntry{name: "demo-skill-master-abc/SKILL.md", body: "#"},
			downloadTestZipEntry{name: "demo-skill-master-abc/a.txt", body: "a"},
			downloadTestZipEntry{name: "demo-skill-master-abc/b.txt", body: "b"},
		)
		var calls atomic.Int32
		client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)
		limits := defaultDownloadLimits
		limits.maxEntries = 2

		_, err := client.downloadSkillWithLimits(context.Background(), "demo/skill", "", limits)
		if err == nil || !strings.Contains(err.Error(), "条目数") {
			t.Fatalf("期望条目数上限错误，实际：%v", err)
		}
		downloadAssertTargetAbsent(t, skillsDir, "demo--skill")
		downloadAssertNoTempDirs(t, skillsDir)
	})
}

// 已取消的上下文应在创建临时目录和发起请求之前返回中文错误。
func TestDownloadSkillContextCanceled(t *testing.T) {
	skillsDir := t.TempDir()
	data := downloadTestZip(t, downloadTestZipEntry{name: "demo-skill-master/SKILL.md", body: "# 技能\n"})
	var calls atomic.Int32
	client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.downloadSkill(ctx, "demo/skill", "")
	if err == nil || !strings.Contains(err.Error(), "取消") {
		t.Fatalf("期望取消错误，实际：%v", err)
	}
	if calls.Load() != 0 {
		t.Errorf("取消后不应发起请求，实际 %d 次", calls.Load())
	}
	downloadAssertTargetAbsent(t, skillsDir, "demo--skill")
	downloadAssertNoTempDirs(t, skillsDir)
}

// 排队等待下载锁期间被取消时，拿到锁后必须立即失败，不能开始创建目录或发请求。
func TestDownloadSkillCanceledWhileWaitingForLock(t *testing.T) {
	skillsDir := t.TempDir()
	data := downloadTestZip(t, downloadTestZipEntry{name: "demo-skill-master/SKILL.md", body: "# 技能\n"})
	var calls atomic.Int32
	client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)

	// 父测试先持有下载锁，让新的下载调用排队。
	client.downloadMu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.downloadSkill(ctx, "demo/skill", "")
		done <- err
	}()

	// 给下载 goroutine 时间完成参数校验并阻塞在锁上。
	time.Sleep(200 * time.Millisecond)
	cancel()
	client.downloadMu.Unlock()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "取消") {
			t.Fatalf("期望排队取消错误，实际：%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("排队下载在取消后未返回")
	}
	if calls.Load() != 0 {
		t.Errorf("取消后不应发起请求，实际 %d 次", calls.Load())
	}
	downloadAssertTargetAbsent(t, skillsDir, "demo--skill")
	downloadAssertNoTempDirs(t, skillsDir)
}

// 不安全的版本名必须在请求前被拒绝。
func TestDownloadSkillRejectsUnsafeRevision(t *testing.T) {
	cases := []struct {
		name     string
		revision string
	}{
		{name: "上级目录", revision: ".."},
		{name: "当前目录", revision: "."},
		{name: "正斜杠", revision: "a/b"},
		{name: "反斜杠", revision: `a\b`},
		{name: "查询串", revision: "a?b"},
		{name: "片段", revision: "a#b"},
		{name: "空格", revision: "a b"},
		{name: "控制字符", revision: "a\x01b"},
		{name: "百分号编码", revision: "a%2fb"},
		{name: "空段", revision: "/"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			skillsDir := t.TempDir()
			data := downloadTestZip(t, downloadTestZipEntry{name: "demo-skill-master/SKILL.md", body: "# 技能\n"})
			var calls atomic.Int32
			client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)

			_, err := client.downloadSkill(context.Background(), "demo/skill", testCase.revision)
			if err == nil {
				t.Fatalf("期望拒绝版本名 %q", testCase.revision)
			}
			if calls.Load() != 0 {
				t.Errorf("版本名校验失败时不应发起请求，实际 %d 次", calls.Load())
			}
			downloadAssertTargetAbsent(t, skillsDir, "demo--skill")
			downloadAssertNoTempDirs(t, skillsDir)
		})
	}
}

// 不安全的技能 ID 必须由方法内部的 normalizeMarketID 校验拒绝。
func TestDownloadSkillRejectsUnsafeID(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{name: "空标识", id: ""},
		{name: "上级目录", id: "../evil"},
		{name: "反斜杠", id: `a\b`},
		{name: "控制字符", id: "a\nb"},
		{name: "盘符", id: "C:/evil"},
		{name: "绝对路径", id: "/evil"},
		{name: "完整网址", id: "https://example.com/skills/evil"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			skillsDir := t.TempDir()
			data := downloadTestZip(t, downloadTestZipEntry{name: "demo-skill-master/SKILL.md", body: "# 技能\n"})
			var calls atomic.Int32
			client := downloadTestServerClient(t, downloadTestZipHandler(t, data, "/skills/demo/skill/archive/zip/master", &calls), skillsDir)

			_, err := client.downloadSkill(context.Background(), testCase.id, "")
			if err == nil {
				t.Fatalf("期望拒绝技能 ID %q", testCase.id)
			}
			if calls.Load() != 0 {
				t.Errorf("技能 ID 校验失败时不应发起请求，实际 %d 次", calls.Load())
			}
			downloadAssertTargetAbsent(t, skillsDir, "demo--skill")
			downloadAssertNoTempDirs(t, skillsDir)
		})
	}
}
