// Package migrations embeds the versioned SQL schema of the trail permit
// dispatch platform so a binary can build the database from an empty file.
package migrations

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed *.sql
var files embed.FS

// Migration is one immutable schema step.
type Migration struct {
	Version  int
	Name     string
	FileName string
	SQL      string
	Checksum string
}

// All returns every embedded migration ordered by version. Version numbers must
// be unique and file names must follow the NNNN_name.sql convention.
func All() ([]Migration, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, fmt.Errorf("读取内置迁移目录失败: %w", err)
	}
	migrations := make([]Migration, 0, len(entries))
	seen := make(map[int]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, name, err := parseFileName(entry.Name())
		if err != nil {
			return nil, err
		}
		if previous, exists := seen[version]; exists {
			return nil, fmt.Errorf("迁移版本 %d 重复出现于 %s 和 %s", version, previous, entry.Name())
		}
		seen[version] = entry.Name()
		content, err := files.ReadFile(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("读取迁移 %s 失败: %w", entry.Name(), err)
		}
		sum := sha256.Sum256(content)
		migrations = append(migrations, Migration{
			Version:  version,
			Name:     name,
			FileName: entry.Name(),
			SQL:      string(content),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	if len(migrations) == 0 {
		return nil, fmt.Errorf("未找到任何内置迁移文件")
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	return migrations, nil
}

// LatestVersion returns the highest embedded schema version.
func LatestVersion() (int, error) {
	all, err := All()
	if err != nil {
		return 0, err
	}
	return all[len(all)-1].Version, nil
}

func parseFileName(fileName string) (int, string, error) {
	base := strings.TrimSuffix(fileName, ".sql")
	parts := strings.SplitN(base, "_", 2)
	if len(parts) != 2 || parts[1] == "" {
		return 0, "", fmt.Errorf("迁移文件名 %s 不符合 NNNN_name.sql 约定", fileName)
	}
	version, err := strconv.Atoi(parts[0])
	if err != nil || version <= 0 {
		return 0, "", fmt.Errorf("迁移文件名 %s 的版本号不合法", fileName)
	}
	return version, parts[1], nil
}
