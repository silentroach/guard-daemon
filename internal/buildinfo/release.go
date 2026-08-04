// Package buildinfo содержит идентичность воспроизводимой release-сборки.
package buildinfo

// ReleaseCommit задаётся только release builder через Go linker. Пустое
// значение означает обычную development-сборку из закреплённого source tree.
var ReleaseCommit string

// Version возвращает только безопасную публичную идентичность бинарного файла.
func Version() string {
	if len(ReleaseCommit) != 40 {
		return "development"
	}
	for _, character := range ReleaseCommit {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return "development"
		}
	}
	return ReleaseCommit
}
