package memdb

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SeedFromDir nạp toàn bộ nội dung memory.md + memory/*.md từ memoryDir (định
// dạng cũ, dùng chung qua git) vào DB — CHỈ chạy 1 lần khi DB còn trống
// (xem IsSeeded), để không ghi đè dữ liệu mới hơn đã có trong DB ở lần khởi
// động sau. Không sửa/xoá file gốc trên đĩa — giữ nguyên làm bản sao lưu.
func (db *DB) SeedFromDir(memoryDir string) (int, error) {
	coreMemory, err := os.ReadFile(filepath.Join(memoryDir, "memory.md"))
	if err != nil {
		return 0, fmt.Errorf("memdb: không đọc được memory.md tại %s: %w", memoryDir, err)
	}
	if err := db.Write(KeyCoreMemory, string(coreMemory)); err != nil {
		return 0, err
	}
	count := 1

	topicDir := filepath.Join(memoryDir, "memory")
	entries, err := os.ReadDir(topicDir)
	if err != nil {
		if os.IsNotExist(err) {
			return count, nil
		}
		return count, fmt.Errorf("memdb: không đọc được %s: %w", topicDir, err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(topicDir, e.Name()))
		if err != nil {
			return count, fmt.Errorf("memdb: không đọc được memory/%s: %w", e.Name(), err)
		}
		topicName := strings.TrimSuffix(e.Name(), ".md")
		if err := db.Write(TopicKey(topicName), string(content)); err != nil {
			return count, err
		}
		count++
	}

	return count, nil
}

// SeedSkillsFromDir nạp ani-memory/skills/ (SKILLS-INDEX.md + mỗi <tên>/SKILL.md) vào DB —
// tách riêng khỏi SeedFromDir vì skills được thêm sau, gate bằng HasSkills() độc lập với
// IsSeeded() để bot đã seed memory core từ trước vẫn tự bổ sung skills ở lần chạy kế tiếp.
func (db *DB) SeedSkillsFromDir(memoryDir string) (int, error) {
	skillsDir := filepath.Join(memoryDir, "skills")

	index, err := os.ReadFile(filepath.Join(skillsDir, "SKILLS-INDEX.md"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("memdb: không đọc được skills/SKILLS-INDEX.md: %w", err)
	}
	if err := db.Write(KeySkillsIndex, string(index)); err != nil {
		return 0, err
	}
	count := 1

	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return count, fmt.Errorf("memdb: không đọc được %s: %w", skillsDir, err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillPath := filepath.Join(skillsDir, e.Name(), "SKILL.md")
		content, err := os.ReadFile(skillPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return count, fmt.Errorf("memdb: không đọc được skills/%s/SKILL.md: %w", e.Name(), err)
		}
		if err := db.Write(SkillKey(e.Name()), string(content)); err != nil {
			return count, err
		}
		count++
	}

	return count, nil
}
