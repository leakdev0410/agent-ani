package memdb

import (
	"fmt"
	"os"
	"path/filepath"
)

// OpenSnapshot copy DB tại srcPath sang 1 file mới trong destDir rồi mở bản sao đó — dùng để
// test/đọc dữ liệu thật làm fixture mà không bao giờ khoá hay đụng vào DB gốc đang chạy thật.
// destDir nên là t.TempDir() để tự dọn sau khi test xong.
func OpenSnapshot(srcPath, destDir string) (*DB, error) {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return nil, fmt.Errorf("memdb: không đọc được %s để snapshot: %w", srcPath, err)
	}
	destPath := filepath.Join(destDir, "snapshot.db")
	if err := os.WriteFile(destPath, data, 0644); err != nil {
		return nil, fmt.Errorf("memdb: không ghi được snapshot %s: %w", destPath, err)
	}
	return Open(destPath)
}
