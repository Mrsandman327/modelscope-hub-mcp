package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// 下载与解压相关的固定限制。测试通过 downloadSkillWithLimits 注入更小的限制，
// 以便用很小的测试压缩包验证各个上限逻辑，避免构造大体积数据。
const (
	// downloadDefaultRevision 是未指定版本时使用的默认版本。
	downloadDefaultRevision = "master"
	// downloadTimeout 限制一次下载、解压和发布的整体耗时。
	downloadTimeout = 120 * time.Second
	// downloadTempPrefix 是本进程创建的临时目录前缀，成功或失败都会清理。
	downloadTempPrefix = ".skills-download-"
	// downloadMaxArchiveBytes 是允许下载的压缩包最大字节数。
	downloadMaxArchiveBytes int64 = 64 << 20
	// downloadMaxExtractBytes 是允许解压展开的最大总字节数。
	downloadMaxExtractBytes uint64 = 256 << 20
	// downloadMaxEntries 是允许的压缩包条目数上限。
	downloadMaxEntries = 5000
	// downloadMaxStatusBody 是读取错误响应正文的上限，避免错误处理本身消耗过多内存。
	downloadMaxStatusBody int64 = 4 << 10
	// downloadMaxRevisionLength 限制版本名长度，避免异常长的路径。
	downloadMaxRevisionLength = 128
	// downloadMaxSegmentRunes 限制单个路径组件的字符数，兼容 Windows 常见限制。
	downloadMaxSegmentRunes = 255
	// downloadMaxInt64 用于在把 uint64 大小转换成 int64 前做溢出保护。
	downloadMaxInt64 = uint64(1<<63 - 1)
)

// downloadResult 是技能下载成功后的结果，可直接交给 json.Marshal 输出。
type downloadResult struct {
	ID          string `json:"id"`
	Revision    string `json:"revision"`
	DownloadURL string `json:"download_url"`
	SkillDir    string `json:"skill_dir"`
	Files       int    `json:"files"`
	Bytes       int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
}

// downloadLimits 把各项上限参数化，生产环境使用 defaultDownloadLimits，
// 测试可以传入很小的值来覆盖上限分支。
type downloadLimits struct {
	maxArchiveBytes int64
	maxExtractBytes uint64
	maxEntries      int
}

// defaultDownloadLimits 是生产环境使用的下载与解压限制。
var defaultDownloadLimits = downloadLimits{
	maxArchiveBytes: downloadMaxArchiveBytes,
	maxExtractBytes: downloadMaxExtractBytes,
	maxEntries:      downloadMaxEntries,
}

// downloadEntry 是经过安全校验后的压缩包条目。
type downloadEntry struct {
	file *zip.File // 原始压缩包条目
	orig string    // 清理并校验后的完整相对路径，使用正斜杠
	rel  string    // 去掉可选单根目录后的最终相对路径，使用正斜杠
	dir  bool      // 是否为目录条目
	size uint64    // 声明的展开后大小
}

// downloadStats 记录实际写入的文件数量与字节数。
type downloadStats struct {
	files int
	bytes int64
}

// downloadSkill 下载并解压一个技能包，默认超时 120 秒。
func (c *hubClient) downloadSkill(ctx context.Context, id, revision string) (*downloadResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	return c.downloadSkillWithLimits(timeoutCtx, id, revision, defaultDownloadLimits)
}

// downloadSkillWithLimits 是实际实现。它只接受技能 ID 和版本名，不接受任意 URL，
// 不会执行技能中的任何代码或安装脚本，只把压缩包安全解压到配置的技能根目录下。
func (c *hubClient) downloadSkillWithLimits(ctx context.Context, id, revision string, limits downloadLimits) (*downloadResult, error) {
	if c == nil {
		return nil, errors.New("下载技能失败：客户端未初始化")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, downloadContextErr(err)
	}
	if err := downloadValidateLimits(limits); err != nil {
		return nil, err
	}

	// 即使调用者已经校验过，方法内部仍必须再次校验技能 ID。
	safeID, err := normalizeMarketID(id)
	if err != nil {
		return nil, fmt.Errorf("下载技能失败：技能标识无效：%s", c.redact(err.Error()))
	}
	if strings.TrimSpace(safeID) == "" {
		return nil, errors.New("下载技能失败：技能标识为空")
	}
	// 在 normalizeMarketID 之外再做一层路径与 URL 字符防护，避免异常标识
	// 被拼进请求路径或目标目录名。
	if err := downloadValidateSafeID(safeID); err != nil {
		return nil, err
	}

	rev, err := downloadNormalizeRevision(revision)
	if err != nil {
		return nil, err
	}

	// 技能根目录只能来自配置，不能从工具参数传入任意绝对路径。
	rootDir := c.skillsDir
	if strings.TrimSpace(rootDir) == "" {
		return nil, errors.New("下载技能失败：未配置技能保存目录")
	}
	if !filepath.IsAbs(rootDir) {
		return nil, errors.New("下载技能失败：技能保存目录必须是绝对路径")
	}

	dirName, err := downloadDirName(safeID)
	if err != nil {
		return nil, err
	}

	// 同一进程内的下载串行执行，避免检查目标、解压和发布之间互相冲突。
	c.downloadMu.Lock()
	defer c.downloadMu.Unlock()
	// 排队等待锁期间上下文可能已经取消或超时，拿到锁后必须立即复查，
	// 不能等已经开始创建目录或发起请求才发现。
	if err := ctx.Err(); err != nil {
		return nil, downloadContextErr(err)
	}

	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("下载技能失败：创建技能保存目录失败：%s", c.redact(err.Error()))
	}
	// 使用 os.Root 把后续所有写入限制在技能根目录内，即使路径校验出现疏漏，
	// 也不应在根目录之外创建文件。
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, fmt.Errorf("下载技能失败：打开技能保存目录失败：%s", c.redact(err.Error()))
	}
	defer root.Close()

	targetName := dirName
	targetPath := filepath.Join(rootDir, targetName)

	// 发布最终目录前先确认目标不存在，已有任何文件、目录或链接都拒绝覆盖。
	if err := downloadEnsureTargetAbsent(root, targetName); err != nil {
		return nil, err
	}

	// 所有临时数据都放在技能根目录内，且只清理本次创建的临时目录。
	tempRoot, err := os.MkdirTemp(rootDir, downloadTempPrefix)
	if err != nil {
		return nil, fmt.Errorf("下载技能失败：创建临时目录失败：%s", c.redact(err.Error()))
	}
	tempRel := filepath.Base(tempRoot)
	defer root.RemoveAll(tempRel)

	stageRel := filepath.Join(tempRel, "stage")
	if err := root.Mkdir(stageRel, 0o755); err != nil {
		return nil, fmt.Errorf("下载技能失败：创建临时解压目录失败：%s", c.redact(err.Error()))
	}
	stage, err := root.OpenRoot(stageRel)
	if err != nil {
		return nil, fmt.Errorf("下载技能失败：打开临时解压目录失败：%s", c.redact(err.Error()))
	}
	stageClosed := false
	defer func() {
		if !stageClosed {
			_ = stage.Close()
		}
	}()

	archiveRel := filepath.Join(tempRel, "archive.zip")
	archiveFile, err := root.Create(archiveRel)
	if err != nil {
		return nil, fmt.Errorf("下载技能失败：创建临时压缩包失败：%s", c.redact(err.Error()))
	}
	// 压缩包只作为临时文件保留，成功后随临时目录删除；不读入内存。
	defer archiveFile.Close()

	hasher := sha256.New()
	if _, err := c.downloadArchiveTo(ctx, safeID, rev, archiveFile, hasher, limits); err != nil {
		return nil, err
	}

	archiveInfo, err := archiveFile.Stat()
	if err != nil {
		return nil, fmt.Errorf("下载技能失败：读取临时压缩包信息失败：%s", c.redact(err.Error()))
	}
	zr, err := zip.NewReader(archiveFile, archiveInfo.Size())
	if err != nil {
		return nil, fmt.Errorf("下载技能失败：技能压缩包格式无效：%s", c.redact(err.Error()))
	}

	entries, err := downloadParseEntries(zr, limits)
	if err != nil {
		return nil, err
	}
	entries = downloadStripSingleRoot(entries)
	if err := downloadValidateSkillRoot(entries); err != nil {
		return nil, err
	}

	stats, err := downloadExtractEntries(ctx, stage, entries, limits)
	if err != nil {
		return nil, err
	}

	// 发布前再次确认目标仍然不存在，降低并发或外部操作带来的覆盖风险。
	if err := downloadEnsureTargetAbsent(root, targetName); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, downloadContextErr(err)
	}

	// Windows 上重命名目录前需要先关闭目录句柄。
	if err := stage.Close(); err != nil {
		return nil, fmt.Errorf("下载技能失败：关闭临时解压目录失败：%s", c.redact(err.Error()))
	}
	stageClosed = true

	if err := root.Rename(stageRel, targetName); err != nil {
		return nil, fmt.Errorf("下载技能失败：发布技能目录失败：%s", c.redact(err.Error()))
	}

	return &downloadResult{
		ID:          safeID,
		Revision:    rev,
		DownloadURL: c.archiveURL(safeID, rev),
		SkillDir:    targetPath,
		Files:       stats.files,
		Bytes:       stats.bytes,
		SHA256:      hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

// downloadValidateLimits 检查内部限制是否为正数，防止误传零值导致所有输入都被拒绝
// 或上限判断失效。
func downloadValidateLimits(limits downloadLimits) error {
	if limits.maxArchiveBytes <= 0 || limits.maxExtractBytes == 0 || limits.maxEntries <= 0 {
		return errors.New("下载技能失败：下载限制配置无效")
	}
	// 为流式读取保留一个额外字节时不能溢出 int64。
	if uint64(limits.maxArchiveBytes) >= downloadMaxInt64 {
		return errors.New("下载技能失败：下载限制配置无效")
	}
	return nil
}

// downloadNormalizeRevision 校验版本名。空版本默认为 master，只允许安全单段名称。
func downloadNormalizeRevision(revision string) (string, error) {
	if revision == "" {
		return downloadDefaultRevision, nil
	}
	if len(revision) > downloadMaxRevisionLength {
		return "", fmt.Errorf("下载技能失败：技能版本名过长（最多 %d 个字符）", downloadMaxRevisionLength)
	}
	if revision == "." || revision == ".." {
		return "", errors.New("下载技能失败：技能版本名不能是 . 或 ..")
	}
	for _, r := range revision {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_', r == '+':
		default:
			return "", errors.New("下载技能失败：技能版本名只能包含字母、数字、点、短横线、下划线和加号，且不能包含路径分隔符")
		}
	}
	return revision, nil
}

// downloadValidateSafeID 对已经过 normalizeMarketID 的标识再做一次防御性校验。
// 技能 ID 只允许由斜杠分隔的安全路径段，不允许点段、空段、反斜杠、控制字符
// 以及会改变 URL 路径含义的字符。
func downloadValidateSafeID(id string) error {
	if !utf8.ValidString(id) {
		return errors.New("下载技能失败：技能标识不是有效的 UTF-8 文本")
	}
	trimmed := strings.TrimPrefix(id, "@")
	if trimmed == "" {
		return errors.New("下载技能失败：技能标识为空")
	}
	if strings.Contains(trimmed, `\`) {
		return errors.New("下载技能失败：技能标识包含反斜杠")
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("下载技能失败：技能标识包含空路径段或点路径段")
		}
		for _, r := range segment {
			if r < 0x20 || r == 0x7f {
				return errors.New("下载技能失败：技能标识包含控制字符")
			}
			switch r {
			case ':', '?', '#', '%':
				return errors.New("下载技能失败：技能标识包含不允许的 URL 或路径字符")
			}
		}
	}
	return nil
}

// downloadDirName 根据技能 ID 生成单段目标目录名：去掉开头的 @，把路径分隔符
// 替换为 --，再按与压缩包条目相同的 Windows 安全规则校验。
func downloadDirName(id string) (string, error) {
	trimmed := strings.TrimPrefix(id, "@")
	if trimmed == "" {
		return "", errors.New("下载技能失败：技能标识为空")
	}
	if strings.Contains(trimmed, `\`) {
		return "", errors.New("下载技能失败：技能标识包含反斜杠")
	}
	if strings.HasPrefix(trimmed, "/") || strings.HasSuffix(trimmed, "/") || strings.Contains(trimmed, "//") {
		return "", errors.New("下载技能失败：技能标识包含空路径段")
	}
	name := strings.ReplaceAll(trimmed, "/", "--")
	if name == "" || name == "." || name == ".." {
		return "", errors.New("下载技能失败：技能目录名无效")
	}
	if err := downloadValidateSegment(name); err != nil {
		return "", fmt.Errorf("下载技能失败：技能目录名不安全：%s", err)
	}
	return name, nil
}

// downloadValidateSegment 校验单个路径组件，拒绝 Windows 无法安全落盘的名称。
func downloadValidateSegment(segment string) error {
	if segment == "" || segment == "." || segment == ".." {
		return errors.New("路径段不能为空、. 或 ..")
	}
	if !utf8.ValidString(segment) {
		return errors.New("路径段不是有效的 UTF-8 文本")
	}
	for _, r := range segment {
		if r < 0x20 || r == 0x7f {
			return errors.New("路径段包含控制字符")
		}
	}
	if strings.ContainsAny(segment, `\/:*?"<>|`) {
		return errors.New("路径段包含路径分隔符、冒号或其他 Windows 不允许的字符")
	}
	if strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " ") {
		return errors.New("路径段不能以点或空格结尾")
	}
	if downloadIsWindowsReservedName(segment) {
		return errors.New("路径段使用了 Windows 保留设备名")
	}
	if utf8.RuneCountInString(segment) > downloadMaxSegmentRunes {
		return fmt.Errorf("路径段过长（最多 %d 个字符）", downloadMaxSegmentRunes)
	}
	return nil
}

// downloadIsWindowsReservedName 判断名称是否为 Windows 保留设备名。
// Windows 对带扩展名的形式同样保留，例如 CON.txt 也不允许。
func downloadIsWindowsReservedName(segment string) bool {
	upper := strings.ToUpper(segment)
	base := upper
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	// 尾随空格在前面已被拒绝，这里再做一次防御性裁剪。
	base = strings.TrimRight(base, " ")
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$", "CLOCK$":
		return true
	// Windows 也将上标数字形式的 COM/LPT 设备名视为保留名称。
	case "COM¹", "COM²", "COM³", "LPT¹", "LPT²", "LPT³":
		return true
	}
	if len(base) == 4 {
		prefix := base[:3]
		if (prefix == "COM" || prefix == "LPT") && base[3] >= '1' && base[3] <= '9' {
			return true
		}
	}
	return false
}

// downloadCleanEntryName 清理并校验压缩包条目名。返回使用正斜杠的规范相对路径。
func downloadCleanEntryName(raw string, isDir bool) (string, error) {
	if raw == "" {
		return "", errors.New("技能压缩包包含空条目名")
	}
	if !utf8.ValidString(raw) {
		return "", fmt.Errorf("技能压缩包条目名不是有效的 UTF-8 文本：%q", raw)
	}
	if strings.Contains(raw, `\`) {
		return "", fmt.Errorf("技能压缩包条目包含反斜杠路径：%s", raw)
	}
	if strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("技能压缩包包含绝对路径条目：%s", raw)
	}
	name := raw
	if isDir {
		name = strings.TrimSuffix(name, "/")
	}
	if name == "" {
		return "", fmt.Errorf("技能压缩包条目路径为空：%q", raw)
	}
	segments := strings.Split(name, "/")
	for _, segment := range segments {
		if segment == "" {
			return "", fmt.Errorf("技能压缩包条目包含空路径段：%s", raw)
		}
		if err := downloadValidateSegment(segment); err != nil {
			return "", fmt.Errorf("技能压缩包条目路径不安全（%s）：%s", raw, err)
		}
	}
	return strings.Join(segments, "/"), nil
}

// downloadParseEntries 在真正写盘前完成全部条目校验，包括条目数、声明大小、
// 特殊文件类型、重复和大小写冲突、文件与目录冲突。
func downloadParseEntries(zr *zip.Reader, limits downloadLimits) ([]downloadEntry, error) {
	if zr == nil || len(zr.File) == 0 {
		return nil, errors.New("下载技能失败：技能压缩包为空")
	}
	if len(zr.File) > limits.maxEntries {
		return nil, fmt.Errorf("下载技能失败：技能压缩包条目数超过上限（%d 个）", limits.maxEntries)
	}

	entries := make([]downloadEntry, 0, len(zr.File))
	seen := make(map[string]string, len(zr.File))
	kinds := make(map[string]bool, len(zr.File))
	prefixSpellings := make(map[string]string, len(zr.File))
	var total uint64

	for _, f := range zr.File {
		if f.Flags&0x1 != 0 {
			return nil, fmt.Errorf("下载技能失败：技能压缩包包含加密条目：%s", f.Name)
		}
		mode := f.Mode()
		if mode&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("下载技能失败：技能压缩包包含符号链接：%s", f.Name)
		}
		// 在根据名称尾斜杠推断目录之前，先拒绝既不是普通文件也不是目录的类型。
		// 否则带 / 后缀的设备、命名管道或套接字条目会冒充目录绕过检查。
		if modeType := mode & os.ModeType; modeType != 0 && modeType != os.ModeDir {
			return nil, fmt.Errorf("下载技能失败：技能压缩包包含不支持的特殊文件：%s", f.Name)
		}
		isDir := mode.IsDir() || strings.HasSuffix(f.Name, "/")
		if !isDir && !mode.IsRegular() {
			return nil, fmt.Errorf("下载技能失败：技能压缩包包含不支持的特殊文件：%s", f.Name)
		}

		cleaned, err := downloadCleanEntryName(f.Name, isDir)
		if err != nil {
			return nil, fmt.Errorf("下载技能失败：%s", err.Error())
		}

		// 逐级检查路径前缀的拼写。Windows 文件系统大小写不敏感，
		// 仅大小写不同的目录同样会合并，因此也视为冲突。
		parts := strings.Split(cleaned, "/")
		for i := 1; i <= len(parts); i++ {
			prefix := strings.Join(parts[:i], "/")
			key := strings.ToLower(prefix)
			if previous, exists := prefixSpellings[key]; exists {
				if previous != prefix {
					return nil, fmt.Errorf("下载技能失败：技能压缩包包含大小写冲突条目：%s 与 %s", previous, prefix)
				}
			} else {
				prefixSpellings[key] = prefix
			}
		}

		// 大小写不敏感地检查重复条目和仅大小写不同的冲突条目。
		key := strings.ToLower(cleaned)
		if previous, exists := seen[key]; exists {
			if previous == cleaned {
				return nil, fmt.Errorf("下载技能失败：技能压缩包包含重复条目：%s", cleaned)
			}
			return nil, fmt.Errorf("下载技能失败：技能压缩包包含大小写冲突条目：%s 与 %s", previous, cleaned)
		}
		seen[key] = cleaned
		kinds[key] = isDir

		if !isDir {
			size := f.UncompressedSize64
			if size > limits.maxExtractBytes || total > limits.maxExtractBytes-size {
				return nil, fmt.Errorf("下载技能失败：技能压缩包展开大小超过上限（%d 字节）", limits.maxExtractBytes)
			}
			total += size
		}

		entries = append(entries, downloadEntry{
			file: f,
			orig: cleaned,
			dir:  isDir,
			size: f.UncompressedSize64,
		})
	}

	// 任意条目的祖先路径不能是普通文件，避免 Windows 上先写文件再建同名目录导致
	// 覆盖或写入失败。
	for _, entry := range entries {
		parts := strings.Split(entry.orig, "/")
		for i := 1; i < len(parts); i++ {
			ancestor := strings.ToLower(strings.Join(parts[:i], "/"))
			if isDir, exists := kinds[ancestor]; exists && !isDir {
				return nil, fmt.Errorf("下载技能失败：技能压缩包存在文件与目录路径冲突：%s", entry.orig)
			}
		}
	}

	return entries, nil
}

// downloadStripSingleRoot 在压缩包只有唯一顶层目录时去掉该层，使最终技能根目录
// 直接包含 SKILL.md。若不是单根结构则原样保留。
func downloadStripSingleRoot(entries []downloadEntry) []downloadEntry {
	if len(entries) == 0 {
		return entries
	}
	root := ""
	for _, entry := range entries {
		first := entry.orig
		if i := strings.IndexByte(first, '/'); i >= 0 {
			first = first[:i]
		}
		if root == "" {
			root = first
			continue
		}
		if first != root {
			// 多个顶层名称，不做剥离。
			for i := range entries {
				entries[i].rel = entries[i].orig
			}
			return entries
		}
	}
	if root == "" {
		for i := range entries {
			entries[i].rel = entries[i].orig
		}
		return entries
	}

	hasChild := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.orig, root+"/") {
			hasChild = true
			break
		}
		if entry.orig == root && !entry.dir {
			// 顶层同名对象是文件，不可能作为单根目录剥离。
			for i := range entries {
				entries[i].rel = entries[i].orig
			}
			return entries
		}
	}
	if !hasChild {
		for i := range entries {
			entries[i].rel = entries[i].orig
		}
		return entries
	}

	next := make([]downloadEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.orig == root {
			// 丢弃单根目录条目本身。
			continue
		}
		entry.rel = strings.TrimPrefix(entry.orig, root+"/")
		if entry.rel == "" {
			continue
		}
		next = append(next, entry)
	}
	if len(next) == 0 {
		for i := range entries {
			entries[i].rel = entries[i].orig
		}
		return entries
	}
	return next
}

// downloadValidateSkillRoot 要求最终技能根目录下存在普通文件 SKILL.md。
func downloadValidateSkillRoot(entries []downloadEntry) error {
	found := false
	for _, entry := range entries {
		if entry.rel == "" {
			continue
		}
		if entry.rel == "SKILL.md" {
			if entry.dir {
				return errors.New("下载技能失败：技能根目录的 SKILL.md 不是普通文件")
			}
			found = true
		}
	}
	if !found {
		return errors.New("下载技能失败：技能压缩包缺少根目录 SKILL.md 文件")
	}
	return nil
}

// downloadExtractEntries 把已经校验过的条目流式写入临时解压目录。
// 不把整个压缩包或单个文件读入内存。
func downloadExtractEntries(ctx context.Context, stage *os.Root, entries []downloadEntry, limits downloadLimits) (downloadStats, error) {
	var stats downloadStats
	var total uint64

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return stats, downloadContextErr(err)
		}
		if entry.rel == "" {
			continue
		}
		if entry.dir {
			if err := stage.MkdirAll(entry.rel, 0o755); err != nil {
				return stats, fmt.Errorf("下载技能失败：创建技能目录失败（%s）：%s", entry.rel, err.Error())
			}
			continue
		}

		if entry.size > limits.maxExtractBytes || total > limits.maxExtractBytes-entry.size {
			return stats, fmt.Errorf("下载技能失败：技能压缩包展开大小超过上限（%d 字节）", limits.maxExtractBytes)
		}
		if entry.size >= downloadMaxInt64 {
			return stats, errors.New("下载技能失败：技能压缩包条目大小异常")
		}

		parent := path.Dir(entry.rel)
		if parent != "." {
			if err := stage.MkdirAll(parent, 0o755); err != nil {
				return stats, fmt.Errorf("下载技能失败：创建技能目录失败（%s）：%s", parent, err.Error())
			}
		}

		// O_EXCL 作为最后一道防线，任何已存在的同名条目都拒绝覆盖。
		dst, err := stage.OpenFile(entry.rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return stats, fmt.Errorf("下载技能失败：创建技能文件失败（%s）：%s", entry.rel, err.Error())
		}

		src, err := entry.file.Open()
		if err != nil {
			_ = dst.Close()
			return stats, fmt.Errorf("下载技能失败：读取压缩包条目失败（%s）：%s", entry.rel, err.Error())
		}

		// 多读一个字节用于发现声明大小与实际内容不一致的情况。
		written, copyErr := io.Copy(dst, io.LimitReader(src, int64(entry.size)+1))
		readCloseErr := src.Close()
		writeCloseErr := dst.Close()

		if copyErr != nil {
			return stats, fmt.Errorf("下载技能失败：写入技能文件失败（%s）：%s", entry.rel, copyErr.Error())
		}
		if readCloseErr != nil {
			return stats, fmt.Errorf("下载技能失败：关闭压缩包条目失败（%s）：%s", entry.rel, readCloseErr.Error())
		}
		if writeCloseErr != nil {
			return stats, fmt.Errorf("下载技能失败：关闭技能文件失败（%s）：%s", entry.rel, writeCloseErr.Error())
		}
		if uint64(written) != entry.size {
			return stats, fmt.Errorf("下载技能失败：压缩包条目大小与声明不一致：%s", entry.rel)
		}

		total += uint64(written)
		stats.files++
		stats.bytes += written
	}

	info, err := stage.Lstat("SKILL.md")
	if err != nil || !info.Mode().IsRegular() {
		return stats, errors.New("下载技能失败：技能根目录缺少普通文件 SKILL.md")
	}
	return stats, nil
}

// downloadArchiveTo 通过统一的请求方法下载压缩包，边下载边写入临时文件和哈希。
// 它只接受技能 ID，不接受任意 URL，因此不会跟随调用者提供的地址。
func (c *hubClient) downloadArchiveTo(ctx context.Context, id, revision string, dst *os.File, hasher hash.Hash, limits downloadLimits) (int64, error) {
	archivePath := "/skills/" + id + "/archive/zip/" + url.PathEscape(revision)
	resp, err := c.request(ctx, http.MethodGet, archivePath, nil, nil)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 0, downloadContextErr(err)
		}
		return 0, fmt.Errorf("下载技能失败：请求下载地址失败：%s", c.redact(err.Error()))
	}
	if resp == nil || resp.Body == nil {
		return 0, errors.New("下载技能失败：下载响应无效")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, downloadMaxStatusBody))
		detail := strings.TrimSpace(string(body))
		if detail == "" {
			detail = http.StatusText(resp.StatusCode)
		}
		return 0, fmt.Errorf("下载技能失败：服务返回 HTTP %d：%s", resp.StatusCode, c.redact(detail))
	}

	// 先根据 Content-Length 提前拒绝明显超限的响应；未知长度时仍由下面的
	// LimitReader 保护。
	if resp.ContentLength > limits.maxArchiveBytes {
		return 0, fmt.Errorf("下载技能失败：技能压缩包超过上限（%d 字节）", limits.maxArchiveBytes)
	}

	limited := &io.LimitedReader{R: resp.Body, N: limits.maxArchiveBytes + 1}
	written, err := io.Copy(io.MultiWriter(dst, hasher), limited)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return written, downloadContextErr(err)
		}
		return written, fmt.Errorf("下载技能失败：读取技能压缩包失败：%s", c.redact(err.Error()))
	}
	// 读取 max+1 后即停止，一旦超过上限必须报错，绝不能截断后当作成功。
	if written > limits.maxArchiveBytes {
		return written, fmt.Errorf("下载技能失败：技能压缩包超过上限（%d 字节）", limits.maxArchiveBytes)
	}
	if written == 0 {
		return 0, errors.New("下载技能失败：技能压缩包为空")
	}
	return written, nil
}

// downloadEnsureTargetAbsent 使用 os.Root 的相对路径检查目标是否存在。
// 使用 Lstat 以便把断开的符号链接也视为已存在，任何已有对象都不覆盖。
func downloadEnsureTargetAbsent(root *os.Root, name string) error {
	_, err := root.Lstat(name)
	if err == nil {
		return fmt.Errorf("下载技能失败：目标技能目录已存在，拒绝覆盖：%s", name)
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("下载技能失败：检查目标技能目录失败：%s", err.Error())
}

// downloadContextErr 把上下文取消或超时转换为中文错误。
func downloadContextErr(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return errors.New("下载技能失败：操作已取消")
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("下载技能失败：操作超时")
	default:
		return fmt.Errorf("下载技能失败：请求未能完成：%s", err.Error())
	}
}
