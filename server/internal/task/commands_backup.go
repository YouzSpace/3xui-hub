package task

// 数据库备份（backup:run）与备份文件操作工具：建目录、gzip、保留份数。
// 从 commands.go 按职责拆出（issue #137）。

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"controlhub-server/internal/tgbot"
)

// runBackupRun 对齐 BackupRun::handle()。
// mysqldump stdout 流式写 gzip，失败删半截文件，成功后按时间戳保留最新 keep 份。
func (r *commandRunner) runBackupRun(ctx context.Context) error {
	// 超时兜底：mysqldump 卡住（大库/磁盘慢/进程挂起）会永久占用 withoutOverlapping
	// 标志，后续每天备份都被跳过且无报错。30 分钟足够导出大库，超时自动掐断。
	timeout := r.deps.BackupTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bin := r.deps.BackupBin
	if bin == "" {
		bin = "mysqldump"
	}
	dir := strings.TrimRight(r.deps.BackupDir, `/\`)
	if dir == "" {
		dir = "/var/backups/3xui-hub"
	}
	keep := r.deps.BackupKeep
	if keep < 1 {
		keep = 7
	}

	// 创建备份目录
	if err := ensureDir(dir); err != nil {
		return fmt.Errorf("备份目录创建失败：%s: %w", dir, err)
	}

	dbName := r.deps.DBName
	if dbName == "" {
		return fmt.Errorf("未配置数据库名")
	}

	target := filepath.Join(dir, "backup-"+r.now().Format("20060102150405")+".sql.gz")

	args := []string{
		"--single-transaction",
		"--skip-lock-tables",
		"--host=" + r.deps.DBHost,
		"--port=" + r.deps.DBPort,
		"-u", r.deps.DBUser,
		dbName,
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	if r.deps.DBPassword != "" {
		cmd.Env = append(cmd.Environ(), "MYSQL_PWD="+r.deps.DBPassword)
	}

	// 流式写 gzip
	gzWriter, err := newGzipFile(target)
	if err != nil {
		return fmt.Errorf("备份文件无法写入：%s: %w", target, err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		gzWriter.Close()
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		gzWriter.Close()
		removeIfExists(target)
		return fmt.Errorf("备份失败（mysqldump 启动失败）：%v", err)
	}

	// 流式拷贝 stdout → gzip
	buf := make([]byte, 32*1024)
	for {
		n, readErr := stdout.Read(buf)
		if n > 0 {
			if _, werr := gzWriter.Write(buf[:n]); werr != nil {
				break
			}
		}
		if readErr != nil {
			break
		}
	}
	gzWriter.Close()

	waitErr := cmd.Wait()
	if waitErr != nil {
		removeIfExists(target)
		summary := strings.TrimSpace(stderr.String())
		if len(summary) > 2000 {
			summary = summary[:2000]
		}
		return fmt.Errorf("备份失败（mysqldump 退出）：%v\n%s", waitErr, summary)
	}

	// 按文件名倒序保留最新 keep 份
	pruneOldBackups(dir, keep)

	size := fileSize(target)
	r.logf("备份完成：%s（%.1f KB）", target, float64(size)/1024)

	// TG 推送（开关 tg_backup_push 开启且已配置 token/chat_id 才发）。
	if r.deps.TGSender != nil && r.deps.TGSender.NotifyEnabled(ctx, tgbot.KeyBackupPush) {
		caption := fmt.Sprintf("ControlHub 每日备份 %s（%.1f KB）",
			r.now().Format("2006-01-02 15:04:05"), float64(size)/1024)
		if err := r.deps.TGSender.SendDocument(ctx, caption, target); err != nil {
			r.logf("TG 推送备份失败: %v", err)
		}
	}
	return nil
}

// ── 3. monitor:rollup ───────────────────────────────────────────────

// ensureDir 确保目录存在。
func ensureDir(dir string) error {
	return ensureDirImpl(dir)
}

// removeIfExists 删除文件（忽略不存在）。
func removeIfExists(path string) {
	removeIfExistsImpl(path)
}

// fileSize 返回文件大小。
func fileSize(path string) int64 {
	return fileSizeImpl(path)
}

// pruneOldBackups 按文件名倒序只保留最新 keep 份。
func pruneOldBackups(dir string, keep int) {
	pruneOldBackupsImpl(dir, keep)
}

// newGzipFile 创建 gzip 文件写入器。
func newGzipFile(path string) (gzipWriteCloser, error) {
	return newGzipFileImpl(path)
}

// gzipWriteCloser 是 gzip 写入器接口。
type gzipWriteCloser interface {
	Write(p []byte) (int, error)
	Close() error
}
